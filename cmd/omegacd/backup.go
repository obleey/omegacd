package main

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/obleey/omegacd/internal/store"
)

// writeBackup writes the backup tar (Q114) to w: a consistent copy of the database as omegacd.db, then each of files
// that exists (the in-process Source Server's encrypted credentials.age), by its base name. No private key is in it.
func writeBackup(ctx context.Context, st *store.Store, files []string, w io.Writer) error {
	dir, err := os.MkdirTemp("", "omegacd-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	db := filepath.Join(dir, "omegacd.db")
	if err := st.Backup(ctx, db); err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	for _, f := range append([]string{db}, files...) {
		if err := addFile(tw, f); errors.Is(err, fs.ErrNotExist) && f != db {
			continue // no Git credentials saved yet
		} else if err != nil {
			return err
		}
	}
	return tw.Close()
}

func addFile(tw *tar.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil { // coverage: unreachable, Stat of a file just opened
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: filepath.Base(path), Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
