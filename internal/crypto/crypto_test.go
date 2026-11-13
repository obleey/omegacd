package crypto

import (
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestLoadIdentityAndSeal(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "age.key")
	key := "# created: now\n# public key: " + id.Recipient().String() + "\n" + id.String() + "\n"
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.String() != id.String() {
		t.Fatal("identity mismatch")
	}

	s := NewSealer(loaded)
	sealed, err := s.Seal("hunter2")
	if err != nil || sealed == "" || sealed == "hunter2" {
		t.Fatalf("seal: %q %v", sealed, err)
	}
	got, err := s.Open(sealed)
	if err != nil || got != "hunter2" {
		t.Fatalf("open: %q %v", got, err)
	}
	if e, _ := s.Seal(""); e != "" {
		t.Fatal("empty should seal to empty")
	}
	if e, _ := s.Open(""); e != "" {
		t.Fatal("empty should open to empty")
	}

	other, _ := age.GenerateX25519Identity()
	if _, err := NewSealer(other).Open(sealed); err == nil {
		t.Fatal("open with wrong key should fail")
	}
	if _, err := LoadIdentity(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file should fail")
	}
}

// Q115: a missing key is made, owner-only in a folder made for it; the next call reads the same one; what isn't a key
// file, or a folder where none can be made, is an error.
func TestLoadOrCreateIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "omegacd", "age.key")
	id, created, err := LoadOrCreateIdentity(path)
	if err != nil || !created {
		t.Fatalf("created %v, %v", created, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	again, created, err := LoadOrCreateIdentity(path)
	if err != nil || created || again.String() != id.String() {
		t.Fatalf("again: %v %v", created, err)
	}
	bad := filepath.Join(t.TempDir(), "age.key")
	os.WriteFile(bad, []byte("not a key"), 0o600)
	if _, _, err := LoadOrCreateIdentity(bad); err == nil {
		t.Fatal("a file that isn't a key was taken")
	}
	file := filepath.Join(t.TempDir(), "a-file")
	os.WriteFile(file, nil, 0o600)
	if _, _, err := LoadOrCreateIdentity(filepath.Join(file, "age.key")); err == nil {
		t.Fatal("made a key under a file")
	}
	if os.Geteuid() == 0 {
		return // root writes into read-only folders
	}
	ro := t.TempDir()
	os.Chmod(ro, 0o500)
	defer os.Chmod(ro, 0o700)
	for _, p := range []string{filepath.Join(ro, "age.key"), filepath.Join(ro, "sub", "age.key")} {
		if _, _, err := LoadOrCreateIdentity(p); err == nil {
			t.Fatalf("made %s in a read-only folder", p)
		}
	}
}
