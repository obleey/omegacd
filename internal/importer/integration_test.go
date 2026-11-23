//go:build integration

package importer

import (
	"bytes"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/enginetest"
	"github.com/obleey/omegacd/internal/secrets"
)

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

const itCompose = `services:
  web:
    image: nginx:1.29-alpine
    volumes:
      - ./site.conf:/etc/nginx/conf.d/default.conf:ro
      - ./data:/data
    env_file: app.env
    environment:
      ADMIN_PASSWORD: hunter2
`

func writeStack(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	for name, content := range map[string]string{
		"compose.yaml": itCompose,
		"site.conf":    "server { listen 80; }\n",
		"app.env":      "API_TOKEN=t0ken-value\nMODE=prod\n",
		"data/db.bin":  "user data",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// checkImported verifies apps/<name> in repo against itCompose imported from hostDir.
func checkImported(t *testing.T, repo, name, hostDir, report string, id *age.X25519Identity) {
	t.Helper()
	app := filepath.Join(repo, "apps", name)
	b, err := os.ReadFile(filepath.Join(app, "compose.yaml"))
	if err != nil {
		t.Fatalf("%v\n%s", err, report)
	}
	for _, want := range []string{"- " + hostDir + "/data:/data", "- ./site.conf:/etc/nginx/conf.d/default.conf:ro", "ADMIN_PASSWORD: ${ADMIN_PASSWORD}"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("compose.yaml lacks %q:\n%s", want, b)
		}
	}
	if c, _ := os.ReadFile(filepath.Join(app, "site.conf")); string(c) != "server { listen 80; }\n" {
		t.Errorf("site.conf: %q", c)
	}
	for _, gone := range []string{"data", "app.env", ".env"} {
		if _, err := os.Stat(filepath.Join(app, gone)); err == nil {
			t.Errorf("%s was written to Git", gone)
		}
	}
	dir := t.TempDir()
	for _, f := range []string{"app.sops.env", ".sops.env"} {
		c, err := os.ReadFile(filepath.Join(app, f))
		if err != nil {
			t.Fatalf("%s: %v\n%s", f, err, report)
		}
		if bytes.Contains(c, []byte("t0ken-value")) || bytes.Contains(c, []byte("hunter2")) {
			t.Fatalf("%s holds a secret in plain text", f)
		}
		os.WriteFile(filepath.Join(dir, f), c, 0o600)
	}
	if _, err := (secrets.Decryptor{}).DecryptDir(dir, []string{id.String()}); err != nil {
		t.Fatal(err)
	}
	if c, _ := os.ReadFile(filepath.Join(dir, ".env")); string(c) != "ADMIN_PASSWORD=hunter2\n" {
		t.Errorf(".env decrypts to %q", c)
	}
	if !strings.Contains(report, "takeover recreates its containers once") || !strings.Contains(report, "data     data → "+hostDir+"/data") {
		t.Errorf("report:\n%s", report)
	}
}

func importInto(t *testing.T, h Host, name, hostName string, id *age.X25519Identity) (string, string) {
	t.Helper()
	repo := t.TempDir()
	out, err := run(t, h, opts{Repo: repo, HostName: hostName, Only: []string{name},
		Recipients: func() ([]string, error) { return []string{id.Recipient().String()}, nil }})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return repo, out
}

func TestImportLocal(t *testing.T) {
	dir := t.TempDir()
	writeStack(t, dir)
	const name = "omegacd-it-import"
	if out, err := docker("compose", "-p", name, "--project-directory", dir, "-f", filepath.Join(dir, "compose.yaml"), "up", "-d", "--quiet-pull"); err != nil {
		t.Fatalf("compose up: %v: %s", err, out)
	}
	t.Cleanup(func() { docker("compose", "-p", name, "down", "-v") })
	h, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	id, _ := age.GenerateX25519Identity()
	repo, report := importInto(t, h, name, "box", id)
	real, _ := filepath.EvalSymlinks(dir) // compose records the resolved path (macOS: /private/var/…)
	if !strings.Contains(report, real) {
		real = dir
	}
	checkImported(t, repo, name, real, report, id)
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

// fail fails every call to method on pattern.
func fail(method, pattern string, status int) func(*http.Request) int {
	return func(r *http.Request) int {
		if enginetest.Is(r, method, pattern) {
			return status
		}
		return 0
	}
}

// Every call DockerHost makes to the engine failing fails what asked it, saying which step (Q142): listing the stacks,
// pulling the helper image (refused, or broken off), its container made, started, waited for, its script failing (a
// folder that is a file), and its output copied out. A folder's own entries in the copy are skipped, and what isn't a
// regular file (a symlink) is left out, saying so.
func TestDockerHostFails(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		skipUnlessRequired(t, "docker not on PATH")
	}
	ctx := t.Context()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "conf"), 0o755)
	os.WriteFile(filepath.Join(dir, "conf", "a.conf"), []byte("a"), 0o644)
	os.Symlink("a.conf", filepath.Join(dir, "conf", "link.conf"))
	h, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	if got, skipped, err := h.Read(ctx, dir, []string{"conf"}); err != nil || len(got) != 1 || string(got["conf/a.conf"]) != "a" ||
		strings.Join(skipped, ",") != "conf/link.conf" {
		t.Fatalf("a folder: %v, skipped %v, %v", got, skipped, err)
	}
	// beside the folder: the helper mounts the one above both (Q323)
	os.MkdirAll(filepath.Join(dir, "shared"), 0o755)
	os.WriteFile(filepath.Join(dir, "shared", "b.env"), []byte("B=1\n"), 0o644)
	if info, err := h.Info(ctx, filepath.Join(dir, "conf"), []string{"../shared/b.env", ".."}); err != nil ||
		info["../shared/b.env"] != (Info{'f', 4}) || info[".."].Kind != 'd' {
		t.Fatalf("beside: %v %v", info, err)
	}
	if got, _, err := h.Read(ctx, filepath.Join(dir, "conf"), []string{"../shared/b.env"}); err != nil || string(got["../shared/b.env"]) != "B=1\n" {
		t.Fatalf("read beside: %v %v", got, err)
	}
	if _, err := docker("volume", "create", "omegacd-it-storage"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker("volume", "rm", "omegacd-it-storage") })
	if st, err := h.Storage(ctx); err != nil || !slices.Contains(slices.Collect(maps.Values(st)), "omegacd-it-storage") {
		t.Fatalf("storage: %v %v", st, err)
	}
	if _, err := h.Info(ctx, filepath.Join(dir, "conf", "a.conf"), []string{"x"}); err == nil || !strings.Contains(err.Error(), "exited 2: sh: cd: line 0: can't cd to /src: Not a directory") {
		t.Errorf("a folder that is a file: %v", err)
	}
	h.Close()
	noImage := func(r *http.Request) int {
		if enginetest.Is(r, "GET", "/images/*/json") {
			return http.StatusNotFound
		}
		return 0
	}
	for _, tc := range []struct {
		name, want string
		answer     func(*http.Request) int
	}{
		{"the stacks", "list containers", fail("GET", "/containers/json", 500)},
		{"the volumes", "list volumes", fail("GET", "/volumes", 500)},
		{"the pull refused", "pull " + helperImage, func(r *http.Request) int { return max(noImage(r), fail("POST", "/images/create", 500)(r)) }},
		{"the pull broken off", "pull " + helperImage, func(r *http.Request) int { return max(noImage(r), fail("POST", "/images/create", 200)(r)) }},
		{"its container", "create helper container", fail("POST", "/containers/create", 500)},
		{"started", "start helper container", fail("POST", "/containers/*/start", 500)},
		{"waited for", "helper container", fail("POST", "/containers/*/wait", 500)},
		{"attached to", "attach to helper container", fail("POST", "/containers/*/attach", 500)},
		{"its output broken", "read helper container", fail("POST", "/containers/*/attach", 101)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", enginetest.Faulty(t, tc.answer))
			h, err := Dial()
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			switch {
			case tc.name == "the stacks":
				_, err = h.Stacks(ctx)
			case tc.name == "the volumes":
				_, err = h.Storage(ctx)
			default:
				if _, err = h.Info(ctx, dir, []string{"conf"}); err == nil {
					_, _, err = h.Read(ctx, dir, []string{"conf"})
				}
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v", err)
			}
			if tc.name == "its container" {
				if _, _, err := h.Read(ctx, dir, []string{"conf"}); err == nil {
					t.Error("read through a helper it can't make")
				}
			}
		})
	}
}
