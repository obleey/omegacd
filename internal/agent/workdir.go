package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	"github.com/obleey/omegacd/internal/workdir"
)

// WorkDir is where the Agent writes each Application's files (Q11, Q48): plaintext, on RAM or disk as configured.
type WorkDir struct {
	Root  string   // the Host's work dir: an Application's folder is here unless it names its own root
	Allow []string // further roots an Application may name (OMEGACD_AGENT_WORKDIR_ALLOW, Q57)
	db    *DB
	cache *Cache
	log   *slog.Logger
}

// NewWorkDir is the work dir at root; log nil logs nothing.
func NewWorkDir(root string, db *DB, cache *Cache, log *slog.Logger) *WorkDir {
	return &WorkDir{Root: root, db: db, cache: cache, log: cmp.Or(log, logging.Discard)}
}

// Dir is a's folder: <root>/<name>, or <root>/<parent>/<name> for a Child Application, in the root it names or the
// Host's work dir.
func (w *WorkDir) Dir(a App) string { return filepath.Join(w.root(a), a.Parent, a.Name) }

func (w *WorkDir) root(a App) string { return cmp.Or(a.WorkDir, w.Root) }

// allowed checks the root a names (Q57): under the Host's work dir or a root OMEGACD_AGENT_WORKDIR_ALLOW lists, so
// neither Git nor the Control Plane can aim the Agent at /etc. It creates the root when missing, as the Agent's own.
func (w *WorkDir) allowed(a App) error {
	root := w.root(a)
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("%w: work dir %q of %s: want an absolute, clean path", domain.ErrInvalid, root, a.Name)
	}
	if !workdir.Allowed(root, append([]string{w.Root}, w.Allow...)) {
		return fmt.Errorf("%w: work dir %s of %s is not under a root this Agent allows (its own %s, and "+
			"OMEGACD_AGENT_WORKDIR_ALLOW: %s): add it there, or change the Application's workDir",
			domain.ErrInvalid, root, a.Name, w.Root, cmp.Or(strings.Join(w.Allow, ","), "none"))
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		u, _ := user.Current()
		g, _ := user.LookupGroupId(strconv.Itoa(os.Getgid()))
		var name, group string
		if u != nil {
			name = u.Username
		}
		if g != nil {
			group = g.Name
		}
		return fmt.Errorf("work dir %s of %s: %w; create it once for the Agent's user: sudo install -d -o %s -g %s %s",
			root, a.Name, err, name, group, root)
	}
	return nil
}

// Write makes a's folder hold exactly r's files, from the cache, in place (single-file bind mounts keep their
// inode). The folder must be a's (workdir.Claim): one the Agent wrote for it before, or one holding no one's files.
// Once written, it is recorded as a's, and the folder a used before (another App of Apps) is removed.
func (w *WorkDir) Write(ctx context.Context, a App, r Revision) error {
	if err := w.allowed(a); err != nil {
		return err // before anything is written
	}
	dir := w.Dir(a)
	prev, err := w.db.GetApp(ctx, a.Name)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	owned := prev.Dir == dir
	w.log.DebugContext(ctx, "claim folder", "dir", dir, "owned", owned, "previous", prev.Dir)
	if a.Parent != "" {
		var kids []string
		apps, err := w.db.ListApps(ctx)
		if err != nil {
			return err
		}
		for _, x := range apps {
			if x.Parent == a.Parent {
				kids = append(kids, x.Name)
			}
		}
		if err := workdir.Claim(filepath.Join(w.root(a), a.Parent), a.Parent, owned, append(kids, a.Name)); err != nil {
			return err
		}
	}
	if err := workdir.Claim(dir, a.Name, owned, nil); err != nil {
		return err
	}
	// recorded before writing: a write that stops halfway leaves a folder that is still a's; the deployed Revision
	// stays the previous one until a Deploy succeeds
	a.Dir, a.Revision = dir, prev.Revision
	if err := w.db.SaveApp(ctx, a); err != nil { // coverage: unreachable, a write to the healthy database that just answered
		return err
	}
	if err := workdir.Write(dir, r.Files, w.cache.Blob); err != nil {
		return err
	}
	if prev.Dir != "" && prev.Dir != dir {
		w.log.DebugContext(ctx, "remove the folder it used before", "dir", prev.Dir)
		os.RemoveAll(prev.Dir)
		if filepath.Dir(prev.Dir) != w.root(prev) {
			os.Remove(filepath.Dir(prev.Dir)) // its old App of Apps' folder, once empty; never a root
		}
	}
	return nil
}

// mountinfo lists this process's mounts; a variable for tests.
var mountinfo = "/proc/self/mountinfo"

// Docker and Podman mount a container's hostname and hosts files from a folder named by its ID
// (…/containers/ID/hostname, …/overlay-containers/ID/userdata/hostname).
var containerID = regexp.MustCompile(`/([0-9a-f]{64})/(?:userdata/)?(?:hostname|hosts|resolv\.conf) `)

// self is the Agent's own container: its ID from its mounts, so it is found with host networking too, where the
// hostname is the host's (Q235); else its hostname, which is its ID unless set otherwise.
func self() string {
	if b, err := os.ReadFile(mountinfo); err == nil {
		if m := containerID.FindSubmatch(b); m != nil {
			return string(m[1])
		}
	}
	h, _ := os.Hostname()
	return h
}

// OwnWorkDir gives the work dir root to the Agent's user when it can't write there: Docker made it, owned by root, for
// the container's bind mount whose source was missing (a RAM work dir after a reboot). Only in a container, through
// Docker with the Agent's own image, as reclaim does (Q237).
func OwnWorkDir(ctx context.Context, root string, log *slog.Logger) error {
	if !inContainer() || unix.Access(root, unix.W_OK) == nil {
		return nil
	}
	log.InfoContext(ctx, "the work dir isn't the Agent's user's: giving it to that user through Docker", "workdir", root)
	return helper(ctx, root, "", "own "+root, `chown "$1" "$2"`, owner(), root)
}

// RootlessContainer reports whether the Agent runs in a container of a rootless engine, whose root is the
// unprivileged user on the host (Q240).
func RootlessContainer(ctx context.Context, log *slog.Logger) bool {
	if !inContainer() {
		return false
	}
	e, err := DetectEngine(ctx)
	if err != nil {
		log.DebugContext(ctx, "in a container, its engine unreachable: not taken for rootless", "err", err)
		return false
	}
	return e.Rootless
}

// inContainer reports whether the Agent runs in a container (Docker or Podman).
func inContainer() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// CheckPathParity refuses a containerised Agent whose work dir isn't mounted at the same path on the host (Q21):
// the engine resolves bind mounts on the host, so ./conf.yaml in /work/web must be /work/web there too.
func CheckPathParity(ctx context.Context, workDir string) error {
	if !inContainer() {
		return nil
	}
	c, err := client.New(client.FromEnv)
	if err != nil { // coverage: unreachable, only in a container, and omegacd agent refused a DOCKER_HOST the client can't use before (useDockerHost)
		return err
	}
	defer c.Close()
	me := self()
	insp, err := c.ContainerInspect(ctx, me, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("checking the work dir's path on the host: can't inspect this container (%s): %w", me, err)
	}
	return pathParity(workDir, insp.Container.Mounts)
}

func pathParity(workDir string, mounts []container.MountPoint) error {
	var best *container.MountPoint
	for i, m := range mounts {
		if (workDir == m.Destination || strings.HasPrefix(workDir, m.Destination+"/")) && (best == nil || len(m.Destination) > len(best.Destination)) {
			best = &mounts[i]
		}
	}
	fix := fmt.Sprintf("mount it at the same path on both sides, e.g. -v %s:%s", workDir, workDir)
	switch {
	case best == nil:
		return fmt.Errorf("the work dir %s is not mounted from the host, so Docker can't find the Applications' files: %s", workDir, fix)
	case best.Type != "bind":
		return fmt.Errorf("the work dir %s is a %s mount, not a host folder: %s", workDir, best.Type, fix)
	case best.Source != best.Destination:
		return fmt.Errorf("the work dir %s is %s on the host: Docker resolves bind mounts on the host, so the paths must match: %s",
			workDir, filepath.Join(best.Source, strings.TrimPrefix(workDir, best.Destination)), fix)
	}
	return nil
}
