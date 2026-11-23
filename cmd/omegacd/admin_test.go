package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/api"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/store"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

// reset-admin-password on the server's machine (Q311): a new password in the data dir's file, to be changed; it needs
// the server's Master Key and database, and makes neither.
func TestResetAdminPassword(t *testing.T) {
	dir := t.TempDir()
	data, key := filepath.Join(dir, "data"), filepath.Join(dir, "age.key")
	t.Setenv("OMEGACD_DATA", data)
	t.Setenv("OMEGACD_AGE_KEY_FILE", key)
	var out strings.Builder
	if err := resetAdminPassword(t.Context(), &out); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Fatalf("no Master Key: %v", err)
	}
	if _, err := os.Stat(key); err == nil {
		t.Fatal("made a Master Key")
	}
	id, _, err := crypto.LoadOrCreateIdentity(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := resetAdminPassword(t.Context(), &out); err == nil || !strings.Contains(err.Error(), "no OmegaCD database") {
		t.Fatalf("no database: %v", err)
	}
	os.MkdirAll(data, 0o750)
	st, err := store.Open(filepath.Join(data, "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetAdminPassword(t.Context(), "old", false); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(data, api.InitialPasswordFile)
	if err := resetAdminPassword(t.Context(), &out); err != nil || !strings.Contains(out.String(), "A new admin password is in "+file) {
		t.Fatalf("%v %q", err, out.String())
	}
	if hash, mustChange, _ := st.AdminPasswordHash(t.Context()); hash == "old" || !mustChange {
		t.Fatalf("hash %q, must change %v", hash, mustChange)
	}
	if b, _ := os.ReadFile(file); strings.Contains(out.String(), strings.TrimSpace(string(b))) {
		t.Error("the password printed")
	}
	geteuid = func() int { return 0 }
	err = resetAdminPassword(t.Context(), &out)
	geteuid = os.Geteuid
	if os.Getuid() != 0 && (err == nil || !strings.Contains(err.Error(), "sudo omegacd-reset-admin-password")) {
		t.Errorf("root over another user's database: %v", err)
	}
	if err := resetAdminPassword(t.Context(), failingWriter{}); err == nil {
		t.Error("stdout gone")
	}
	if os.Getuid() != 0 { // root writes whatever the mode
		os.Chmod(file, 0o400)
		if err := resetAdminPassword(t.Context(), &out); err == nil || !strings.Contains(err.Error(), "write the initial admin password") {
			t.Errorf("a file it can't write: %v", err)
		}
	}
	os.Remove(filepath.Join(data, "omegacd.db"))
	os.Mkdir(filepath.Join(data, "omegacd.db"), 0o750)
	if err := resetAdminPassword(t.Context(), &out); err == nil {
		t.Error("a database that is a folder")
	}
}
