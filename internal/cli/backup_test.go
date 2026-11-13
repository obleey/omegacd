package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Q114: the backup lands in a new owner-only file, with the note on the keys (Q115); an existing file is never
// overwritten, and a failed download leaves no file behind.
func TestBackup(t *testing.T) {
	_, srv := newAPI(t, map[string]any{"GET /api/backup": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("the tar"))
	})})
	file := filepath.Join(t.TempDir(), "omegacd.tar")
	r := run(t, srv, nil, "", "backup", file)
	if r.code != 0 || !strings.Contains(r.stdout, "Master Key") || !strings.Contains(r.stdout, "source.key") {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(file); string(b) != "the tar" {
		t.Fatalf("written %q", b)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if r := run(t, srv, nil, "", "backup", file); r.code == 0 || !strings.Contains(r.stderr, "never overwritten") {
		t.Fatalf("over an existing file: %+v", r)
	}

	_, broken := newAPI(t, map[string]any{"GET /api/backup": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "disk full", http.StatusInternalServerError)
	})})
	failed := filepath.Join(t.TempDir(), "omegacd.tar")
	if r := run(t, broken, nil, "", "backup", failed); r.code == 0 {
		t.Fatalf("a failed backup: %+v", r)
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatal("a failed backup left its file")
	}
	if r := run(t, srv, nil, "", "backup"); r.code == 0 {
		t.Fatal("no file named")
	}
	if r := run(t, srv, map[string]string{"OMEGACD_SERVER": "", "OMEGACD_TOKEN": "", "HOME": t.TempDir()}, "", "backup", filepath.Join(t.TempDir(), "x")); r.code == 0 {
		t.Fatalf("no server to ask: %+v", r)
	}
}
