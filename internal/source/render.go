package source

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/obleey/omegacd/internal/appdef"
	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/secretkey"
	"github.com/obleey/omegacd/internal/workdir"
)

var _ domain.Source = (*Server)(nil)

// Server is the Source Server, in-process: Git (Resolve, Diff, CheckAccess, …) plus rendering.
type Server struct {
	*Git
	creds   *creds
	blobs   *blob.Store
	staging string

	mu          sync.Mutex
	rendered    map[string]*list.Element // of *cached, most recently used first in recent
	recent      *list.List
	maxRendered int
}

type cached struct {
	key string
	r   domain.Rendered
}

// NewServer keeps clones in dir/repos, rendered files in dir/blobs, and the Repository credentials in
// dir/credentials.age, sealed with dir/source.key.
func NewServer(dir string) (*Server, error) {
	blobs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		return nil, err
	}
	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, err
	}
	creds, err := openCreds(dir)
	if err != nil {
		return nil, err
	}
	return &Server{Git: New(filepath.Join(dir, "repos"), creds.all), creds: creds, blobs: blobs, staging: staging,
		rendered: map[string]*list.Element{}, recent: list.New(), maxRendered: 1000}, nil
}

// blobGrace is how long a blob outlives the last render that put it: the Control Plane fetches a render's blobs
// right after rendering, well within it.
const blobGrace = time.Hour

var collectEvery = blobGrace // tests use less

// CollectBlobs removes, every hour until ctx ends, the blobs no cached render uses that weren't put within the
// last hour. They are only a cache: a later Render of their commit puts them again.
func (s *Server) CollectBlobs(ctx context.Context) {
	t := time.NewTicker(collectEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, err := s.collectBlobs(); err != nil {
			s.Log.Warn("collect source server blobs; they are only a cache, the next collection tries again", "err", err)
		} else {
			s.Log.Debug("collected source server blobs", "removed", n)
		}
	}
}

func (s *Server) collectBlobs() (int, error) {
	used := map[string]bool{}
	s.mu.Lock()
	for e := s.recent.Front(); e != nil; e = e.Next() {
		for _, f := range e.Value.(*cached).r.Files {
			used[f.SHA256] = true
		}
	}
	s.mu.Unlock()
	return s.blobs.GC(func(hash string) bool { return used[hash] }, blobGrace)
}

func (s *Server) Blob(_ context.Context, hash string) ([]byte, error) { return s.blobs.Get(hash) }

func (s *Server) Render(ctx context.Context, spec domain.RenderSpec) (domain.Rendered, error) {
	// a commit never changes, so neither does its render
	key := strings.Join([]string{NormalizeURL(spec.RepoURL), spec.Revision, cleanPath(spec.Path),
		strings.Join(spec.ComposeFiles, ","), strings.Join(spec.Profiles, ","), fmt.Sprint(spec.Apps)}, "\x00")
	var r domain.Rendered
	s.mu.Lock()
	e, ok := s.rendered[key]
	if ok {
		s.recent.MoveToFront(e)
		r = e.Value.(*cached).r
	}
	s.mu.Unlock()
	if ok {
		s.Log.DebugContext(ctx, "render cached", "repo", redact(spec.RepoURL), "revision", spec.Revision, "path", spec.Path)
		return r, nil
	}
	tree, err := s.tree(ctx, spec.RepoURL, spec.Revision, spec.Path)
	if err != nil {
		return r, err
	}
	if tree == nil {
		return r, fmt.Errorf("path %q not found at %s", spec.Path, spec.Revision[:min(7, len(spec.Revision))])
	}
	slices.SortFunc(tree, func(a, b treeFile) int { return strings.Compare(a.path, b.path) })
	sum := sha256.New()
	for _, f := range tree {
		if f.path == domain.MarkerFile {
			continue // never taken from Git: a Sync reads it as an old marker
		}
		h, err := s.blobs.Put(f.content)
		if err != nil {
			return r, err
		}
		r.Files = append(r.Files, domain.File{Path: f.path, Mode: f.mode, SHA256: h})
		fmt.Fprintf(sum, "%s\x00%o\x00%s\n", f.path, f.mode, h)
	}
	r.Checksum = hex.EncodeToString(sum.Sum(nil))

	dir, err := os.MkdirTemp(s.staging, "")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(dir)
	switch err := workdir.Write(dir, r.Files, s.blobs.Get); {
	case errors.Is(err, domain.ErrInvalid):
		r.Invalid = err
	case err != nil: // coverage: unreachable, a new staging folder and blobs just put: only a full disk fails here
		return r, err
	case spec.Apps:
		r.Apps, r.Invalid = appdef.Parse(dir)
	default:
		a := domain.Application{Name: "render", ComposeFiles: spec.ComposeFiles, Profiles: spec.Profiles}
		r.Invalid = validate(ctx, s.Log, a, dir)
		r.Warnings = plainSecrets(ctx, s.Log, a, dir)
	}

	s.Log.DebugContext(ctx, "rendered", "repo", redact(spec.RepoURL), "revision", spec.Revision, "path", spec.Path,
		"files", len(r.Files), "tree", r.Checksum, "invalid", r.Invalid, "warnings", r.Warnings)
	s.mu.Lock()
	if _, ok := s.rendered[key]; !ok { // a concurrent Render of the same spec may have been first
		s.rendered[key] = s.recent.PushFront(&cached{key, r})
		if s.recent.Len() > s.maxRendered {
			delete(s.rendered, s.recent.Remove(s.recent.Back()).(*cached).key)
		}
	}
	s.mu.Unlock()
	return r, nil
}

// validate refuses what can't be deployed safely. It sees the files as in Git, without secrets: compose files that
// don't load without them are left to the final render on the Host, which reports their errors (Q138).
func validate(ctx context.Context, log *slog.Logger, a domain.Application, dir string) error {
	files := a.ComposeFilesIn(dir)
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = filepath.Join(dir, f)
	}
	// interpolation sees dir/.env only (a plain one from Git), never omegacd's process env; paths as written, to
	// tell ./data from /data
	opts, err := cli.NewProjectOptions(paths,
		cli.WithWorkingDirectory(dir),
		cli.WithEnvFiles(),
		cli.WithDotEnv,
		cli.WithProfiles(a.Profiles),
		cli.WithName(a.Name),
		cli.WithResolvedPaths(false),
		cli.WithLoadOptions(func(o *loader.Options) { o.SkipResolveEnvironment = true }))
	var p *types.Project
	if err == nil {
		p, err = opts.LoadProject(ctx)
	}
	if err != nil {
		log.DebugContext(ctx, "not validated on the Source Server: the compose files don't load without their secrets; the Host's render checks them",
			"err", err)
		return nil
	}
	if err := checkNoBuild(p); err != nil {
		return err
	}
	return checkBindSources(p, dir)
}

func checkNoBuild(p *types.Project) error {
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		if p.Services[name].Build != nil {
			return fmt.Errorf("%w: service %q uses build:, only pre-built images are supported", domain.ErrInvalid, name)
		}
	}
	return nil
}

// checkBindSources refuses relative bind mounts that are not in Git: every Sync makes the app dir match Git
// exactly, so files a container writes there would be deleted on the next Sync (and on reboot, with the
// work dir in /run). Only named volumes and absolute paths keep data.
func checkBindSources(p *types.Project, dir string) error {
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		for _, v := range p.Services[name].Volumes {
			if v.Type != types.VolumeTypeBind || filepath.IsAbs(v.Source) || strings.HasPrefix(v.Source, "~") {
				continue
			}
			src := filepath.Join(dir, v.Source)
			if rel, err := filepath.Rel(dir, src); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return fmt.Errorf("%w: service %q mounts %s, outside the Application's files; "+
					"use a named volume or an absolute path", domain.ErrInvalid, name, v.Source)
			}
			if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: service %q mounts %s, which is not in Git: OmegaCD deletes files that are not in Git on every Sync, "+
					"so data written there would be lost; use a named volume or an absolute path", domain.ErrInvalid, name, v.Source)
			}
		}
	}
	return nil
}

// plainSecrets warns about environment values in the compose files that look like secrets and are written out
// in Git instead of coming from ${VAR} (a SOPS-encrypted .env) or an env_file. Files are read as written,
// before interpolation; unreadable ones are skipped (the final render reports them, Q138).
func plainSecrets(ctx context.Context, log *slog.Logger, a domain.Application, dir string) []string {
	var warnings []string
	for _, f := range a.ComposeFilesIn(dir) {
		var doc struct {
			Services map[string]struct {
				Environment yaml.Node `yaml:"environment"`
			} `yaml:"services"`
		}
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err == nil {
			err = yaml.Unmarshal(b, &doc)
		}
		if err != nil {
			log.DebugContext(ctx, "not checked for plain-text secrets: the Host's render reports the file", "file", f, "err", err)
			continue
		}
		for _, svc := range slices.Sorted(maps.Keys(doc.Services)) {
			env := doc.Services[svc].Environment
			var pairs [][2]string
			switch env.Kind {
			case yaml.MappingNode:
				for i := 0; i+1 < len(env.Content); i += 2 {
					pairs = append(pairs, [2]string{env.Content[i].Value, env.Content[i+1].Value})
				}
			case yaml.SequenceNode:
				for _, n := range env.Content {
					k, v, _ := strings.Cut(n.Value, "=")
					pairs = append(pairs, [2]string{k, v})
				}
			}
			for _, kv := range pairs {
				k, v := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
				if secretkey.Plain(k, v) {
					warnings = append(warnings, fmt.Sprintf("%s: %s is in plain text in %s, readable by anyone with access to the repository; "+
						"put it in a SOPS-encrypted .env and write %s: ${%s}", svc, k, f, k, k))
				}
			}
		}
	}
	return warnings
}
