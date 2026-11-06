package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/obleey/omegacd/internal/domain"
)

// appDir is where a's files live in the local work dir: <work dir>/<name>, or <work dir>/<parent>/<name> for a
// Child Application. Applications on remote Hosts keep a copy here too.
func (c *Controller) appDir(a domain.Application) string {
	return filepath.Join(c.cfg.WorkDir, filepath.FromSlash(a.Dir()))
}

// liveDir is where a's deployed files are: its folder or, after the upgrade from the old layout and until a's
// next Sync moves it, <work dir>/apps/<name>.
func (c *Controller) liveDir(a domain.Application) string {
	dir := c.appDir(a)
	if old := filepath.Join(c.cfg.WorkDir, domain.LegacyDir, a.Name); !composeFilesPresent(dir, a.ComposeFilesIn(dir)) &&
		composeFilesPresent(old, a.ComposeFilesIn(old)) {
		return old
	}
	return dir
}

// claim makes sure a Sync of a writes only into folders that are a's: its own and, for a Child Application, its App
// of Apps' folder. Every Sync makes a's folder match Git and deletes other files, so a folder that already holds
// someone else's files (say /opt/services/alloy, host-only data) is refused, never synced into. The folder is a's if
// a Sync wrote to it (a.Dirs) or a container OmegaCD created for a runs from it; the App of Apps' folder also when it
// holds nothing but folders of kids, its children. It records a's folder in a.Dirs before the files are written.
// The file helper's claim() in internal/engine/remote.go does the same on remote Hosts.
func (c *Controller) claim(ctx context.Context, a *domain.Application, kids []string) error {
	dir, legacy := c.appDir(*a), filepath.Join(c.cfg.WorkDir, domain.LegacyDir)
	owned := slices.Contains(a.Dirs, domain.HostDir(domain.LocalHost, dir))
	if !owned && cmp.Or(a.Host, domain.LocalHost) == domain.LocalHost {
		wds, _ := c.cfg.Engine.WorkingDirs(ctx, *a) // unknown: the other rules still apply
		owned = slices.Contains(wds, dir)
	}
	c.claimMu.Lock()
	defer c.claimMu.Unlock()
	if a.Parent != "" {
		if err := claimDir(filepath.Join(c.cfg.WorkDir, a.Parent), a.Parent, owned || a.Parent == domain.LegacyDir, kids); err != nil {
			return err
		}
	}
	if err := claimDir(dir, a.Name, owned || dir == filepath.Join(legacy, a.Name), nil); err != nil {
		return err
	}
	return c.addDir(ctx, a, domain.HostDir(domain.LocalHost, dir))
}

// addDir adds dir (a HostDir) to a's Dirs.
func (c *Controller) addDir(ctx context.Context, a *domain.Application, dir string) error {
	if slices.Contains(a.Dirs, dir) {
		return nil
	}
	a.Dirs = append(slices.Clone(a.Dirs), dir)
	return c.cfg.Store.SetAppDirs(ctx, a.Name, a.Dirs)
}

// claimDir creates dir if needed when it may be written for name: it carries name's marker from before Dirs (deleted
// now), or no marker and it is owned, holds nothing but directories (Docker re-creates missing bind-mount sources as
// empty directories, which hold no one's data), or holds nothing but folders named in kids.
func claimDir(dir, name string, owned bool, kids []string) error {
	marker := filepath.Join(dir, domain.MarkerFile)
	b, err := os.ReadFile(marker)
	if err == nil && string(b) == name || err != nil && (owned || !hasFiles(dir) || onlyDirs(dir, kids)) {
		if err == nil {
			os.Remove(marker)
		}
		return os.MkdirAll(dir, 0o750)
	}
	return fmt.Errorf("refusing to sync into %s: the folder exists and OmegaCD did not create it for Application %q; "+
		"rename the Application, pick another work dir, or move the folder away", dir, name)
}

// hasFiles reports whether dir holds anything but directories; a missing dir holds nothing, an unreadable one may.
func hasFiles(dir string) bool {
	errFound := errors.New("found")
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			return errFound
		}
		return err
	})
	return err != nil && !errors.Is(err, fs.ErrNotExist)
}

// onlyDirs reports whether dir holds nothing but folders named in names.
func onlyDirs(dir string, names []string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil || len(names) == 0 {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() || !slices.Contains(names, e.Name()) {
			return false
		}
	}
	return true
}

// removeFiles deletes a's folders in the work dir other than keep ("" deletes them all): those in a.Dirs or marked
// for a before Dirs (its current one, one left from before its App of Apps changed) and its folder from the old
// layout, then an App of Apps' folder once it is empty. Nothing else is touched.
func (c *Controller) removeFiles(a domain.Application, keep string) error {
	legacy := filepath.Join(c.cfg.WorkDir, domain.LegacyDir, a.Name)
	mine := func(dir string) bool {
		if b, err := os.ReadFile(filepath.Join(dir, domain.MarkerFile)); err == nil {
			return string(b) == a.Name
		}
		return (dir == legacy || slices.Contains(a.Dirs, domain.HostDir(domain.LocalHost, dir))) && dirExists(dir)
	}
	dirs, _ := filepath.Glob(filepath.Join(c.cfg.WorkDir, "*", a.Name))
	var errs []error
	for _, dir := range append(dirs, filepath.Join(c.cfg.WorkDir, a.Name)) {
		if dir == keep || !mine(dir) {
			continue
		}
		errs = append(errs, os.RemoveAll(dir))
		if dir == legacy {
			// the old layout kept the decrypted list beside the app; its folder goes once empty
			os.Remove(filepath.Join(filepath.Dir(legacy), "."+a.Name+".decrypted"))
			os.Remove(filepath.Dir(legacy))
		}
	}
	if a.Kind == domain.KindApps && keep == "" {
		os.Remove(c.appDir(a)) // only if empty: its children's folders are theirs
	}
	return errors.Join(errs...)
}

// stage exports a's path at sha into a new folder named like the app under the work dir's .staging (Application
// names never start with a dot), for reading without touching the app's files. done removes it.
func (c *Controller) stage(ctx context.Context, a domain.Application, sha string) (dir string, done func(), err error) {
	root := filepath.Join(c.cfg.WorkDir, ".staging")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", nil, err
	}
	tmp, err := os.MkdirTemp(root, "")
	if err != nil {
		return "", nil, err
	}
	done = func() { os.RemoveAll(tmp) }
	dir = filepath.Join(tmp, a.Name)
	if err := os.Mkdir(dir, 0o750); err != nil {
		done()
		return "", nil, err
	}
	if err := c.cfg.Git.Export(ctx, a.RepoURL, sha, a.Path, dir); err != nil {
		done()
		return "", nil, err
	}
	return dir, done, nil
}
