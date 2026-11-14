package secrets

import (
	"errors"
	"fmt"
	"io/fs"
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
		skipUnlessRequired(t, "sops not on PATH")
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
	// the encrypted copies are gone once decrypted; other files stay
	for _, enc := range []string{"db.sops.env", filepath.Join("sub", "config.sops.yaml")} {
		if _, err := os.Stat(filepath.Join(dir, enc)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s still there: %v", enc, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
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
		skipUnlessRequired(t, "sops not on PATH")
	}
	// a key in the environment must never be used
	stray := newID(t)
	t.Setenv("SOPS_AGE_KEY", stray.String())
	a, b := newID(t), newID(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	sopsEncrypt(t, dirA, "a.sops.env", "WHO=a\n", a.Recipient().String())
	sopsEncrypt(t, dirB, "b.sops.env", "WHO=b\n", b.Recipient().String(), stray.Recipient().String())
	// DecryptDir deletes the encrypted copy (a Sync writes it from Git again each time): every call gets a fresh dir
	encA, _ := os.ReadFile(filepath.Join(dirA, "a.sops.env"))
	encB, _ := os.ReadFile(filepath.Join(dirB, "b.sops.env"))
	fresh := func(enc []byte, name string) string {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, name), enc, 0o644); err != nil {
			t.Error(err)
		}
		return d
	}
	freshA := func() string { return fresh(encA, "a.sops.env") }
	freshB := func() string { return fresh(encB, "b.sops.env") }

	if _, err := (Decryptor{}).DecryptDir(freshA(), []string{b.String()}); err == nil {
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
	if _, err := (Decryptor{}).DecryptDir(freshB(), nil); err == nil {
		t.Fatal("decrypted without identities (environment key used?)")
	}
	// previous keys are tried after the current one
	if _, err := (Decryptor{}).DecryptDir(freshA(), []string{b.String(), a.String()}); err != nil {
		t.Fatal(err)
	}

	// concurrent calls with different keys don't mix them
	decrypts := func(dir, id, who string) error {
		if _, err := (Decryptor{}).DecryptDir(dir, []string{id}); err != nil {
			return err
		}
		if got, _ := os.ReadFile(filepath.Join(dir, who+".env")); !strings.Contains(string(got), "WHO="+who) {
			return fmt.Errorf("%s: %q", who, got)
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for range 10 {
		wg.Go(func() { errs <- decrypts(freshA(), a.String(), "a") })
		wg.Go(func() { errs <- decrypts(freshB(), b.String(), "b") })
		wg.Go(func() {
			if _, err := (Decryptor{}).DecryptDir(freshA(), []string{b.String()}); err == nil {
				errs <- errors.New("a decrypted with b")
			}
		})
		wg.Go(func() {
			if _, err := (Decryptor{}).DecryptDir(freshB(), []string{a.String()}); err == nil {
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
}

func TestEncrypt(t *testing.T) {
	id := newID(t)
	// each secret is long enough that random ciphertext can't contain it by chance (a 3-character one did)
	for name, plain := range map[string]string{
		"db.sops.env":    "DB_PASSWORD=s3cret-db-password-value\nDB_USER=app\n",
		"conf.sops.yaml": "api:\n  key: abc123-api-key-value\n",
		"pw.sops.txt":    "hunter2-hunter2-hunter2\n",
		"raw.sops.json":  `{"token":"t0k-t0k-t0k-token-value"}`,
	} {
		secret := map[string]string{"db.sops.env": "s3cret-db-password-value", "conf.sops.yaml": "abc123-api-key-value",
			"pw.sops.txt": "hunter2-hunter2-hunter2", "raw.sops.json": "t0k-t0k-t0k-token-value"}[name]
		enc, err := Encrypt([]byte(plain), name, []string{id.Recipient().String()})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(enc), secret) {
			t.Fatalf("%s: plaintext in the encrypted file:\n%s", name, enc)
		}
		// OmegaCD reads it back
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, name), enc, 0o644)
		if _, err := (Decryptor{}).DecryptDir(dir, []string{id.String()}); err != nil {
			t.Fatalf("%s: decrypt: %v", name, err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, strings.Replace(name, ".sops.", ".", 1)))
		if name == "raw.sops.json" || name == "conf.sops.yaml" { // SOPS re-indents structured files, like its CLI
			if !strings.Contains(string(got), secret) {
				t.Fatalf("%s: %q", name, got)
			}
		} else if string(got) != plain {
			t.Fatalf("%s: round trip %q, want %q", name, got, plain)
		}
	}
	if _, err := Encrypt([]byte("A=1\n"), "x.sops.env", nil); err == nil {
		t.Fatal("encrypted without a recipient")
	}
	if _, err := Encrypt([]byte("A=1\n"), "x.sops.env", []string{"age1notakey"}); err == nil {
		t.Fatal("accepted a bad recipient")
	}
}

// The sops CLI opens what Encrypt writes.
func TestEncryptReadableBySopsCLI(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		skipUnlessRequired(t, "sops not on PATH")
	}
	id := newID(t)
	enc, err := Encrypt([]byte("DB_PASSWORD=s3cret\n"), "db.sops.env", []string{id.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "db.sops.env")
	os.WriteFile(f, enc, 0o644)
	cmd := exec.Command("sops", "--decrypt", "--input-type", "dotenv", "--output-type", "dotenv", f)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "SOPS_AGE_KEY=" + id.String()}
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "DB_PASSWORD=s3cret\n" {
		t.Fatalf("sops --decrypt: %v\n%s", err, out)
	}
}

// skipUnlessRequired skips a test whose environment is missing (a tool, a dind that won't start), except in the whole
// suite: make test-all sets OMEGACD_TEST_ALL, and there a missing environment fails, so nothing passes by not running.
func skipUnlessRequired(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("OMEGACD_TEST_ALL") != "" {
		t.Fatalf("make test-all requires this test to run: "+format, args...)
	}
	t.Skipf(format, args...)
}

// Every way a decryption fails is an error naming the file, nothing half-written passed off as decrypted: keys that
// don't parse, a folder that can't be walked, a file that isn't SOPS, a value or MAC tampered with, an output
// path that can't be written; and Encrypt refuses a file in its format that doesn't parse.
func TestDecryptDirFailures(t *testing.T) {
	id := newID(t)
	enc := func(t *testing.T, name, plain string) string {
		t.Helper()
		b, err := Encrypt([]byte(plain), name, []string{id.Recipient().String()})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	decrypt := func(files map[string]string) error {
		dir := t.TempDir()
		for name, content := range files {
			if content == "" {
				os.MkdirAll(filepath.Join(dir, name), 0o755)
				continue
			}
			os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
		}
		_, err := Decryptor{}.DecryptDir(dir, []string{id.String()})
		return err
	}
	// flip changes the character after the first of field in the line of s starting with line
	flip := func(s, line, field string) string {
		i := strings.Index(s, "\n"+line) + 1
		j := i + strings.Index(s[i:], field) + len(field)
		c := byte('A')
		if s[j] == 'A' {
			c = 'B'
		}
		return s[:j] + string(c) + s[j+1:]
	}
	env := enc(t, "db.sops.env", "DB_PASSWORD=s3cret-db-password-value\n")

	if _, err := (Decryptor{}).DecryptDir(t.TempDir(), []string{"not a key"}); err == nil {
		t.Error("a key that doesn't parse")
	}
	if _, err := (Decryptor{}).DecryptDir(filepath.Join(t.TempDir(), "missing"), nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing folder: %v", err)
	}
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"not SOPS":         {map[string]string{"db.sops.env": "A=1\n"}, "decrypt db.sops.env"},
		"a value tampered": {map[string]string{"db.sops.env": flip(env, "DB_PASSWORD=", "tag:")}, "decrypt db.sops.env"},
		"the MAC tampered": {map[string]string{"db.sops.env": flip(env, "sops_mac=", "tag:")}, "integrity check failed"},
		"output a folder":  {map[string]string{"db.sops.env": env, "db.env": ""}, "db.env"},
	} {
		if err := decrypt(tc.files); err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if err := decrypt(map[string]string{"app.sops.ini": enc(t, "app.sops.ini", "[db]\npassword = s3cret-ini-value\n")}); err != nil {
		t.Errorf("ini: %v", err)
	}
	if _, err := Encrypt([]byte("a: [\n"), "x.sops.yaml", []string{id.Recipient().String()}); err == nil {
		t.Error("encrypted YAML that doesn't parse")
	}

	if os.Getuid() == 0 {
		t.Skip("root reads anything") // the suite runs as a normal user
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "db.sops.env"), []byte(env), 0)
	if _, err := (Decryptor{}).DecryptDir(dir, []string{id.String()}); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("an unreadable file: %v", err)
	}
}
