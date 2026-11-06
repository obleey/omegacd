package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/secrets"
)

const (
	plainDB  = "plaintext-database-password-value"
	plainTLS = "plaintext-tls-private-key-value"
)

// appWithSecrets makes an app folder holding Encrypted Secrets for key, as written from the cache.
func appWithSecrets(t *testing.T, key *age.X25519Identity) string {
	dir := filepath.Join(t.TempDir(), "work", "web")
	os.MkdirAll(filepath.Join(dir, "tls"), 0o755)
	for name, plain := range map[string]string{
		".sops.env":           "DB_PASSWORD=" + plainDB + "\nexport DB_USER=app\n# a comment\n",
		"tls/server.sops.pem": plainTLS + "\n",
	} {
		enc, err := secrets.Encrypt([]byte(plain), filepath.Base(name), []string{key.Recipient().String()})
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, name), enc, 0o644)
	}
	return dir
}

func TestDecryptOnlyIntoTheAppFolder(t *testing.T) {
	state := t.TempDir()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // a temp file anywhere would land here
	key, err := HostKey(state)
	if err != nil {
		t.Fatal(err)
	}
	dir := appWithSecrets(t, key)
	ds, err := NewSecrets(-1, SOPS{HostKey: key}).Decrypt(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(ds, func(a, b Decrypted) int { return strings.Compare(a.Path, b.Path) })
	if len(ds) != 2 || ds[0].Path != ".env" || !slices.Equal(ds[0].Keys, []string{"DB_PASSWORD", "DB_USER"}) || ds[1].Path != "tls/server.pem" {
		t.Fatalf("decrypted %+v", ds)
	}
	for _, d := range ds {
		fi, err := os.Stat(filepath.Join(dir, d.Path))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want 0600", d.Path, fi.Mode(), err)
		}
	}
	// plaintext is in the app folder and nowhere else: not the state dir, not a temp dir
	for _, root := range []string{filepath.Dir(dir), state, tmp} {
		filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			b, _ := os.ReadFile(p)
			has := strings.Contains(string(b), plainDB) || strings.Contains(string(b), plainTLS)
			if want := strings.HasPrefix(p, dir+"/") && !strings.Contains(p, ".sops."); has && !want {
				t.Errorf("plaintext in %s", p)
			}
			return nil
		})
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "tls/server.pem")); string(b) != plainTLS+"\n" {
		t.Fatalf("server.pem = %q", b)
	}
}

func TestDecryptWithAGroup(t *testing.T) {
	key, _ := HostKey(t.TempDir())
	dir := appWithSecrets(t, key)
	gid := os.Getgid() // a group the test's user is in, as the Agent's user must be
	if _, err := NewSecrets(gid, SOPS{HostKey: key}).Decrypt(dir); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, ".env"))
	if fi.Mode().Perm() != 0o640 || int(fi.Sys().(*syscall.Stat_t).Gid) != gid {
		t.Fatalf(".env: %v gid %d, want 0640 and gid %d", fi.Mode(), fi.Sys().(*syscall.Stat_t).Gid, gid)
	}
}

func TestDecryptWithTheWrongKey(t *testing.T) {
	key, _ := HostKey(t.TempDir())
	other, _ := HostKey(t.TempDir())
	dir := appWithSecrets(t, key)
	if _, err := NewSecrets(-1, SOPS{HostKey: other}).Decrypt(dir); err == nil {
		t.Fatal("decrypted with another Host's key")
	}
}

// rot13 is a second secret source: "*.rot13" files, to show sources plug in without changing callers.
type rot13 struct{}

func (rot13) Decrypt(dir string) ([]Decrypted, error) {
	var out []Decrypted
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !strings.HasSuffix(p, ".rot13") {
			return err
		}
		b, _ := os.ReadFile(p)
		for i, c := range b {
			switch {
			case c >= 'a' && c <= 'z':
				b[i] = 'a' + (c-'a'+13)%26
			case c >= 'A' && c <= 'Z':
				b[i] = 'A' + (c-'A'+13)%26
			}
		}
		out = append(out, Decrypted{Path: strings.TrimSuffix(filepath.Base(p), ".rot13")})
		return os.WriteFile(strings.TrimSuffix(p, ".rot13"), b, 0o644)
	})
	return out, err
}

func TestSecondSecretSource(t *testing.T) {
	key, _ := HostKey(t.TempDir())
	dir := appWithSecrets(t, key)
	os.WriteFile(filepath.Join(dir, "token.rot13"), []byte("uryyb"), 0o644)
	ds, err := NewSecrets(-1, SOPS{HostKey: key}, rot13{}).Decrypt(dir)
	if err != nil || len(ds) != 3 {
		t.Fatalf("%+v, %v", ds, err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "token"))
	if b, _ := os.ReadFile(filepath.Join(dir, "token")); string(b) != "hello" || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token = %q %v: the second source's file gets the same mode", b, fi.Mode())
	}
}

func TestHostKey(t *testing.T) {
	state := t.TempDir()
	k1, err := HostKey(state)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := HostKey(state)
	if err != nil || k2.String() != k1.String() {
		t.Fatal("the Host Key changed on the second start")
	}
	if fi, _ := os.Stat(filepath.Join(state, "host.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("host.key mode %v", fi.Mode())
	}
}
