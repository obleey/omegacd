package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/obleey/omegacd/internal/domain"
)

// appDir is where a's files live in the local work dir: <work dir>/<name>, or <work dir>/<parent>/<name> for a
// Child Application. Applications on remote Hosts keep a copy here too.
func (c *Controller) appDir(a domain.Application) string {
	return filepath.Join(c.cfg.WorkDir, filepath.FromSlash(a.Dir()))
}

// claim makes sure a Sync of a writes only into folders OmegaCD made for it: its own and, for a Child Application,
// its App of Apps' folder. Every Sync makes a's folder match Git and deletes other files, so a folder that already
// holds someone else's files (say /opt/services/alloy, host-only data) is refused, never synced into.
func (c *Controller) claim(a domain.Application) error {
	legacy := filepath.Join(c.cfg.WorkDir, domain.LegacyDir)
	if a.Parent != "" {
		if err := claimDir(filepath.Join(c.cfg.WorkDir, a.Parent), a.Parent, legacy); err != nil {
			return err
		}
	}
	return claimDir(c.appDir(a), a.Name, filepath.Join(legacy, a.Name))
}

// claimDir creates dir if needed and marks it as name's. It refuses a dir marked for another Application, and an
// unmarked one holding anything but directories (Docker re-creates missing bind-mount sources as empty directories,
// which hold no one's data) unless it is legacy: name's own folder from the old layout.
// The file helper's claim() in internal/engine/remote.go does the same on remote Hosts.
func claimDir(dir, name, legacy string) error {
	marker := filepath.Join(dir, domain.MarkerFile)
	b, err := os.ReadFile(marker)
	switch {
	case err == nil && string(b) == name:
		return nil
	case err != nil && (dir == legacy || !hasFiles(dir)):
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		return os.WriteFile(marker, []byte(name), 0o644)
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

// removeFiles deletes a's folders in the work dir other than keep ("" deletes them all): those marked for a (its
// current one, one left from before its App of Apps was deleted) and its unmarked one from the old layout.
// Nothing without a's marker is touched, the old layout's own folder excepted.
func (c *Controller) removeFiles(a domain.Application, keep string) error {
	legacy := filepath.Join(c.cfg.WorkDir, domain.LegacyDir, a.Name)
	mine := func(dir string) bool {
		if b, err := os.ReadFile(filepath.Join(dir, domain.MarkerFile)); err == nil {
			return string(b) == a.Name
		}
		return dir == legacy && dirExists(dir)
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
