package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

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
	blobs   *blob.Store
	staging string
	mu      sync.Mutex
	// ponytail: renders are kept until there are maxRendered, then all dropped; an LRU if that ever thrashes
	rendered map[string]domain.Rendered
}

const maxRendered = 1000

// NewServer keeps clones in dir/repos and rendered files in dir/blobs.
func NewServer(dir string, creds func(ctx context.Context) ([]domain.RepoCredential, error)) (*Server, error) {
	// ponytail: blobs are never collected; GC them by the Revisions in use once the Source Server runs standalone (M1-05)
	blobs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		return nil, err
	}
	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, err
	}
	return &Server{Git: New(filepath.Join(dir, "repos"), creds), blobs: blobs, staging: staging, rendered: map[string]domain.Rendered{}}, nil
}

func (s *Server) Blob(_ context.Context, hash string) ([]byte, error) { return s.blobs.Get(hash) }

func (s *Server) Render(ctx context.Context, spec domain.RenderSpec) (domain.Rendered, error) {
	// a commit never changes, so neither does its render
	key := strings.Join([]string{NormalizeURL(spec.RepoURL), spec.Revision, cleanPath(spec.Path),
		strings.Join(spec.ComposeFiles, ","), strings.Join(spec.Profiles, ","), fmt.Sprint(spec.Apps)}, "\x00")
	s.mu.Lock()
	r, ok := s.rendered[key]
	s.mu.Unlock()
	if ok {
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
	case err != nil:
		return r, err
	case spec.Apps:
		r.Apps, r.Invalid = appdef.Parse(dir)
	default:
		a := domain.Application{Name: "render", ComposeFiles: spec.ComposeFiles, Profiles: spec.Profiles}
		r.Invalid = validate(ctx, a, dir)
		r.Warnings = plainSecrets(a, dir)
	}

	s.mu.Lock()
	if len(s.rendered) >= maxRendered {
		clear(s.rendered)
	}
	s.rendered[key] = r
	s.mu.Unlock()
	return r, nil
}

// validate refuses what can't be deployed safely. It sees the files as in Git, without secrets: compose files that
// don't load without them are left to the final render on the Host, which reports their errors.
func validate(ctx context.Context, a domain.Application, dir string) error {
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
	if err != nil {
		return nil
	}
	p, err := opts.LoadProject(ctx)
	if err != nil {
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
// before interpolation; unreadable ones are skipped (the final render reports them).
func plainSecrets(a domain.Application, dir string) []string {
	var warnings []string
	for _, f := range a.ComposeFilesIn(dir) {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		var doc struct {
			Services map[string]struct {
				Environment yaml.Node `yaml:"environment"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal(b, &doc) != nil {
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
