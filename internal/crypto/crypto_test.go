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
