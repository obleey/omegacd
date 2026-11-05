package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"filippo.io/age"
	"github.com/obleey/omegacd/internal/importer"
	"github.com/obleey/omegacd/internal/secrets"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
)

// fakeAPI serves canned responses keyed by "METHOD path"; a func value is called per request.
type fakeAPI struct {
	mu     sync.Mutex
	routes map[string]any
	calls  []string
	bodies []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer ocd_good" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid token"})
		return
	}
	key := r.Method + " " + r.URL.Path
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		f.bodies = append(f.bodies, string(b))
	}
	v, ok := f.routes[key]
	f.mu.Unlock()
	if fn, isFn := v.(func() any); isFn {
		v = fn()
	}
	switch {
	case !ok:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": key + ": not found"})
	case v == nil:
		w.WriteHeader(http.StatusNoContent)
	default:
		json.NewEncoder(w).Encode(v)
	}
}

func (f *fakeAPI) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, srv *httptest.Server, env map[string]string, stdin string, args ...string) result {
	t.Helper()
	return runCLI(t, srv, env, stdin, nil, args...)
}

// runWith is run with a hook to set up the CLI (e.g. a fake Docker host for import).
func runWith(t *testing.T, srv *httptest.Server, env map[string]string, setup func(*CLI), args ...string) result {
	t.Helper()
	return runCLI(t, srv, env, "", setup, args...)
}

func runCLI(t *testing.T, srv *httptest.Server, env map[string]string, stdin string, setup func(*CLI), args ...string) result {
	t.Helper()
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["XDG_CONFIG_HOME"]; !ok {
		env["XDG_CONFIG_HOME"] = t.TempDir()
	}
	if srv != nil {
		if _, ok := env["OMEGACD_SERVER"]; !ok {
			env["OMEGACD_SERVER"] = srv.URL
		}
		if _, ok := env["OMEGACD_TOKEN"]; !ok {
			env["OMEGACD_TOKEN"] = "ocd_good"
		}
	}
	var out, errOut bytes.Buffer
	c := &CLI{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut,
		Getenv: func(k string) string { return env[k] }, Poll: time.Millisecond,
		StdinTTY: env["STDIN_TTY"] == "1",
	}
	if setup != nil {
		setup(c)
	}
	code := c.Run(context.Background(), args)
	return result{code, out.String(), errOut.String()}
}

func newAPI(t *testing.T, routes map[string]any) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{routes: routes}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestListAndGet(t *testing.T) {
	apps := []domain.AppView{
		{Application: domain.Application{Name: "web", Kind: "compose", Host: "local", Parent: "platform", TargetRevision: "main"}, SyncStatus: domain.Synced, Health: domain.Healthy, DeployedSHA: "abcdef123456"},
		{Application: domain.Application{Name: "platform", Kind: "apps", Host: "local", TargetRevision: "main"}, SyncStatus: domain.OutOfSync, Health: domain.Progressing},
		{Application: domain.Application{Name: "api", Kind: "compose", Host: "edge"}, SyncStatus: domain.Unknown, Health: domain.Missing,
			LastOperation: &domain.SyncOperation{ID: 3, Phase: domain.OpFailed, StartedAt: time.Now()}},
	}
	_, srv := newAPI(t, map[string]any{"GET /api/apps": apps, "GET /api/apps/web": apps[0]})

	r := run(t, srv, nil, "", "app", "list")
	if r.code != 0 {
		t.Fatalf("list: %+v", r)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "NAME") || !strings.HasPrefix(lines[1], "api ") ||
		!strings.HasPrefix(lines[2], "platform ") || !strings.HasPrefix(lines[3], "  web ") {
		t.Fatalf("list output:\n%s", r.stdout)
	}
	if !strings.Contains(lines[3], "main (abcdef1)") || !strings.Contains(lines[1], "Failed") {
		t.Fatalf("list columns:\n%s", r.stdout)
	}

	r = run(t, srv, nil, "", "app", "get", "web", "-o", "json")
	var got domain.AppView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &got) != nil || got.Name != "web" {
		t.Fatalf("get json: %+v", r)
	}
	r = run(t, srv, nil, "", "app", "get", "nope")
	if r.code != 1 || !strings.Contains(r.stderr, "GET /api/apps/nope: not found") {
		t.Fatalf("get missing: %+v", r)
	}
}

func TestUnauthorized(t *testing.T) {
	_, srv := newAPI(t, nil)
	r := run(t, srv, map[string]string{"OMEGACD_TOKEN": "ocd_bad"}, "", "app", "list")
	if r.code != 1 || !strings.Contains(r.stderr, "run omegacd login") {
		t.Fatalf("%+v", r)
	}
}

// syncAPI fakes a sync whose op goes Running → final after a few polls, then health turns Healthy/Degraded.
func syncAPI(t *testing.T, final domain.OpPhase, health domain.HealthStatus) *httptest.Server {
	var mu sync.Mutex
	polls := 0
	finished := time.Now().Add(time.Hour) // set when the op finishes
	_, srv := newAPI(t, map[string]any{
		"POST /api/apps/app/sync": map[string]int64{"opID": 7},
		"GET /api/apps/app/history": func() any {
			mu.Lock()
			defer mu.Unlock()
			polls++
			op := domain.SyncOperation{ID: 7, Phase: domain.OpRunning, StartedAt: time.Now()}
			if polls > 2 {
				if finished.After(time.Now()) {
					finished = time.Now()
				}
				op.Phase, op.Message, op.FinishedAt = final, "compose up: boom", finished
			}
			return []domain.SyncOperation{op, {ID: 6, Phase: domain.OpSucceeded}}
		},
		"GET /api/apps/app": func() any {
			mu.Lock()
			defer mu.Unlock()
			// a stale Degraded from before the op must be ignored
			v := domain.AppView{Application: domain.Application{Name: "app"}, Health: domain.Degraded, ReconciledAt: finished.Add(-time.Hour)}
			if polls > 2 {
				v.Health, v.ReconciledAt = health, finished.Add(time.Millisecond)
			}
			return v
		},
	})
	return srv
}

func TestSyncWait(t *testing.T) {
	r := run(t, syncAPI(t, domain.OpSucceeded, domain.Healthy), nil, "", "app", "sync", "app", "--wait")
	if r.code != 0 || !strings.Contains(r.stdout, "#7 Succeeded") || !strings.Contains(r.stdout, "app: Healthy") {
		t.Fatalf("success: %+v", r)
	}
	r = run(t, syncAPI(t, domain.OpFailed, domain.Healthy), nil, "", "app", "sync", "--wait", "app")
	if r.code != 1 || !strings.Contains(r.stderr, "compose up: boom") {
		t.Fatalf("failed op: %+v", r)
	}
	r = run(t, syncAPI(t, domain.OpSucceeded, domain.Degraded), nil, "", "app", "sync", "app", "--wait")
	if r.code != 1 || !strings.Contains(r.stderr, "Degraded") {
		t.Fatalf("degraded: %+v", r)
	}
	r = run(t, syncAPI(t, domain.OpSucceeded, domain.Degraded), nil, "", "app", "sync", "app", "--wait", "--no-health")
	if r.code != 0 {
		t.Fatalf("no-health: %+v", r)
	}

	_, srv := newAPI(t, map[string]any{
		"POST /api/apps/app/sync":   map[string]int64{"opID": 7},
		"GET /api/apps/app/history": []domain.SyncOperation{{ID: 7, Phase: domain.OpRunning}},
	})
	r = run(t, srv, nil, "", "app", "sync", "app", "--wait", "--timeout", "30ms")
	if r.code != 1 || !strings.Contains(r.stderr, "timed out after 30ms") {
		t.Fatalf("timeout: %+v", r)
	}
}

func TestDiffExitCodes(t *testing.T) {
	_, srv := newAPI(t, map[string]any{
		"GET /api/apps/same/diff": domain.DiffView{FromSHA: "a", ToSHA: "a",
			Services: []domain.ServiceDiff{{Kind: "network", Service: "same_old", Change: "removed"}}},
		"GET /api/apps/drift/diff": domain.DiffView{FromSHA: "a", ToSHA: "b",
			Services: []domain.ServiceDiff{{Service: "web", Change: "changed", Fields: []string{"image"}}, {Kind: "volume", Service: "drift_old", Change: "removed"}},
			GitDiff:  "--- a/compose.yaml\n+++ b/compose.yaml\n@@ -1 +1 @@\n-image: nginx:1\n+image: nginx:2\n"},
	})
	if r := run(t, srv, nil, "", "app", "diff", "same"); r.code != 0 || !strings.Contains(r.stdout, "no differences") {
		t.Fatalf("same: %+v", r)
	}
	r := run(t, srv, nil, "", "app", "diff", "drift")
	if r.code != 1 || !strings.Contains(r.stdout, "~ web: image") || !strings.Contains(r.stdout, "- volume drift_old") || !strings.Contains(r.stdout, "+image: nginx:2") || strings.Contains(r.stdout, "\x1b[") {
		t.Fatalf("drift: %+v", r)
	}
	if r := run(t, srv, nil, "", "app", "diff", "missing"); r.code != 2 {
		t.Fatalf("error: %+v", r)
	}
}

func TestLoginWritesConfigAndEnvOverrides(t *testing.T) {
	_, srv := newAPI(t, map[string]any{
		"GET /api/auth/me": map[string]any{"username": "admin"},
		"GET /api/apps":    []domain.AppView{},
	})
	cfgDir := t.TempDir()
	env := map[string]string{"XDG_CONFIG_HOME": cfgDir, "OMEGACD_SERVER": "", "OMEGACD_TOKEN": ""}

	if r := run(t, srv, env, "", "login", "--server", srv.URL, "--token", "ocd_bad"); r.code != 1 {
		t.Fatalf("bad token login: %+v", r)
	}
	r := run(t, srv, env, "", "login", "--server", srv.URL+"/", "--token", "ocd_good")
	if r.code != 0 {
		t.Fatalf("login: %+v", r)
	}
	p := filepath.Join(cfgDir, "omegacd", "config.json")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", st, err)
	}
	b, _ := os.ReadFile(p)
	var cfg config
	if json.Unmarshal(b, &cfg) != nil || cfg.Server != srv.URL || cfg.Token != "ocd_good" {
		t.Fatalf("config: %s", b)
	}
	if r := run(t, srv, env, "", "app", "list"); r.code != 0 {
		t.Fatalf("list from config: %+v", r)
	}
	// the environment beats the config file
	env["OMEGACD_TOKEN"] = "ocd_bad"
	if r := run(t, srv, env, "", "app", "list"); r.code != 1 || !strings.Contains(r.stderr, "omegacd login") {
		t.Fatalf("env override: %+v", r)
	}
	env["OMEGACD_TOKEN"] = ""
	if r := run(t, srv, env, "", "logout"); r.code != 0 {
		t.Fatalf("logout: %+v", r)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("config still there: %v", err)
	}
}

func TestDeleteConfirmation(t *testing.T) {
	f, srv := newAPI(t, map[string]any{"DELETE /api/apps/web": nil})
	// a terminal that answers no: nothing is deleted
	r := run(t, srv, map[string]string{"STDIN_TTY": "1"}, "n\n", "app", "delete", "web")
	if r.code != 1 || f.called("DELETE") {
		t.Fatalf("declined: %+v", r)
	}
	// no terminal and no --yes: refused, nothing deleted
	r = run(t, srv, nil, "", "app", "delete", "web")
	if r.code != 1 || f.called("DELETE") || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("non-tty without --yes: %+v %v", r, f.calls)
	}
	// no terminal with --yes: no prompt
	r = run(t, srv, nil, "", "app", "delete", "web", "--cascade=false", "--yes")
	if r.code != 0 || !f.called("DELETE /api/apps/web?cascade=false") {
		t.Fatalf("non-tty --yes: %+v %v", r, f.calls)
	}
	// --volumes: the prompt names the volumes, the request asks for them
	r = run(t, srv, map[string]string{"STDIN_TTY": "1"}, "y\n", "app", "delete", "web", "--volumes")
	if r.code != 0 || !strings.Contains(r.stderr, "volumes") || !f.called("DELETE /api/apps/web?cascade=true&volumes=true") {
		t.Fatalf("--volumes: %+v %v", r, f.calls)
	}
	if r := run(t, srv, nil, "", "app", "delete", "web", "--cascade=false", "--volumes", "--yes"); r.code == 0 {
		t.Fatalf("--volumes without --cascade: %+v", r)
	}
}

func TestCreateFromFile(t *testing.T) {
	f, srv := newAPI(t, map[string]any{"POST /api/apps": domain.AppView{}})
	file := filepath.Join(t.TempDir(), "apps.yaml")
	os.WriteFile(file, []byte("name: a\nrepoURL: x\n---\nname: b\nkind: apps\nrepoURL: x\n"), 0o600)
	r := run(t, srv, nil, "", "app", "create", "-f", file)
	if r.code != 0 || len(f.calls) != 2 {
		t.Fatalf("%+v %v", r, f.calls)
	}
	if r := run(t, srv, nil, "", "app", "create", "c"); r.code != 1 || !strings.Contains(r.stderr, "--repo is required") {
		t.Fatalf("missing repo: %+v", r)
	}
	if r := run(t, srv, nil, "", "app", "create", "c", "--repo", "x", "--prune-volumes"); r.code != 1 || !strings.Contains(r.stderr, "--prune-volumes requires --prune") {
		t.Fatalf("--prune-volumes alone: %+v", r)
	}
	f.bodies = nil
	if r := run(t, srv, nil, "", "app", "create", "c", "--repo", "x", "--prune", "--prune-volumes"); r.code != 0 || len(f.bodies) != 1 || !strings.Contains(f.bodies[0], `"pruneVolumes":true`) {
		t.Fatalf("--prune --prune-volumes: %+v %v", r, f.bodies)
	}
}

func TestSyncPrune(t *testing.T) {
	f, srv := newAPI(t, map[string]any{"POST /api/apps/app/sync": map[string]int64{"opID": 7}})
	if r := run(t, srv, nil, "", "app", "sync", "app"); r.code != 0 || len(f.bodies) != 0 {
		t.Fatalf("plain sync sent a body: %+v %v", r, f.bodies)
	}
	if r := run(t, srv, nil, "", "app", "sync", "app", "--prune"); r.code != 0 || len(f.bodies) != 1 || !strings.Contains(f.bodies[0], `"prune":true`) {
		t.Fatalf("--prune: %+v %v", r, f.bodies)
	}
}

func TestHostKeys(t *testing.T) {
	f, srv := newAPI(t, map[string]any{
		"GET /api/settings/hosts/prod/keys": []domain.HostKey{
			{Recipient: "age1new", Current: true, CreatedAt: time.Now()}, {Recipient: "age1old"},
		},
		"POST /api/settings/hosts/prod/keys/import":    domain.HostKey{Recipient: "age1imported", Current: true},
		"POST /api/settings/hosts/prod/keys/generate":  domain.HostKey{Recipient: "age1generated", Current: true},
		"DELETE /api/settings/hosts/prod/keys/age1old": nil,
	})
	if r := run(t, srv, nil, "", "host", "key", "show", "prod"); r.code != 0 ||
		!strings.Contains(r.stdout, "age1new") || !strings.Contains(r.stdout, "current") || !strings.Contains(r.stdout, "previous") {
		t.Fatalf("show: %+v", r)
	}
	file := filepath.Join(t.TempDir(), "prod.key")
	os.WriteFile(file, []byte("# public key: age1imported\nAGE-SECRET-KEY-1SECRET\n"), 0o600)
	if r := run(t, srv, nil, "", "host", "key", "import", "prod", "--file", file); r.code != 0 || !strings.Contains(r.stdout, "age1imported") {
		t.Fatalf("import: %+v", r)
	}
	var body map[string]string
	if len(f.bodies) != 1 || json.Unmarshal([]byte(f.bodies[0]), &body) != nil || body["privateKey"] != "# public key: age1imported\nAGE-SECRET-KEY-1SECRET\n" {
		t.Fatalf("import body: %v", f.bodies)
	}
	if r := run(t, srv, nil, "", "host", "key", "import", "prod"); r.code != 1 || !strings.Contains(r.stderr, "--file is required") {
		t.Fatalf("import without --file: %+v", r)
	}
	if r := run(t, srv, nil, "", "host", "key", "generate", "prod"); r.code != 0 || !strings.Contains(r.stdout, "age1generated") {
		t.Fatalf("generate: %+v", r)
	}
	if r := run(t, srv, nil, "", "host", "key", "remove", "prod", "age1old"); r.code != 0 || !f.called("DELETE /api/settings/hosts/prod/keys/age1old") {
		t.Fatalf("remove: %+v", r)
	}
	if r := run(t, srv, nil, "", "host", "key", "nope"); r.code != 2 {
		t.Fatalf("unknown subcommand: %+v", r)
	}
}

func TestKeygen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "age.key")
	r := run(t, nil, nil, "", "keygen", "--out", path)
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", st.Mode().Perm(), err)
	}
	id, err := crypto.LoadIdentity(path) // what the server reads OMEGACD_AGE_KEY_FILE with
	if err != nil {
		t.Fatal(err)
	}
	if want := "Public key: " + id.Recipient().String() + "\n"; r.stderr != want {
		t.Fatalf("stderr %q, want %q", r.stderr, want)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "# public key: "+id.Recipient().String()) {
		t.Fatalf("no public key comment:\n%s", b)
	}

	// never overwrites: losing the Master Key loses every sealed credential
	if r := run(t, nil, nil, "", "keygen", "--out", path); r.code == 0 {
		t.Fatal("overwrote an existing key")
	}
	if b2, _ := os.ReadFile(path); !bytes.Equal(b, b2) {
		t.Fatal("existing key changed")
	}

	// without -o: the key on stdout, a different one each time
	r1, r2 := run(t, nil, nil, "", "keygen"), run(t, nil, nil, "", "keygen")
	if r1.code != 0 || !strings.Contains(r1.stdout, "AGE-SECRET-KEY-1") || r1.stdout == r2.stdout {
		t.Fatalf("%+v", r1)
	}
}

// importHost is one running stack with a secret in an env file, as importer.Host.
type importHost struct{}

func (importHost) Stacks(context.Context) ([]importer.Stack, error) {
	return []importer.Stack{{Name: "blog", Dir: "/srv/blog", Files: []string{"compose.yaml"}}}, nil
}
func (importHost) Info(_ context.Context, _ string, paths []string) (map[string]importer.Info, error) {
	out := map[string]importer.Info{}
	for _, p := range paths {
		if p == "app.env" {
			out[p] = importer.Info{Kind: 'f', Size: 20}
		}
	}
	return out, nil
}
func (importHost) Read(_ context.Context, _ string, paths []string) (map[string][]byte, error) {
	all := map[string][]byte{
		"compose.yaml": []byte("services:\n  web:\n    image: nginx\n    env_file: app.env\n"),
		"app.env":      []byte("DB_PASSWORD=s3cret\n"),
	}
	out := map[string][]byte{}
	for _, p := range paths {
		if b, ok := all[p]; ok {
			out[p] = b
		}
	}
	return out, nil
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"remote", "add", "origin", "git@github.com:acme/server.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(dir)
	return dir
}

func TestImport(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	_, srv := newAPI(t, map[string]any{"GET /api/settings/hosts": []domain.HostView{
		{Host: domain.Host{Name: "local"}, KeyRecipient: "age1master"},
		{Host: domain.Host{Name: "vps"}, KeyRecipient: id.Recipient().String()},
	}})
	repo := gitRepo(t)
	var dialed string
	withHost := func(c *CLI) {
		c.ImportHost = func(target string) (importer.Host, error) { dialed = target; return importHost{}, nil }
	}
	r := runWith(t, srv, nil, withHost, "import", "--ssh", "root@vps.example.com", "--host", "vps")
	if r.code != 0 || dialed != "root@vps.example.com" {
		t.Fatalf("%+v dialed %q", r, dialed)
	}
	enc, err := os.ReadFile(filepath.Join(repo, "apps/blog/app.sops.env"))
	if err != nil || bytes.Contains(enc, []byte("s3cret")) {
		t.Fatalf("app.sops.env: %v\n%s", err, enc)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.sops.env"), enc, 0o600)
	if _, err := (secrets.Decryptor{}).DecryptDir(dir, []string{id.String()}); err != nil {
		t.Fatalf("encrypted for the Host Key looked up on the server? %v", err)
	}
	defs, _ := os.ReadFile(filepath.Join(repo, "platform/apps.yaml"))
	for _, want := range []string{"name: blog", "repoURL: git@github.com:acme/server.git", "path: apps/blog", "targetRevision: main", "host: vps", "adopt: true", "autoSync: false"} {
		if !strings.Contains(string(defs), want) {
			t.Errorf("definitions lack %q:\n%s", want, defs)
		}
	}
	if !strings.Contains(r.stdout, "✓ blog → apps/blog") || !strings.Contains(r.stdout, "git diff") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
}

func TestImportRecipientFlagAndErrors(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	withHost := func(c *CLI) { c.ImportHost = func(string) (importer.Host, error) { return importHost{}, nil } }
	repo := gitRepo(t)
	// --recipient: no server needed
	if r := runWith(t, nil, nil, withHost, "import", "--recipient", id.Recipient().String(), "--dry-run"); r.code != 0 || !strings.Contains(r.stdout, "blog would go to apps/blog") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(repo, "apps")); err == nil {
		t.Fatal("--dry-run wrote files")
	}
	for _, c := range []struct {
		args []string
		srv  bool
		want string
	}{
		{[]string{"import", "--ssh", "root@vps"}, false, "--ssh needs --host NAME"},
		{[]string{"import"}, false, "pass --recipient age1"},                        // secrets, not logged in
		{[]string{"import", "--host", "edge"}, true, "Host edge is not in OmegaCD"}, // unknown Host on the server
	} {
		var srv *httptest.Server
		if c.srv {
			_, srv = newAPI(t, map[string]any{"GET /api/settings/hosts": []domain.HostView{{Host: domain.Host{Name: "local"}}}})
		}
		r := runWith(t, srv, nil, withHost, c.args...)
		if r.code == 0 || !strings.Contains(r.stdout+r.stderr, c.want) {
			t.Errorf("%v: %+v, want %q", c.args, r, c.want)
		}
	}
	t.Chdir(t.TempDir()) // not a Git checkout
	if r := runWith(t, nil, nil, withHost, "import", "--recipient", id.Recipient().String()); r.code == 0 || !strings.Contains(r.stderr, "inside your Git repository") {
		t.Errorf("outside a repository: %+v", r)
	}
}
