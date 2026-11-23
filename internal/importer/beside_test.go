package importer

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Issue #9: what a stack references beside its folder is handled as it would be inside it, its copy going into the
// Application without the ../ (Q323, Q324).
func TestImportBesideTheStack(t *testing.T) {
	d, sh := "/srv/stacks/app", "/srv/stacks/shared"
	h := &fakeHost{stacks: []Stack{{Name: "app", Dir: d, Files: []string{"compose.yaml"}}}, files: map[string]string{
		d + "/compose.yaml": `services:
  web:
    image: nginx
    env_file: ../shared/common.env
    volumes:
      - ../shared/proxy.conf:/etc/proxy.conf:ro
      - ../shared/www:/www:ro
      - ../data:/data
  api:
    image: nginx
    env_file:
      - path: ../shared/api.env
        required: true
      - ./local.env
secrets:
  db:
    file: ../shared/db_pass.txt
configs:
  site:
    file: ../shared/site.conf
  inline:
    content: x
`,
		d + "/local.env":         "WORKERS=2\n",
		sh + "/common.env":       "TZ=UTC\n",
		sh + "/api.env":          "API_TOKEN=t0ken-from-shared\n",
		sh + "/proxy.conf":       "upstream {}\n",
		sh + "/www/index.html":   "<h1>hi</h1>\n",
		sh + "/db_pass.txt":      "pa55\n",
		sh + "/site.conf":        "site\n",
		"/srv/stacks/data/a.bin": "data",
	}}
	repo := t.TempDir()
	report, err := run(t, h, opts{Repo: repo, HostName: "box", Recipients: recipient(t)})
	if err != nil {
		t.Fatalf("%v\n%s", err, report)
	}
	app := filepath.Join(repo, "apps", "app")
	for f, plain := range map[string]string{"shared/common.env": "TZ=UTC\n", "shared/proxy.conf": "upstream {}\n", "shared/www/index.html": "<h1>hi</h1>\n",
		"shared/site.conf": "site\n", "local.env": "WORKERS=2\n", "shared/api.sops.env": "", "shared/db_pass.sops.txt": ""} {
		b, err := os.ReadFile(filepath.Join(app, f))
		switch {
		case err != nil:
			t.Errorf("%s: %v\n%s", f, err, report)
		case plain != "" && string(b) != plain:
			t.Errorf("%s = %q", f, b)
		case plain == "" && (!bytes.Contains(b, []byte("sops")) || bytes.Contains(b, []byte("t0ken-from-shared")) || bytes.Contains(b, []byte("pa55"))):
			t.Errorf("%s isn't encrypted: %s", f, b)
		}
	}
	compose, _ := os.ReadFile(filepath.Join(app, "compose.yaml"))
	for _, want := range []string{"env_file: ./shared/common.env", "./shared/proxy.conf:/etc/proxy.conf:ro", "./shared/www:/www:ro",
		"/srv/stacks/data:/data", "path: ./shared/api.env", "- ./local.env", "file: ./shared/db_pass.txt", "file: ./shared/site.conf"} {
		if !strings.Contains(string(compose), want) {
			t.Errorf("compose.yaml lacks %q:\n%s", want, compose)
		}
	}
	if !strings.Contains(report, "data     ../data → /srv/stacks/data") {
		t.Errorf("report:\n%s", report)
	}
}

// Data inside an engine's storage would go with that tool's data after the takeover: the stack fails, naming it
// (Q325); doco-cd's checkout in its volume is issue #9's case.
func TestImportRefusesDataInEngineStorage(t *testing.T) {
	for name, tc := range map[string]struct {
		dir     string
		volumes map[string]string
		want    string
	}{
		"a volume":       {"/var/lib/docker/volumes/doco-cd_data/_data/acme/app", map[string]string{"/var/lib/docker/volumes/doco-cd_data/_data": "doco-cd_data"}, "inside the volume doco-cd_data"},
		"rootless":       {"/home/u/.local/share/docker/volumes/v/_data/app", map[string]string{"/home/u/.local/share/docker/volumes/v/_data": "v"}, "inside the volume v"},
		"a volume gone":  {"/var/lib/docker/volumes/old/_data/app", nil, "inside the engine's storage /var/lib/docker/volumes"},
		"podman storage": {"/var/lib/containers/storage/volumes/x/_data/app", nil, "inside the engine's storage /var/lib/containers/storage"},
	} {
		t.Run(name, func(t *testing.T) {
			h := &fakeHost{volumes: tc.volumes, stacks: []Stack{{Name: "app", Dir: tc.dir, Files: []string{"compose.yaml"}}}, files: map[string]string{
				tc.dir + "/compose.yaml": "services:\n  web:\n    image: x\n    volumes: [./data:/data]\n",
				tc.dir + "/data/x":       "data",
			}}
			repo := t.TempDir()
			report, err := run(t, h, opts{Repo: repo, HostName: "box"})
			if err == nil || !strings.Contains(report, "✗ app: data is "+tc.dir+"/data, "+tc.want) || !strings.Contains(report, "move the data out of it") {
				t.Fatalf("%v\n%s", err, report)
			}
		})
	}
	if inStorage("/srv/app/data", map[string]string{"/var/lib/docker/volumes/v/_data": "v"}) != "" {
		t.Error("a path outside every storage counted as in one")
	}
}

// A copy from beside the stack landing on a file of the stack fails the stack, naming both (Q323).
func TestImportBesideCollides(t *testing.T) {
	d := "/srv/app"
	h := &fakeHost{stacks: []Stack{{Name: "app", Dir: d, Files: []string{"compose.yaml"}}}, files: map[string]string{
		d + "/compose.yaml": "services:\n  web:\n    image: x\n    env_file: [conf/a.env, ../conf/a.env]\n",
		d + "/conf/a.env":   "A=1\n",
		"/srv/conf/a.env":   "A=2\n",
		"/srv/compose.yaml": "services: {}\n",
	}}
	report, err := run(t, h, opts{HostName: "box"})
	if err == nil || !strings.Contains(report, "../conf/a.env and conf/a.env would both be conf/a.env in the Application") &&
		!strings.Contains(report, "conf/a.env and ../conf/a.env would both be conf/a.env in the Application") {
		t.Fatalf("%v\n%s", err, report)
	}
	// the stack's compose file and a copy of the one beside it
	h.files[d+"/compose.yaml"] = "services:\n  web:\n    image: x\n    volumes: [../compose.yaml:/c.yaml:ro]\n"
	if report, err := run(t, h, opts{HostName: "box"}); err == nil || !strings.Contains(report, "would both be compose.yaml") {
		t.Fatalf("%v\n%s", err, report)
	}
	// and .env, made of the stack's own and of secrets moved out of the compose file
	h.files[d+"/compose.yaml"] = "services:\n  web:\n    image: x\n    env_file: ../.env\n"
	h.files["/srv/.env"] = "A=1\n"
	h.files[d+"/.env"] = "B=1\n"
	if report, err := run(t, h, opts{HostName: "box"}); err == nil || !strings.Contains(report, "would both be .env") {
		t.Fatalf("%v\n%s", err, report)
	}
}

// One stack's files are one frame on the Agent's connection: more than MaxStack fails the stack (Q319).
func TestImportTooBig(t *testing.T) {
	d := "/srv/app"
	h := &fakeHost{stacks: []Stack{{Name: "app", Dir: d, Files: []string{"compose.yaml"}}}, files: map[string]string{
		d + "/compose.yaml": "services:\n  web:\n    image: x\nconfigs:\n  big:\n    file: ./big.json\n",
		d + "/big.json":     strings.Repeat("x", MaxStack+1),
	}}
	if report, err := run(t, h, opts{HostName: "box"}); err == nil || !strings.Contains(report, "its files come to 6.0 MB, more than the 6 MB") {
		t.Fatalf("%v\n%s", err, report)
	}
}

// Secrets with no key sent to encrypt them for fail the stack: never written in plain text.
func TestImportNoRecipients(t *testing.T) {
	if report, err := run(t, shopHost(), opts{HostName: "box"}); err == nil || !strings.Contains(report, "no key to encrypt them for") {
		t.Fatalf("%v\n%s", err, report)
	}
}

// The Agent's answer is input like any other: a file outside the Application's folder is refused.
func TestWriteRefusesPathsOutside(t *testing.T) {
	repo := t.TempDir()
	var out bytes.Buffer
	err := Write(Options{Repo: repo, AppsDir: "apps", Definitions: "platform/apps.yaml", Out: &out},
		[]Result{{Name: "x", Files: map[string][]byte{"../../evil": []byte("x")}}})
	if err == nil || !strings.Contains(out.String(), `the Agent sent "../../evil", outside apps/x`) {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

// The helper container reaches paths beside the stack by mounting the folder above it that holds them all.
func TestWithin(t *testing.T) {
	base, rel := within("/a/b", []string{"x", "../c/y", ".."})
	if base != "/a" || !slices.Equal(rel, []string{"b/x", "c/y", "."}) {
		t.Fatalf("within: %s %v", base, rel)
	}
	for p, want := range map[string]string{"b/x": "x", "c/y": "../c/y", ".": ".."} {
		if got := back(base, "/a/b", p); got != want {
			t.Errorf("back(%q) = %q, want %q", p, got, want)
		}
	}
	if base, rel := within("/a", []string{"../../x"}); base != "/" || !slices.Equal(rel, []string{"x"}) {
		t.Errorf("up to the root: %s %v", base, rel)
	}
	for in, want := range map[string]string{"a/b": "a/b", "../a": "a", "../../a/b": "a/b", "..": ".", "../..": "."} {
		if got := local(in); got != want {
			t.Errorf("local(%q) = %q, want %q", in, got, want)
		}
	}
}
