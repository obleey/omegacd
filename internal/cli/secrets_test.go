package cli

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/secrets"
)

// dummySource is a second secret source (Q295): what the CLI asks of any source, recorded.
type dummySource struct {
	written []secrets.Rule
	rekeyed secrets.Rekeyed
}

func (d *dummySource) Rules(string) ([]secrets.Rule, error) { return d.written, nil }
func (d *dummySource) WriteRules(_ string, rules []secrets.Rule) error {
	d.written = rules
	return nil
}
func (d *dummySource) Rekey(string) (secrets.Rekeyed, error) { return d.rekeyed, nil }

// keysAPI is a server with Host web (its Host Key hostKey), Host other, Host db not enrolled yet; ana's key covering
// all Hosts, bo's only other; Applications from the checkout's repository and from another one.
func keysAPI(t *testing.T, hostKey, ana string) *httptest.Server {
	_, srv := newAPI(t, map[string]any{
		"GET /api/settings/hosts": []domain.HostView{{Host: domain.Host{Name: "web", AgeRecipient: hostKey}},
			{Host: domain.Host{Name: "other", AgeRecipient: newIdentity(t).Recipient().String()}}, {Host: domain.Host{Name: "db"}}},
		"GET /api/settings/encryption-keys": []domain.EncryptionKey{{Name: "ana", Recipient: ana, AllHosts: true},
			{Name: "bo", Recipient: newIdentity(t).Recipient().String(), Hosts: []string{"other"}}},
		"GET /api/apps": []domain.AppView{
			{Application: domain.Application{Name: "shop", Host: "web", RepoURL: "https://github.com/acme/server", Path: "apps/shop"}},
			{Application: domain.Application{Name: "root", Host: "web", RepoURL: "ssh://git@github.com/acme/server.git"}},
			{Application: domain.Application{Name: "pg", Host: "web", RepoURL: "https://github.com/acme/db.git", Path: "apps/pg"}},
			{Application: domain.Application{Name: "platform", Kind: domain.KindApps, Host: "web", RepoURL: "https://github.com/acme/server", Path: "platform"}},
			{Application: domain.Application{Name: "cache", Host: "other", RepoURL: "https://github.com/acme/server", Path: "apps/cache"}},
		},
	})
	return srv
}

// secrets init writes a rule per Application of the Host from this repository, and --path's, for its Host Key and
// the people's keys covering it (Q197, Q286); secrets rekey refreshes them from the server and rekeys every file with
// the person's key (Q288); a Host's new key (Q289) makes the files deployable to it again.
func TestSecretsInitAndRekey(t *testing.T) {
	oldHost, host, ana := newIdentity(t), newIdentity(t), newIdentity(t)
	repo := gitRepo(t)
	srv := keysAPI(t, oldHost.Recipient().String(), ana.Recipient().String())
	r := run(t, srv, nil, "", "secrets", "init", "--host", "web", "--path", "/extra/")
	if r.code != 0 || r.stdout != `^apps/shop/.*\.sops\.  Host web, ana`+"\n"+`^.*\.sops\.  Host web, ana`+"\n"+`^extra/.*\.sops\.  Host web, ana`+"\n" ||
		!strings.Contains(r.stderr, "Wrote 3 rule(s) for Host web") {
		t.Fatalf("%+v", r)
	}
	b, _ := os.ReadFile(filepath.Join(repo, ".sops.yaml"))
	if !strings.Contains(string(b), "# omegacd: Host web\n  - path_regex: ^apps/shop/.*\\.sops\\.\n    age: "+oldHost.Recipient().String()+","+ana.Recipient().String()) {
		t.Fatalf(".sops.yaml:\n%s", b)
	}

	// the file sops encrypts by that rule, then the Host enrolls again with a new key
	value := newIdentity(t).String()
	enc, err := secrets.Encrypt([]byte("TOKEN="+value+"\n"), ".sops.env", []string{oldHost.Recipient().String(), ana.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(repo, "apps/shop"), 0o755)
	os.WriteFile(filepath.Join(repo, "apps/shop/.sops.env"), enc, 0o644)
	srv = keysAPI(t, host.Recipient().String(), ana.Recipient().String())
	keys := filepath.Join(t.TempDir(), "keys.txt")
	os.WriteFile(keys, []byte(ana.String()+"\n"), 0o600)
	t.Setenv("SOPS_AGE_KEY_FILE", keys)
	t.Setenv("SOPS_AGE_KEY", "")
	r = run(t, srv, nil, "", "secrets", "rekey")
	if r.code != 0 || r.stdout != "rekeyed     apps/shop/.sops.env\n" || !strings.Contains(r.stderr, "Rekeyed 1 file(s)") {
		t.Fatalf("rekey: %+v", r)
	}
	out := t.TempDir()
	b, _ = os.ReadFile(filepath.Join(repo, "apps/shop/.sops.env"))
	os.WriteFile(filepath.Join(out, ".sops.env"), b, 0o600)
	if _, err := (secrets.Decryptor{}).DecryptDir(out, []string{host.String()}); err != nil {
		t.Fatalf("the Host's new key can't decrypt it: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, ".sops.yaml")); !strings.Contains(string(b), host.Recipient().String()) || strings.Contains(string(b), oldHost.Recipient().String()) {
		t.Fatalf("rules not refreshed:\n%s", b)
	}

	// a file only the old Host Key opens, and one no rule matches
	enc, _ = secrets.Encrypt([]byte("A=1\n"), "x.sops.env", []string{oldHost.Recipient().String()})
	os.WriteFile(filepath.Join(repo, "apps/shop/x.sops.env"), enc, 0o644)
	r = run(t, srv, nil, "", "secrets", "rekey")
	if r.code != 1 || !strings.Contains(r.stdout, "can't open  apps/shop/x.sops.env") || !strings.Contains(r.stderr, "write their values again") {
		t.Fatalf("a file it can't open: %+v", r)
	}
}

// Q295: any source plugs in; what it reports is what the commands say.
func TestSecretsSecondSource(t *testing.T) {
	d := &dummySource{rekeyed: secrets.Rekeyed{NoRule: []string{"loose.sops.env"}}}
	secrets.Sources["dummy"] = d
	t.Cleanup(func() { delete(secrets.Sources, "dummy") })
	gitRepo(t)
	srv := keysAPI(t, newIdentity(t).Recipient().String(), newIdentity(t).Recipient().String())
	if r := run(t, srv, nil, "", "secrets", "init", "--host", "other", "--source", "dummy"); r.code != 0 || len(d.written) != 1 ||
		d.written[0].Path != `^apps/cache/.*\.sops\.` || len(d.written[0].Recipients) != 3 {
		t.Fatalf("%+v %+v", r, d.written)
	}
	if r := run(t, srv, nil, "", "secrets", "rekey", "--source", "dummy"); r.code != 0 || r.stdout != "no rule     loose.sops.env (left as it is)\n" {
		t.Fatalf("rekey: %+v", r)
	}
}

func TestSecretsRefused(t *testing.T) {
	repo := gitRepo(t)
	host := newIdentity(t).Recipient().String()
	url := keysAPI(t, host, newIdentity(t).Recipient().String())
	_, empty := newAPI(t, map[string]any{"GET /api/settings/hosts": []domain.HostView{{Host: domain.Host{Name: "web", AgeRecipient: host}}},
		"GET /api/settings/encryption-keys": []domain.EncryptionKey{}})
	_, noApps := newAPI(t, map[string]any{"GET /api/settings/hosts": []domain.HostView{}, "GET /api/settings/encryption-keys": []domain.EncryptionKey{}})
	_, noKeys := newAPI(t, map[string]any{"GET /api/settings/hosts": []domain.HostView{}})
	for _, c := range []struct {
		srv  *httptest.Server
		args []string
		want string
	}{
		{url, []string{"secrets", "init"}, "needs --host NAME"},
		{url, []string{"secrets", "init", "--host", "web", "--source", "vault"}, `unknown secret source "vault"`},
		{url, []string{"secrets", "init", "--host", "db"}, "Host db has no Host Key yet"},
		{url, []string{"secrets", "init", "--host", "gone"}, "Host gone is not in OmegaCD"},
		{url, []string{"secrets", "init", "--host", "web", "--repo-url", "https://example.com/x.git"}, "no Application of Host web comes from https://example.com/x.git"},
		{empty, []string{"secrets", "init", "--host", "web"}, "or pass --host-only"},
		{empty, []string{"secrets", "init", "--host", "web", "--host-only"}, "GET /api/apps: not found"},
		{noApps, []string{"secrets", "init", "--host", "web"}, "Host web is not in OmegaCD"},
		{noKeys, []string{"secrets", "init", "--host", "web"}, "encryption-keys: not found"},
		{nil, []string{"secrets", "init", "--host", "web"}, "omegacd login"},
		{url, []string{"secrets", "init", "--host", "web", "extra"}, "Usage: omegacd secrets init"},
		{url, []string{"secrets", "rekey"}, "no rules OmegaCD wrote: run omegacd secrets init"},
		{url, []string{"secrets", "rekey", "--source", "vault"}, `unknown secret source "vault"`},
		{url, []string{"secrets", "rekey", "extra"}, "Usage: omegacd secrets rekey"},
	} {
		if r := run(t, c.srv, nil, "", c.args...); r.code == 0 || !strings.Contains(r.stdout+r.stderr, c.want) {
			t.Errorf("%v: %+v, want %q", c.args, r, c.want)
		}
	}

	os.WriteFile(filepath.Join(repo, ".sops.yaml"), []byte("# omegacd: Host gone\ncreation_rules:\n  # omegacd: Host gone\n  - path_regex: ^a/\n    age: age1x\n"), 0o644)
	for _, c := range []struct {
		srv  *httptest.Server
		want string
	}{{url, "the rule for ^a/: Host gone is not in OmegaCD"}, {nil, "omegacd login"}, {noKeys, "encryption-keys: not found"}} {
		if r := run(t, c.srv, nil, "", "secrets", "rekey"); r.code != 1 || !strings.Contains(r.stderr, c.want) {
			t.Errorf("rekey: %+v, want %q", r, c.want)
		}
	}
	os.WriteFile(filepath.Join(repo, ".sops.yaml"), []byte("- not a mapping\n"), 0o644)
	for _, args := range [][]string{{"secrets", "init", "--host", "web"}, {"secrets", "rekey"}} {
		if r := run(t, url, nil, "", args...); r.code != 1 || !strings.Contains(r.stderr, "not a mapping") {
			t.Errorf("%v: %+v", args, r)
		}
	}
	os.WriteFile(filepath.Join(repo, ".sops.yaml"), []byte("creation_rules:\n  # omegacd: Host web\n  - path_regex: ^a/\n    age: age1x\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "a.sops.env"), []byte("not sops\n"), 0o644)
	os.MkdirAll(filepath.Join(repo, "a"), 0o755)
	os.WriteFile(filepath.Join(repo, "a", "b.sops.env"), []byte("not sops\n"), 0o644)
	if r := run(t, url, nil, "", "secrets", "rekey"); r.code != 1 || !strings.Contains(r.stderr, "a/b.sops.env") {
		t.Errorf("a file that isn't SOPS: %+v", r)
	}

	if os.Getuid() != 0 { // root writes whatever the mode
		os.Remove(filepath.Join(repo, "a.sops.env"))
		os.RemoveAll(filepath.Join(repo, "a"))
		os.Chmod(filepath.Join(repo, ".sops.yaml"), 0o444)
		if r := run(t, url, nil, "", "secrets", "rekey"); r.code != 1 || !strings.Contains(r.stderr, "permission denied") {
			t.Errorf("a .sops.yaml it can't write: %+v", r)
		}
	}

	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"secrets", "init", "--host", "web"}, {"secrets", "rekey"}} {
		if r := run(t, url, nil, "", args...); r.code != 1 || !strings.Contains(r.stderr, "inside your Git repository") {
			t.Errorf("%v outside a checkout: %+v", args, r)
		}
	}
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init", "-q").Run()
	t.Chdir(dir)
	if r := run(t, url, nil, "", "secrets", "init", "--host", "web"); r.code != 1 || !strings.Contains(r.stderr, "no origin remote") {
		t.Errorf("no origin: %+v", r)
	}
}

// The key list through the CLI (Q292): saved for all Hosts or the ones named, listed as a table or JSON, Q191's
// warning when empty, and Q193's advice on removal.
func TestSecretsKeys(t *testing.T) {
	pub := newIdentity(t).Recipient().String()
	f, srv := newAPI(t, map[string]any{
		"PUT /api/settings/encryption-keys/ana laptop": domain.EncryptionKey{Name: "ana laptop", Recipient: pub, Hosts: []string{"web", "db"}},
		"PUT /api/settings/encryption-keys/all":        domain.EncryptionKey{Name: "all", Recipient: pub, AllHosts: true},
		"GET /api/settings/encryption-keys": []domain.EncryptionKey{{Name: "ana laptop", Recipient: pub, Hosts: []string{"web"}},
			{Name: "all", Recipient: pub, AllHosts: true}, {Name: "orphan", Recipient: pub, Hosts: []string{}}},
		"DELETE /api/settings/encryption-keys/ana laptop": nil,
	})
	if r := run(t, srv, nil, "", "secrets", "key", "add", "ana laptop", pub, "--host", "web", "--host", "db"); r.code != 0 ||
		!strings.Contains(r.stdout, "Saved ana laptop for web, db. Run omegacd secrets rekey") {
		t.Fatalf("%+v", r)
	}
	if want := `"hosts":["web","db"]`; !strings.Contains(f.bodies[0], want) || !strings.Contains(f.bodies[0], `"allHosts":false`) {
		t.Errorf("body %s", f.bodies[0])
	}
	if r := run(t, srv, nil, "", "secrets", "key", "add", "all", pub); r.code != 0 || !strings.Contains(r.stdout, "for all Hosts") {
		t.Fatalf("%+v", r)
	}
	r := run(t, srv, nil, "", "secrets", "key", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "ana laptop") || !strings.Contains(r.stdout, "all Hosts") || !strings.Contains(r.stdout, "no Host") {
		t.Fatalf("%+v", r)
	}
	if r := run(t, srv, nil, "", "secrets", "key", "list", "-o", "json"); r.code != 0 || !strings.Contains(r.stdout, `"name": "orphan"`) {
		t.Fatalf("%+v", r)
	}
	if r := run(t, srv, nil, "", "secrets", "key", "remove", "ana laptop"); r.code != 0 || !strings.Contains(r.stdout, "change the values") {
		t.Fatalf("%+v", r)
	}
	_, none := newAPI(t, map[string]any{"GET /api/settings/encryption-keys": []domain.EncryptionKey{}})
	if r := run(t, none, nil, "", "secrets", "key", "list"); r.code != 0 || !strings.Contains(r.stderr, "only the Hosts can decrypt") {
		t.Fatalf("empty: %+v", r)
	}
	for _, args := range [][]string{{"secrets", "key", "add", "x", pub}, {"secrets", "key", "remove", "x"}, {"secrets", "key", "list"}} {
		if r := run(t, nil, nil, "", args...); r.code != 1 || !strings.Contains(r.stderr, "omegacd login") {
			t.Errorf("%v not logged in: %+v", args, r)
		}
		if r := run(t, srv, nil, "", append(args, "extra", "more")...); r.code != 2 {
			t.Errorf("%v with too many arguments: %+v", args, r)
		}
	}
	_, broken := newAPI(t, map[string]any{})
	for _, args := range [][]string{{"secrets", "key", "add", "x", pub}, {"secrets", "key", "remove", "x"}, {"secrets", "key", "list"}} {
		if r := run(t, broken, nil, "", args...); r.code != 1 || !strings.Contains(r.stderr, "not found") {
			t.Errorf("%v: %+v", args, r)
		}
	}
}
