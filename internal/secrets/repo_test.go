package secrets

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
)

func newKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// OmegaCD's rules go first and replace their own Path's; a person's rules and comments stay (Q287); Rules reads back
// only the ones OmegaCD wrote, with their Host (Q288).
func TestWriteRules(t *testing.T) {
	dir := t.TempDir()
	s := SOPS{}
	if rules, err := s.Rules(dir); err != nil || rules != nil {
		t.Fatalf("no .sops.yaml: %v %v", rules, err)
	}
	must(t, s.WriteRules(dir, []Rule{{Host: "web", Path: `^apps/shop/.*\.sops\.`, Recipients: []string{"age1web", "age1ana"}}}))
	rules, err := s.Rules(dir)
	must(t, err)
	if want := []Rule{{"web", `^apps/shop/.*\.sops\.`, []string{"age1web", "age1ana"}}}; !reflect.DeepEqual(rules, want) {
		t.Fatalf("got %+v", rules)
	}

	write(t, filepath.Join(dir, ".sops.yaml"), `# the team's own
creation_rules:
  # Host web, copied from its card
  # omegacd: Host web
  - path_regex: ^apps/shop/.*\.sops\.
    age: age1old
  # omegacd: Host gone
  - path_regex: ^apps/gone/
    age: age1gone
  - path_regex: .*  # everything else
    age: age1ci
`)
	must(t, s.WriteRules(dir, []Rule{
		{Host: "web", Path: `^apps/shop/.*\.sops\.`, Recipients: []string{"age1web", "age1ana"}},
		{Host: "db", Path: `^apps/pg/.*\.sops\.`, Recipients: []string{"age1db"}},
	}))
	b, err := os.ReadFile(filepath.Join(dir, ".sops.yaml"))
	must(t, err)
	want := `# the team's own
creation_rules:
  # omegacd: Host db
  - path_regex: ^apps/pg/.*\.sops\.
    age: age1db
  # omegacd: Host web
  - path_regex: ^apps/shop/.*\.sops\.
    age: age1web,age1ana
  # omegacd: Host gone
  - path_regex: ^apps/gone/
    age: age1gone
  - path_regex: .* # everything else
    age: age1ci
`
	if string(b) != want {
		t.Fatalf("got\n%s", b)
	}
	if rules, _ = s.Rules(dir); len(rules) != 3 || rules[0].Host != "db" || rules[1].Host != "web" || rules[2].Host != "gone" {
		t.Fatalf("read back %+v", rules)
	}

	write(t, filepath.Join(dir, ".sops.yaml"), "stores: {}\n")
	must(t, s.WriteRules(dir, []Rule{{Host: "web", Path: "^a/", Recipients: []string{"age1web"}}}))
	if rules, _ = s.Rules(dir); len(rules) != 1 {
		t.Fatalf("a config without creation_rules: %+v", rules)
	}
	write(t, filepath.Join(dir, ".sops.yaml"), "creation_rules:\n  - age: age1ci\n") // a rule for every file
	must(t, s.WriteRules(dir, []Rule{{Host: "web", Path: "^a/", Recipients: []string{"age1web"}}}))
	if rules, _ = s.Rules(dir); len(rules) != 1 {
		t.Fatalf("beside a rule without path_regex: %+v", rules)
	}
	write(t, filepath.Join(dir, ".sops.yaml"), "")
	if rules, err = s.Rules(dir); err != nil || rules != nil {
		t.Fatalf("an empty .sops.yaml: %v %v", rules, err)
	}
}

func TestRulesRefused(t *testing.T) {
	for content, says := range map[string]string{
		"a: [":                     ".sops.yaml",
		"- a\n":                    "not a mapping",
		"creation_rules: {a: b}\n": "creation_rules is not a list",
	} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".sops.yaml"), content)
		if _, err := (SOPS{}).Rules(dir); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%q: %v", content, err)
		}
		if err := (SOPS{}).WriteRules(dir, nil); err == nil {
			t.Errorf("%q written", content)
		}
	}
	dir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dir, ".sops.yaml"), 0o755))
	if _, err := (SOPS{}).Rules(dir); err == nil {
		t.Error("a .sops.yaml that is a folder")
	}
}

// Rekey gives each file its rule's keys with the person's key, keeping the values (Q288); a file it can't open and
// one no rule matches are listed and left (Q200); one already right isn't rewritten.
func TestRekey(t *testing.T) {
	person, oldHost, newHost, other := newKey(t), newKey(t), newKey(t), newKey(t)
	keyFile := filepath.Join(t.TempDir(), "keys.txt")
	write(t, keyFile, person.String()+"\n")
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	t.Setenv("SOPS_AGE_KEY", "")
	value := rand.Text()

	dir := t.TempDir()
	enc := func(rel string, keys ...*age.X25519Identity) {
		t.Helper()
		var rs []string
		for _, k := range keys {
			rs = append(rs, k.Recipient().String())
		}
		b, err := Encrypt([]byte("TOKEN="+value+"\n"), filepath.Base(rel), rs)
		must(t, err)
		write(t, filepath.Join(dir, rel), string(b))
	}
	enc("apps/shop/.sops.env", oldHost, person)
	enc("apps/shop/conf/db.sops.env", newHost, person) // already right
	enc("apps/shop/host-only.sops.env", oldHost)
	enc("other/x.sops.env", person)
	write(t, filepath.Join(dir, ".git", "y.sops.env"), "not looked at")
	must(t, (SOPS{}).WriteRules(dir, []Rule{{Host: "web", Path: `^apps/shop/.*\.sops\.`,
		Recipients: []string{newHost.Recipient().String(), person.Recipient().String()}}}))
	right, err := os.ReadFile(filepath.Join(dir, "apps/shop/conf/db.sops.env"))
	must(t, err)

	got, err := (SOPS{}).Rekey(dir)
	must(t, err)
	want := Rekeyed{Changed: []string{"apps/shop/.sops.env"}, CantOpen: []string{"apps/shop/host-only.sops.env"},
		NoRule: []string{"other/x.sops.env"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "apps/shop/conf/db.sops.env")); string(b) != string(right) {
		t.Error("a file already encrypted for its rule's keys was rewritten")
	}
	b, err := os.ReadFile(filepath.Join(dir, "apps/shop/.sops.env"))
	must(t, err)
	keys, err := Recipients(b, ".sops.env")
	must(t, err)
	if len(keys) != 2 || !strings.Contains(strings.Join(keys, " "), newHost.Recipient().String()) {
		t.Fatalf("rekeyed for %v", keys)
	}
	out := t.TempDir()
	write(t, filepath.Join(out, ".sops.env"), string(b))
	files, err := Decryptor{}.DecryptDir(out, []string{newHost.String()})
	must(t, err)
	if plain, _ := os.ReadFile(files[0]); string(plain) != "TOKEN="+value+"\n" {
		t.Fatalf("the new Host Key reads %q", plain)
	}
	if _, err := (Decryptor{}).DecryptDir(out, []string{other.String()}); err != nil {
		t.Fatal(err) // nothing left to decrypt: DecryptDir took the file
	}
}

func TestRekeyErrors(t *testing.T) {
	dir := t.TempDir()
	must(t, (SOPS{}).WriteRules(dir, []Rule{{Host: "web", Path: ".*", Recipients: []string{newKey(t).Recipient().String()}}}))
	write(t, filepath.Join(dir, "bad.sops.env"), "not sops\n")
	if _, err := (SOPS{}).Rekey(dir); err == nil || !strings.Contains(err.Error(), "bad.sops.env") {
		t.Errorf("a file that isn't SOPS: %v", err)
	}
	if _, err := Recipients([]byte("not sops"), "x.sops.env"); err == nil || !strings.Contains(err.Error(), "x.sops.env") {
		t.Errorf("recipients of a file that isn't SOPS: %v", err)
	}
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		return // root reads and writes whatever the mode
	}
	person := newKey(t)
	keyFile := filepath.Join(t.TempDir(), "keys.txt")
	write(t, keyFile, person.String()+"\n")
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	b, err := Encrypt([]byte("A=1\n"), "ro.sops.env", []string{person.Recipient().String()})
	must(t, err)
	must(t, os.Remove(filepath.Join(dir, "bad.sops.env")))
	write(t, filepath.Join(dir, "ro.sops.env"), string(b))
	must(t, os.Chmod(filepath.Join(dir, "ro.sops.env"), 0o444))
	if _, err := (SOPS{}).Rekey(dir); err == nil {
		t.Error("a file it can't write")
	}
	must(t, os.Remove(filepath.Join(dir, "ro.sops.env")))
	must(t, os.Chmod(filepath.Join(dir, ".sops.yaml"), 0o444))
	if err := (SOPS{}).WriteRules(dir, nil); err == nil {
		t.Error("a .sops.yaml it can't write")
	}
	must(t, os.Chmod(filepath.Join(dir, ".sops.yaml"), 0o644))

	locked := filepath.Join(dir, "locked")
	must(t, os.Mkdir(locked, 0o755))
	must(t, os.Chmod(locked, 0))
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if _, err := (SOPS{}).Rekey(dir); err == nil {
		t.Error("an unreadable folder")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
