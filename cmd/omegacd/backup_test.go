package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/store"
)

// failAfter accepts n bytes, then fails: a disk that fills up mid-backup.
type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.n {
		return 0, errors.New("disk full")
	}
	f.n -= len(p)
	return len(p), nil
}

// Q114: the backup is a tar of a consistent copy of the database and the Git credentials file when there is one; it
// restores as a working database. Each way it can't be made is an error.
func TestWriteBackup(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	st.CreateApp(ctx, domain.Application{Name: "web"})
	creds := filepath.Join(dir, "credentials.age")
	os.WriteFile(creds, []byte("sealed"), 0o600)

	var buf bytes.Buffer
	if err := writeBackup(ctx, st, []string{creds, filepath.Join(dir, "none.age")}, &buf); err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	tr := tar.NewReader(&buf)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		b, _ := io.ReadAll(tr)
		os.WriteFile(filepath.Join(restored, h.Name), b, 0o600)
	}
	if len(names) != 2 || names[0] != "omegacd.db" || names[1] != "credentials.age" {
		t.Fatalf("in the tar: %v", names)
	}
	back, err := store.Open(filepath.Join(restored, "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := back.GetApp(ctx, "web"); err != nil {
		t.Fatalf("the restored database: %v", err)
	}
	back.Close()

	unreadable := filepath.Join(dir, "unreadable.age")
	os.WriteFile(unreadable, []byte("x"), 0o000)
	if err := writeBackup(ctx, st, []string{unreadable}, io.Discard); err == nil && os.Geteuid() != 0 {
		t.Error("a credentials file it can't read was left out")
	}
	for _, n := range []int{100, 1000} { // the header, then the database itself
		if err := writeBackup(ctx, st, nil, &failAfter{n}); err == nil {
			t.Errorf("a writer failing after %d bytes", n)
		}
	}
	file := filepath.Join(t.TempDir(), "a-file")
	os.WriteFile(file, nil, 0o600)
	t.Setenv("TMPDIR", file)
	if err := writeBackup(ctx, st, nil, io.Discard); err == nil {
		t.Error("no temporary folder")
	}
	t.Setenv("TMPDIR", t.TempDir())
	st.Close()
	if err := writeBackup(ctx, st, nil, io.Discard); err == nil {
		t.Error("a closed database")
	}
}
