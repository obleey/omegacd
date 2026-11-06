// Package workdir writes an Application's files, a rendered Desired State, into its folder.
package workdir

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/obleey/omegacd/internal/domain"
)

// Write makes dest hold exactly files, reading each one's content with blob: files are overwritten in place (same
// inode, for single-file bind mounts) and everything else is deleted, except what decrypting writes next to an
// Encrypted Secret ("x.sops.ext" → "x.ext").
func Write(dest string, files []domain.File, blob func(hash string) ([]byte, error)) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, f := range files {
		keep[f.Path] = true
		if base := path.Base(f.Path); strings.Contains(base, ".sops.") {
			keep[path.Join(path.Dir(f.Path), strings.Replace(base, ".sops.", ".", 1))] = true
		}
		content, err := blob(f.SHA256)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		if err := writeEntry(dest, f, content); err != nil {
			return err
		}
	}
	return prune(dest, keep)
}

func writeEntry(dest string, f domain.File, content []byte) error {
	if path.IsAbs(f.Path) || !filepath.IsLocal(filepath.FromSlash(f.Path)) {
		return fmt.Errorf("file %s is outside the app path: %w", f.Path, domain.ErrInvalid)
	}
	target := filepath.Join(dest, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	fi, statErr := os.Lstat(target)
	if f.Mode&fs.ModeSymlink != 0 {
		link := string(content)
		if path.IsAbs(link) || strings.HasPrefix(path.Join(path.Dir(f.Path), link), "..") {
			return fmt.Errorf("symlink %s -> %s points outside the app path: %w", f.Path, link, domain.ErrInvalid)
		}
		if statErr == nil {
			if cur, _ := os.Readlink(target); fi.Mode()&fs.ModeSymlink != 0 && cur == link {
				return nil
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
		return os.Symlink(link, target)
	}
	perm := f.Mode.Perm()
	if statErr == nil && !fi.Mode().IsRegular() {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	} else if statErr == nil {
		if cur, err := os.ReadFile(target); err == nil && bytes.Equal(cur, content) {
			if fi.Mode().Perm() == perm {
				return nil
			}
			return os.Chmod(target, perm)
		}
	}
	// Truncate in place instead of rename so the inode stays the same for Docker bind mounts.
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = file.Write(content)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Chmod(target, perm)
}

// prune deletes files under dest not in keep, then empty directories.
func prune(dest string, keep map[string]bool) error {
	var dirs []string
	err := filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dest, p)
		if d.IsDir() {
			if rel != "." {
				dirs = append(dirs, p)
			}
			return nil
		}
		if !keep[filepath.ToSlash(rel)] {
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Reverse(dirs)
	for _, d := range dirs {
		_ = os.Remove(d) // fails while non-empty, which is what we want
	}
	return nil
}

// Claim creates dir if needed when it may be written for Application name, and refuses a folder someone else
// owns (Q48): dir may be written if it it carries name's marker from before Dirs (deleted
// now), or no marker and it is owned (the caller knows it wrote there or runs containers from it), holds nothing but directories (Docker re-creates missing bind-mount sources as
// empty directories, which hold no one's data), or holds nothing but folders named in kids.
func Claim(dir, name string, owned bool, kids []string) error {
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
