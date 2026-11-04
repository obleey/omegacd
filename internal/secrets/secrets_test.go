package secrets

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"filippo.io/age"
)

func TestDecryptDir(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops not on PATH")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	keyFile := filepath.Join(tmp, "age.key")
	if err := os.WriteFile(keyFile, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "app")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	encrypt := func(plain, src, dst string) {
		t.Helper()
		p := filepath.Join(tmp, src)
		if err := os.WriteFile(p, []byte(plain), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sops", "--encrypt", "--age", id.Recipient().String(), p)
		cmd.Env = append(os.Environ(), "SOPS_AGE_KEY_FILE="+keyFile)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("sops encrypt %s: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	encrypt("DB_PASSWORD=s3cret\n", "db.env", "db.sops.env")
	encrypt("api:\n  key: abc123\n", "config.yaml", filepath.Join("sub", "config.sops.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// pre-existing output must keep its inode
	envOut := filepath.Join(dir, "db.env")
	if err := os.WriteFile(envOut, []byte("old content that is longer than the new one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := inode(t, envOut)

	d, err := New(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	written, err := d.DecryptDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(written)
	want := []string{envOut, filepath.Join(dir, "sub", "config.yaml")}
	slices.Sort(want)
	if !slices.Equal(written, want) {
		t.Fatalf("written %v, want %v", written, want)
	}

	check := func(path, contains string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), contains) || strings.Contains(string(b), "ENC[") {
			t.Fatalf("%s: %q", path, b)
		}
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", path, st.Mode().Perm())
		}
	}
	check(envOut, "DB_PASSWORD=s3cret")
	check(want[1], "key: abc123")
	if inode(t, envOut) != before {
		t.Fatal("inode changed")
	}
	if b, _ := os.ReadFile(envOut); strings.Contains(string(b), "old content") {
		t.Fatal("not truncated")
	}

	if _, err := New(filepath.Join(tmp, "missing")); err == nil {
		t.Fatal("missing key file should fail")
	}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Ino
}
