package importer

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

var errHost = errors.New("the host went away")

// brokenHost is a fakeHost whose calls named in fail fail; Read fails only from its second call (the first reads the
// compose files) when failSecond.
type brokenHost struct {
	*fakeHost
	fail       map[string]bool
	failSecond bool
	reads      int
}

func (h *brokenHost) Stacks(ctx context.Context) ([]Stack, error) {
	if h.fail["Stacks"] {
		return nil, errHost
	}
	return h.fakeHost.Stacks(ctx)
}

func (h *brokenHost) Storage(ctx context.Context) (map[string]string, error) {
	if h.fail["Storage"] {
		return nil, errHost
	}
	return h.fakeHost.Storage(ctx)
}

func (h *brokenHost) Info(ctx context.Context, dir string, paths []string) (map[string]Info, error) {
	if h.fail["Info"] {
		return nil, errHost
	}
	return h.fakeHost.Info(ctx, dir, paths)
}

func (h *brokenHost) Read(ctx context.Context, dir string, paths []string) (map[string][]byte, []string, error) {
	h.reads++
	if h.fail["Read"] && (!h.failSecond || h.reads > 1) {
		return nil, nil, errHost
	}
	return h.fakeHost.Read(ctx, dir, paths)
}

// one is a host running one stack, named name in dir, with files (relative to dir).
func one(name string, files map[string]string, composeFiles ...string) *fakeHost {
	d := "/opt/stacks/" + name
	h := &fakeHost{stacks: []Stack{{Name: name, Dir: d, Files: composeFiles}}, files: map[string]string{}}
	if len(composeFiles) == 0 {
		h.stacks[0].Files = []string{"compose.yaml"}
	}
	for f, c := range files {
		h.files[d+"/"+f] = c
	}
	return h
}

func recipient(t *testing.T) func() ([]string, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return func() ([]string, error) { return []string{id.Recipient().String()}, nil }
}

// A stack that can't be imported is reported and skipped, saying why; the run fails naming it.
func TestStackNotImported(t *testing.T) {
	compose := "services:\n  web:\n    image: x\n"
	withSecret := one("shop", map[string]string{"compose.yaml": compose + "    env_file: db.env\n", "db.env": "DB_PASSWORD=" + rand.Text() + "\n"})
	for name, tc := range map[string]struct {
		host Host
		want string
	}{
		"a name that isn't one":    {&fakeHost{stacks: []Stack{{Name: "Shop!", Files: []string{"compose.yaml"}}}}, "is not a valid Application name"},
		"compose files elsewhere":  {&fakeHost{stacks: []Stack{{Name: "shop"}}}, "not in its folder on the host"},
		"a compose file missing":   {one("shop", nil), "compose file compose.yaml not found"},
		"compose that won't load":  {one("shop", map[string]string{"compose.yaml": "services: [\n"}), "read compose files"},
		"a .env that won't load":   {one("shop", map[string]string{"compose.yaml": compose, ".env": "A='open\n"}), "read compose files"},
		"files it can't read":      {&brokenHost{fakeHost: one("shop", map[string]string{"compose.yaml": compose}), fail: map[string]bool{"Read": true}}, "the host went away"},
		"paths it can't tell":      {&brokenHost{fakeHost: one("shop", map[string]string{"compose.yaml": compose}), fail: map[string]bool{"Info": true}}, "the host went away"},
		"config it can't read":     {&brokenHost{fakeHost: one("shop", map[string]string{"compose.yaml": compose}), fail: map[string]bool{"Read": true}, failSecond: true}, "the host went away"},
		"secrets it can't encrypt": {withSecret, "✗ shop"},
	} {
		report, err := run(t, tc.host, opts{HostName: "vps", Recipients: func() ([]string, error) { return []string{"not a recipient"}, nil }})
		if err == nil || !strings.Contains(err.Error(), "not imported: ") || !strings.Contains(report, tc.want) {
			t.Errorf("%s: %v\n%s", name, err, report)
		}
	}
	if _, err := run(t, &brokenHost{fakeHost: &fakeHost{}, fail: map[string]bool{"Stacks": true}}, opts{}); !errors.Is(err, errHost) {
		t.Errorf("stacks: %v", err)
	}
	if _, err := run(t, &brokenHost{fakeHost: one("shop", map[string]string{"compose.yaml": compose}), fail: map[string]bool{"Storage": true}}, opts{}); !errors.Is(err, errHost) {
		t.Errorf("the engine's volumes: %v", err)
	}
	// the Agent's connection gone while it answers: no more stacks are read
	stop, read := errors.New("connection closed"), 0
	h := &fakeHost{stacks: []Stack{{Name: "a", Managed: true}, {Name: "b", Managed: true}}}
	if err := Plan(t.Context(), h, Request{}, func(Result) error { read++; return stop }); err != stop || read != 1 {
		t.Errorf("each failing: %v after %d", err, read)
	}
}

// A definitions file that can't be read stops the import before anything is written.
func TestDefinitionsUnreadable(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "platform"), 0o755)
	os.WriteFile(filepath.Join(repo, "platform", "apps.yaml"), []byte("name: [\n"), 0o644)
	if _, err := run(t, shopHost(), opts{Repo: repo, Recipients: recipient(t)}); err == nil {
		t.Fatal("imported next to definitions it can't read")
	}
}

// What a compose file can hold besides the shop's: a config's file, a path referenced twice, a variable in a mount, a
// long-form mount without a source, a secret set in two places, environment as a list, an env file that won't parse,
// a key inside a file with an innocent name, and no services key at all in a second file.
func TestComposeVariety(t *testing.T) {
	secret, other := rand.Text(), rand.Text()
	key := "-----BEGIN " + "PRIVATE KEY-----\n" + rand.Text() + "\n" // assembled: no key block is committed
	h := one("mix", map[string]string{
		"compose.yaml": `services:
  web:
    image: x
    configs: [site]
    env_file: [web.env, broken.env, gone.env]
    volumes:
      - ./site.conf:/site.conf:ro
      - ./settings.txt:/settings.txt:ro
      - ${DATA_DIR}:/data
      - ./${SUB}/x:/x
      - type: tmpfs
        target: /tmp
    environment:
      - DB_PASSWORD=` + secret + `
      - API_TOKEN=` + other + `x
      - MODE=prod
configs:
  site:
    file: ./site.conf
`,
		"extra.yaml":   "volumes:\n  cache: {}\n",
		".env":         "DB_PASSWORD=" + other + "\nDATA_DIR=/srv\nSUB=s\n",
		"site.conf":    "server {}\n",
		"settings.txt": key,
		"web.env":      "MODE=prod\n",
		"broken.env":   "A='open\n",
	}, "compose.yaml", "extra.yaml")
	repo := t.TempDir()
	report, err := run(t, h, opts{Repo: repo, HostName: "vps", Recipients: recipient(t)})
	if err != nil {
		t.Fatalf("%v\n%s", err, report)
	}
	for _, want := range []string{
		"DB_PASSWORD is in plain text in the compose file and set differently in .env",
		"mounts ${DATA_DIR}, which uses a variable", "mounts ./${SUB}/x, which uses a variable",
		"settings.txt → settings.sops.txt", "gone.env is referenced but not on the host", "web: API_TOKEN",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	b, _ := os.ReadFile(filepath.Join(repo, "apps", "mix", "compose.yaml"))
	if !strings.Contains(string(b), "- MODE=prod") || !strings.Contains(string(b), "- API_TOKEN=${API_TOKEN}") {
		t.Errorf("compose.yaml:\n%s", b)
	}
	defs, _ := os.ReadFile(filepath.Join(repo, "platform", "apps.yaml"))
	if !strings.Contains(string(defs), "composeFiles:") {
		t.Errorf("definitions:\n%s", defs)
	}
}

// Definitions are appended to what the file holds, a line break added where it lacked one; writing that can't happen
// fails the stack.
func TestDefinitionsAppended(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "platform"), 0o755)
	os.WriteFile(filepath.Join(repo, "platform", "apps.yaml"), []byte("name: old\nrepoURL: x\nhost: vps"), 0o644)
	if report, err := run(t, one("web", map[string]string{"compose.yaml": "services:\n  web:\n    image: x\n"}), opts{Repo: repo, HostName: "vps"}); err != nil {
		t.Fatalf("%v\n%s", err, report)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "platform", "apps.yaml"))
	if !strings.Contains(string(b), "host: vps\n---\n# imported") {
		t.Fatalf("appended:\n%s", b)
	}

	if err := appendDefinition(filepath.Join(repo, "platform"), nil); err == nil {
		t.Error("appended to a folder")
	}
	if err := appendDefinition(filepath.Join(repo, "platform", "apps.yaml", "x"), nil); err == nil {
		t.Error("appended under a file")
	}
	if os.Getuid() == 0 {
		return // root writes anywhere; the suite runs as a normal user
	}
	ro := t.TempDir()
	os.Chmod(ro, 0o500)
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	if _, err := run(t, one("web", map[string]string{"compose.yaml": "services:\n  web:\n    image: x\n"}), opts{Repo: ro, HostName: "vps"}); err == nil {
		t.Error("imported into a checkout it can't write")
	}
	repo = t.TempDir()
	app := filepath.Join(repo, "apps", "web")
	os.MkdirAll(app, 0o755)
	os.WriteFile(filepath.Join(app, "old"), nil, 0o644)
	os.Chmod(app, 0o500)
	t.Cleanup(func() { os.Chmod(app, 0o755) })
	if _, err := run(t, one("web", map[string]string{"compose.yaml": "services:\n  web:\n    image: x\n"}), opts{Repo: repo, HostName: "vps", Force: true}); err == nil {
		t.Error("overwrote a folder it can't delete")
	}
}

// Compose files are loaded from a temporary folder: one that can't be made fails the stack.
func TestNoTempFolder(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "gone"))
	report, err := run(t, one("web", map[string]string{"compose.yaml": "services:\n  web:\n    image: x\n"}), opts{HostName: "vps"})
	if err == nil || !strings.Contains(report, "read compose files") {
		t.Fatalf("%v\n%s", err, report)
	}
}

// An env file that can't be read as one is encrypted all the same, as it may hold secrets; what the host's Read left out
// is reported, to be copied by hand.
func TestUnreadableAndLeftOut(t *testing.T) {
	h := one("web", map[string]string{
		"compose.yaml": "services:\n  web:\n    image: x\n    env_file: broken.env\n    volumes: [\"./conf:/conf:ro\"]\n",
		"broken.env":   "A='open\n",
		"conf/a.conf":  "a",
	})
	h.skipped = []string{"conf/link.conf"}
	repo := t.TempDir()
	report, err := run(t, h, opts{Repo: repo, HostName: "vps", Recipients: recipient(t)})
	if err != nil {
		t.Fatalf("%v\n%s", err, report)
	}
	for _, want := range []string{"broken.env can't be read as an env file", "broken.env → broken.sops.env",
		"conf/link.conf left out: not a regular file (a symlink?)"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "apps", "web", "broken.env")); err == nil {
		t.Error("an env file it can't read went to Git as it is")
	}
}
