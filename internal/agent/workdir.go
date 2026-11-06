package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/workdir"
)

// WorkDir is where the Agent writes each Application's files (Q11, Q48): plaintext, on RAM or disk as configured.
type WorkDir struct {
	Root  string
	db    *DB
	cache *Cache
}

func NewWorkDir(root string, db *DB, cache *Cache) *WorkDir {
	return &WorkDir{Root: root, db: db, cache: cache}
}

// Dir is a's folder: <root>/<name>, or <root>/<parent>/<name> for a Child Application.
func (w *WorkDir) Dir(a App) string { return filepath.Join(w.Root, a.Parent, a.Name) }

// Write makes a's folder hold exactly r's files, from the cache, in place (single-file bind mounts keep their
// inode). The folder must be a's (workdir.Claim): one the Agent wrote for it before, or one holding no one's files.
// Once written, it is recorded as a's, and the folder a used before (another App of Apps) is removed.
func (w *WorkDir) Write(ctx context.Context, a App, r Revision) error {
	dir := w.Dir(a)
	prev, err := w.db.GetApp(ctx, a.Name)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	owned := prev.Dir == dir
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
		if err := workdir.Claim(filepath.Join(w.Root, a.Parent), a.Parent, owned, append(kids, a.Name)); err != nil {
			return err
		}
	}
	if err := workdir.Claim(dir, a.Name, owned, nil); err != nil {
		return err
	}
	// recorded before writing: a write that stops halfway leaves a folder that is still a's; the deployed Revision
	// stays the previous one until a Deploy succeeds
	a.Dir, a.Revision = dir, prev.Revision
	if err := w.db.SaveApp(ctx, a); err != nil {
		return err
	}
	if err := workdir.Write(dir, r.Files, w.cache.Blob); err != nil {
		return err
	}
	if prev.Dir != "" && prev.Dir != dir {
		os.RemoveAll(prev.Dir)
		if filepath.Dir(prev.Dir) != w.Root {
			os.Remove(filepath.Dir(prev.Dir)) // its old App of Apps' folder, once empty
		}
	}
	return nil
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
	if err != nil {
		return err
	}
	defer c.Close()
	self, _ := os.Hostname() // a container's hostname is its ID unless set otherwise
	insp, err := c.ContainerInspect(ctx, self, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("checking the work dir's path on the host: can't inspect this container (%s): %w; "+
			"keep the container's hostname its ID", self, err)
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
