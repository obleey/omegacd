package workdir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
)

// write writes path → content into dest with Write; "*.sh" is executable, "@target" a symlink.
func write(dest string, m map[string]string) error {
	blobs := map[string][]byte{}
	var files []domain.File
	for p, content := range m {
		mode := fs.FileMode(0o644)
		if filepath.Ext(p) == ".sh" {
			mode = 0o755
		}
		if content != "" && content[0] == '@' {
			mode, content = fs.ModeSymlink|0o777, content[1:]
		}
		sum := sha256.Sum256([]byte(content))
		h := hex.EncodeToString(sum[:])
		blobs[h] = []byte(content)
		files = append(files, domain.File{Path: p, Mode: mode, SHA256: h})
	}
	return Write(dest, files, func(h string) ([]byte, error) { return blobs[h], nil })
}

func TestWrite(t *testing.T) {
	dest := t.TempDir()
	os.WriteFile(filepath.Join(dest, "stale.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dest, "olddir"), 0o755)
	os.WriteFile(filepath.Join(dest, "olddir", "f"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dest, "db.env"), []byte("decrypted"), 0o600) // secrets output
	os.WriteFile(filepath.Join(dest, domain.MarkerFile), []byte("web"), 0o644)
	if err := write(dest, map[string]string{"compose.yaml": "services: {}\n", "run.sh": "#!/bin/sh\n", "db.sops.env": "enc\n", "link": "@compose.yaml"}); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(dest, "compose.yaml")
	before, err := os.Stat(compose)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dest, "run.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode %v", fi.Mode())
	}
	if l, _ := os.Readlink(filepath.Join(dest, "link")); l != "compose.yaml" {
		t.Errorf("link -> %q", l)
	}
	for _, gone := range []string{"stale.txt", "olddir", domain.MarkerFile} {
		if _, err := os.Stat(filepath.Join(dest, gone)); !os.IsNotExist(err) {
			t.Errorf("%s not deleted", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "db.env")); err != nil {
		t.Error("decrypted secret output was deleted")
	}

	if err := write(dest, map[string]string{"compose.yaml": "services: {web: {}}\n"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(compose)
	if !os.SameFile(before, after) {
		t.Error("compose.yaml inode changed")
	}
	if b, _ := os.ReadFile(compose); string(b) != "services: {web: {}}\n" {
		t.Errorf("content %q", b)
	}
}

func TestWriteRefusesEscapes(t *testing.T) {
	for _, m := range []map[string]string{
		{"evil": "@../../etc/passwd"},
		{"../evil": "x"},
		{"/etc/evil": "x"},
	} {
		if err := write(t.TempDir(), m); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%v: want ErrInvalid, got %v", m, err)
		}
	}
}

func TestClaim(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, files map[string]string) string {
		dir := filepath.Join(root, name)
		os.MkdirAll(dir, 0o750)
		for f, content := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o750)
			os.WriteFile(filepath.Join(dir, f), []byte(content), 0o644)
		}
		return dir
	}
	kids := []string{"web", "api"}
	for _, tt := range []struct {
		name  string
		dir   string
		owned bool
		ok    bool
	}{
		{"missing", filepath.Join(root, "missing", "web"), false, true},
		{"empty", mk("empty", nil), false, true},
		{"a file deep down", mk("dirs", map[string]string{"a/b/.keep": ""}), false, false}, // even an empty one
		{"owned", mk("owned", map[string]string{"compose.yaml": "x"}), true, true},
		{"marked for web", mk("mine", map[string]string{domain.MarkerFile: "web", "compose.yaml": "x"}), false, true},
		{"marked for another app", mk("other", map[string]string{domain.MarkerFile: "api"}), true, false},
		{"foreign files", mk("foreign", map[string]string{"data": "x"}), false, false},
		{"children's folders", mk("parent", map[string]string{"web/compose.yaml": "x", "api/compose.yaml": "x"}), false, true},
		{"children's folders and more", mk("mixed", map[string]string{"api/compose.yaml": "x", "notes": "x"}), false, false},
		{"another app's folder", mk("stranger", map[string]string{"db/data": "x"}), false, false},
	} {
		err := Claim(tt.dir, "web", tt.owned, kids)
		if (err == nil) != tt.ok {
			t.Errorf("%s: %v", tt.name, err)
		}
		if _, merr := os.Stat(filepath.Join(tt.dir, domain.MarkerFile)); tt.ok && !errors.Is(merr, fs.ErrNotExist) {
			t.Errorf("%s: marker left: %v", tt.name, merr)
		}
	}
	os.RemoveAll(filepath.Join(root, "dirs", "a", "b", ".keep"))
	if err := Claim(filepath.Join(root, "dirs"), "web", false, nil); err != nil {
		t.Errorf("only empty directories: %v", err)
	}
}
