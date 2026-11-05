package secrets

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/domain"
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

	written, err := Decryptor{}.DecryptDir(dir, []string{id.String()})
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
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Ino
}

// sopsEncrypt encrypts plain for the recipients with the sops CLI and writes it to dir/name.
func sopsEncrypt(t *testing.T, dir, name, plain string, recipients ...string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), strings.Replace(name, ".sops.", ".", 1))
	if err := os.WriteFile(src, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sops", "--encrypt", "--age", strings.Join(recipients, ","), src)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sops encrypt %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newID(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDecryptDirUsesOnlyGivenIdentities(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops not on PATH")
	}
	// a key in the environment must never be used
	stray := newID(t)
	t.Setenv("SOPS_AGE_KEY", stray.String())
	a, b := newID(t), newID(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	sopsEncrypt(t, dirA, "a.sops.env", "WHO=a\n", a.Recipient().String())
	sopsEncrypt(t, dirB, "b.sops.env", "WHO=b\n", b.Recipient().String(), stray.Recipient().String())

	if _, err := (Decryptor{}).DecryptDir(dirA, []string{b.String()}); err == nil {
		t.Fatal("a's file decrypted with b's key")
	} else {
		var wk *domain.WrongKeyError
		if !errors.As(err, &wk) || wk.File != "a.sops.env" || !slices.Equal(wk.Recipients, []string{a.Recipient().String()}) {
			t.Fatalf("%v", err)
		}
		if want := "a.sops.env is encrypted for " + a.Recipient().String(); err.Error() != want {
			t.Fatalf("%q, want %q", err, want)
		}
	}
	if _, err := (Decryptor{}).DecryptDir(dirB, nil); err == nil {
		t.Fatal("decrypted without identities (environment key used?)")
	}
	// previous keys are tried after the current one
	if _, err := (Decryptor{}).DecryptDir(dirA, []string{b.String(), a.String()}); err != nil {
		t.Fatal(err)
	}

	// concurrent calls with different keys don't mix them
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for range 10 {
		wg.Go(func() { _, err := (Decryptor{}).DecryptDir(dirA, []string{a.String()}); errs <- err })
		wg.Go(func() { _, err := (Decryptor{}).DecryptDir(dirB, []string{b.String()}); errs <- err })
		wg.Go(func() {
			if _, err := (Decryptor{}).DecryptDir(dirA, []string{b.String()}); err == nil {
				errs <- errors.New("a decrypted with b")
			}
		})
		wg.Go(func() {
			if _, err := (Decryptor{}).DecryptDir(dirB, []string{a.String()}); err == nil {
				errs <- errors.New("b decrypted with a")
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for dir, who := range map[string]string{dirA: "a", dirB: "b"} {
		got, _ := os.ReadFile(filepath.Join(dir, who+".env"))
		if !strings.Contains(string(got), "WHO="+who) {
			t.Fatalf("%s: %q", dir, got)
		}
	}
}
