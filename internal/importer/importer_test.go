package importer

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/appdef"
	"github.com/obleey/omegacd/internal/secrets"
)

// fakeHost serves files of stacks by absolute path; folders are implied by the files under them.
type fakeHost struct {
	stacks []Stack
	files  map[string]string // absolute path -> content
	big    map[string]bool   // absolute paths reported larger than MaxConfig
}

func (h *fakeHost) Stacks(context.Context) ([]Stack, error) { return h.stacks, nil }

func (h *fakeHost) Info(_ context.Context, dir string, paths []string) (map[string]Info, error) {
	out := map[string]Info{}
	for _, p := range paths {
		abs := path.Join(dir, p)
		size := int64(0)
		kind := byte('-')
		for f, c := range h.files {
			switch {
			case f == abs:
				kind, size = 'f', int64(len(c))
			case strings.HasPrefix(f, abs+"/"):
				kind, size = 'd', size+int64(len(c))
			}
		}
		if h.big[abs] {
			size = MaxConfig + 1
		}
		out[p] = Info{kind, size}
	}
	return out, nil
}

func (h *fakeHost) Read(_ context.Context, dir string, paths []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, p := range paths {
		abs := path.Join(dir, p)
		for f, c := range h.files {
			if f == abs || strings.HasPrefix(f, abs+"/") {
				out[strings.TrimPrefix(f, dir+"/")] = []byte(c)
			}
		}
	}
	return out, nil
}

const stackCompose = `# shop: the storefront
services:
  web:
    image: nginx:1.29-alpine
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro   # config file
      - ./html:/usr/share/nginx/html:ro         # small read-only folder: config
      - ./uploads:/uploads                      # writable folder: data
      - type: bind
        source: ./cache
        target: /cache                          # missing on the host: data (Docker creates it)
      - ./dump.sql:/dump.sql                    # big file: data
      - ../shared:/shared                       # outside the stack folder: data
      - /srv/media:/media                       # absolute already: untouched
    env_file: [web.env, plain.env]
    environment:
      DB_PASSWORD: hunter2                      # written in plain text: moves into .sops.env
      DB_HOST: db
      API_TOKEN: ${API_TOKEN}                   # already from .env
    secrets: [db_pass]
  builder:
    build: .
    volumes:
      - ./tls/server.pem:/tls/server.pem:ro     # a private key: encrypted
      - ./config.yaml:/etc/app/config.yaml:ro   # an innocent name, a secret inside: encrypted
      - ./id_ed25519:/root/.ssh/id_ed25519:ro    # a key without an extension: left out
secrets:
  db_pass:
    file: ./db_pass.txt
`

// fake key files, assembled at run time so no key block is ever committed (gitleaks)
var (
	pemKey = "-----BEGIN " + "PRIVATE KEY-----\nk3y-material\n-----END " + "PRIVATE KEY-----\n"
	sshKey = "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nk3y-material\n"
)

func shopHost() *fakeHost {
	d := "/opt/stacks/shop"
	return &fakeHost{
		stacks: []Stack{
			{Name: "shop", Dir: d, Files: []string{"compose.yaml"}},
			{Name: "managed", Dir: "/opt/stacks/managed", Files: []string{"compose.yaml"}, Managed: true},
		},
		files: map[string]string{
			d + "/compose.yaml":                stackCompose,
			d + "/.env":                        "API_TOKEN=tok-from-dotenv\nTZ=Europe/Ljubljana\n",
			d + "/nginx.conf":                  "events {}\n",
			d + "/html/index.html":             "<h1>shop</h1>\n",
			d + "/uploads/photo.jpg":           "jpeg bytes",
			d + "/dump.sql":                    "insert into …",
			d + "/web.env":                     "SESSION_SECRET=s3ss10n\nLOG_LEVEL=info\n",
			d + "/plain.env":                   "WORKERS=4\n",
			d + "/db_pass.txt":                 "pa55word\n",
			d + "/tls/server.pem":              pemKey,
			d + "/config.yaml":                 "listen: :80\ndataStoreEncryptionKey: c2VjcmV0\n",
			d + "/id_ed25519":                  sshKey,
			"/opt/stacks/managed/compose.yaml": "services: {}\n",
		},
		big: map[string]bool{d + "/dump.sql": true},
	}
}

func run(t *testing.T, h Host, o Options) (string, error) {
	t.Helper()
	var out bytes.Buffer
	o.Out = &out
	if o.Repo == "" {
		o.Repo = t.TempDir()
	}
	o.AppsDir, o.Definitions = "apps", "platform/apps.yaml"
	o.RepoURL, o.Revision = "git@github.com:acme/server.git", "main"
	err := Run(context.Background(), h, o)
	return out.String(), err
}

func TestImport(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	repo := t.TempDir()
	asked := 0
	report, err := run(t, shopHost(), Options{Repo: repo, HostName: "vps", Recipients: func() ([]string, error) {
		asked++
		return []string{id.Recipient().String()}, nil
	}})
	if err != nil {
		t.Fatalf("%v\n%s", err, report)
	}
	app := filepath.Join(repo, "apps", "shop")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(app, rel))
		if err != nil {
			t.Fatalf("%s: %v\n%s", rel, err, report)
		}
		return string(b)
	}
	exists := func(rel string) bool { _, err := os.Stat(filepath.Join(app, rel)); return err == nil }

	// config into Git; data, missing, big and outside paths stay on the host
	for _, rel := range []string{"compose.yaml", "nginx.conf", "html/index.html", "plain.env", "tls/server.sops.pem", "config.sops.yaml"} {
		if !exists(rel) {
			t.Errorf("%s not copied\n%s", rel, report)
		}
	}
	for _, rel := range []string{"uploads", "uploads/photo.jpg", "dump.sql", "cache", ".env", "web.env", "db_pass.txt", "tls/server.pem", "config.yaml", "id_ed25519"} {
		if exists(rel) {
			t.Errorf("%s copied (data, or a secret that must only exist encrypted)", rel)
		}
	}
	c := read("compose.yaml")
	for _, want := range []string{
		"- /opt/stacks/shop/uploads:/uploads", "source: /opt/stacks/shop/cache", "- /opt/stacks/shop/dump.sql:/dump.sql",
		"- /opt/stacks/shared:/shared", "- /srv/media:/media", "- ./nginx.conf:/etc/nginx/nginx.conf:ro", "- ./html:/usr/share/nginx/html:ro",
		"DB_PASSWORD: ${DB_PASSWORD}", "DB_HOST: db", "API_TOKEN: ${API_TOKEN}", "# shop: the storefront",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("compose.yaml lacks %q:\n%s", want, c)
		}
	}

	// no secret value anywhere in what was written
	filepath.WalkDir(repo, func(p string, d os.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, secret := range []string{"hunter2", "s3ss10n", "pa55word", "tok-from-dotenv", "k3y-material", "c2VjcmV0"} { // "-": never in an age recipient or base64, where a short secret could turn up by chance
			if strings.Contains(string(b), secret) {
				t.Errorf("%s holds the secret %q in plain text", p, secret)
			}
		}
		return nil
	})

	// the encrypted files decrypt to the originals, the moved secret included
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "tls"), 0o700)
	for _, rel := range []string{".sops.env", "web.sops.env", "db_pass.sops.txt", "tls/server.sops.pem", "config.sops.yaml"} {
		os.WriteFile(filepath.Join(dir, rel), []byte(read(rel)), 0o600)
	}
	if _, err := (secrets.Decryptor{}).DecryptDir(dir, []string{id.String()}); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		".env":           "API_TOKEN=tok-from-dotenv\nTZ=Europe/Ljubljana\nDB_PASSWORD=hunter2\n",
		"web.env":        "SESSION_SECRET=s3ss10n\nLOG_LEVEL=info\n",
		"db_pass.txt":    "pa55word\n",
		"tls/server.pem": pemKey,
		"config.yaml":    "listen: :80\ndataStoreEncryptionKey: c2VjcmV0\n",
	} {
		if b, _ := os.ReadFile(filepath.Join(dir, rel)); string(b) != want {
			t.Errorf("%s decrypts to %q, want %q", rel, b, want)
		}
	}
	if asked != 1 {
		t.Errorf("recipients asked %d times, want once", asked)
	}

	// the definition: adopt, the Host, Auto-Sync off
	apps, err := appdef.Parse(filepath.Join(repo, "platform"))
	if err != nil || len(apps) != 1 {
		t.Fatalf("definitions: %+v %v", apps, err)
	}
	a := apps[0]
	if a.Name != "shop" || a.Path != "apps/shop" || a.Host != "vps" || !a.Adopt || a.AutoSync || a.RepoURL != "git@github.com:acme/server.git" || a.TargetRevision != "main" {
		t.Fatalf("definition: %+v", a)
	}

	// the report says what happened and what the takeover does
	for _, want := range []string{
		"✓ shop → apps/shop (2 services)", "config   nginx.conf", "data     uploads → /opt/stacks/shop/uploads (stays on the host)",
		"secret   web.env → web.sops.env", "moved    web: DB_PASSWORD", "builder uses build:", "secret   tls/server.pem → tls/server.sops.pem", "secret   config.yaml → config.sops.yaml", "id_ed25519 holds secrets but has no file extension",
		"takeover recreates its containers once", "managed: already managed by OmegaCD, skipped",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}

	// importing again: already defined, nothing overwritten
	again, err := run(t, shopHost(), Options{Repo: repo, Recipients: func() ([]string, error) { return nil, nil }})
	if err != nil || !strings.Contains(again, "shop: already defined in platform/apps.yaml, skipped") {
		t.Fatalf("second import: %v\n%s", err, again)
	}
}

func TestImportKeepsContainersWithoutFileMounts(t *testing.T) {
	h := &fakeHost{
		stacks: []Stack{{Name: "cache", Dir: "/opt/cache", Files: []string{"docker-compose.yml"}}},
		files:  map[string]string{"/opt/cache/docker-compose.yml": "services:\n  redis:\n    image: redis:7\n    volumes: [data:/data, ./state:/state]\nvolumes:\n  data: {}\n"},
	}
	repo := t.TempDir()
	report, err := run(t, h, Options{Repo: repo, Recipients: func() ([]string, error) { t.Fatal("asked for recipients without secrets"); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "takeover keeps its containers running") {
		t.Fatalf("report:\n%s", report)
	}
	apps, _ := appdef.Parse(filepath.Join(repo, "platform"))
	if len(apps) != 1 || !slices.Equal(apps[0].ComposeFiles, []string{"docker-compose.yml"}) || apps[0].Host != "local" {
		t.Fatalf("definition: %+v", apps)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "apps/cache/docker-compose.yml"))
	if !strings.Contains(string(b), "/opt/cache/state:/state") || !strings.Contains(string(b), "data:/data") {
		t.Fatalf("compose:\n%s", b)
	}
}

func TestImportDryRunOnlyAndExisting(t *testing.T) {
	repo := t.TempDir()
	id, _ := age.GenerateX25519Identity()
	rec := func() ([]string, error) { return []string{id.Recipient().String()}, nil }
	report, err := run(t, shopHost(), Options{Repo: repo, DryRun: true, Only: []string{"shop"}, Recipients: rec})
	if err != nil || !strings.Contains(report, "✓ shop would go to apps/shop") {
		t.Fatalf("%v\n%s", err, report)
	}
	if entries, _ := os.ReadDir(repo); len(entries) != 0 {
		t.Fatal("dry run wrote files")
	}
	if _, err := run(t, shopHost(), Options{Repo: repo, Only: []string{"nope"}, Recipients: rec}); err == nil || !strings.Contains(err.Error(), `no Compose stack "nope"`) {
		t.Fatalf("unknown stack: %v", err)
	}
	// an existing app folder is not overwritten without --force
	os.MkdirAll(filepath.Join(repo, "apps", "shop"), 0o755)
	report, err = run(t, shopHost(), Options{Repo: repo, Recipients: rec})
	if err == nil || !strings.Contains(report, "apps/shop exists already (--force overwrites it)") {
		t.Fatalf("%v\n%s", err, report)
	}
	if _, err := run(t, shopHost(), Options{Repo: repo, Force: true, Recipients: rec}); err != nil {
		t.Fatal(err)
	}
}

func TestImportNeedsARecipientForSecrets(t *testing.T) {
	repo := t.TempDir()
	report, err := run(t, shopHost(), Options{Repo: repo, Recipients: func() ([]string, error) {
		return nil, os.ErrNotExist
	}})
	if err == nil || !strings.Contains(report, "✗ shop:") {
		t.Fatalf("%v\n%s", err, report)
	}
	if _, err := os.Stat(filepath.Join(repo, "apps", "shop")); err == nil {
		t.Fatal("wrote a stack whose secrets could not be encrypted")
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]string{".env": ".sops.env", "db.env": "db.sops.env", "conf/app.yaml": "conf/app.sops.yaml", "secrets": "", "a/.hidden": ""} {
		if got := sopsName(in); got != want {
			t.Errorf("sopsName(%q) = %q, want %q", in, got, want)
		}
	}
	for v, want := range map[string]string{"abc": "K=abc\n", "a b": "K='a b'\n", "it's": "K=\"it's\"\n", `a"b$c`: "K='a\"b$c'\n"} {
		if got := dotenvLine("K", v); got != want {
			t.Errorf("dotenvLine(%q) = %q, want %q", v, got, want)
		}
	}
	// every dotenvLine reads back as written
	for _, v := range []string{"abc", "a b", "it's", `a"b$c`, `back\slash`, "x#y", "multi\nline"} {
		if got := envKeys(dotenvLine("K", v))["K"]; got != v {
			t.Errorf("round trip of %q: %q", v, got)
		}
	}
	for n, want := range map[string]bool{"shop": true, "my-app_2": true, "Shop": false, "-x": false, "a.b": false, "": false} {
		if validName(n) != want {
			t.Errorf("validName(%q) != %v", n, want)
		}
	}
}
