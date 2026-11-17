package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/importer"
)

// every command with arguments it takes, for what all of them do alike
var everyCommand = [][]string{
	{"login"}, {"app", "list"}, {"app", "get", "web"}, {"app", "create", "web", "--repo", "r"}, {"app", "delete", "web", "--yes"},
	{"app", "sync", "web"}, {"app", "refresh", "web"}, {"app", "diff", "web"}, {"app", "history", "web"},
	{"app", "rollback", "web", "1"}, {"app", "wait", "web"}, {"app", "logs", "web"}, {"app", "export", "web"},
	{"host", "list"}, {"host", "add", "edge"}, {"host", "approve", "edge", "SHA256:x"}, {"host", "rotate", "edge"},
	{"host", "allow", "edge"}, {"host", "unlock", "edge"}, {"host", "delete", "edge"}, {"host", "key", "show", "edge"},
}

// Every command refuses a flag it doesn't know (with its usage), says to log in without a server and token, and fails
// with the server's error.
func TestEveryCommandFails(t *testing.T) {
	_, srv := newAPI(t, map[string]any{}) // knows nothing: every request is 404 with its reason
	for _, cmd := range everyCommand {
		name := strings.Join(cmd[:min(len(cmd), 2)], " ")
		if r := run(t, srv, nil, "", append(cmd, "--bogus")...); r.code != 1 || !strings.Contains(r.stderr, "Usage: omegacd") {
			t.Errorf("%s --bogus: %+v", name, r)
		}
		if r := run(t, nil, nil, "", cmd...); r.code != 1 || !strings.Contains(r.stderr, "omegacd login") && !strings.Contains(r.stderr, "--server and --token") {
			t.Errorf("%s without a server: %+v", name, r)
		}
		if cmd[0] == "login" {
			continue
		}
		if r := run(t, srv, nil, "", cmd...); r.code == 0 || !strings.Contains(r.stderr, "not found") {
			t.Errorf("%s with the server failing: %+v", name, r)
		}
	}
	for _, cmd := range [][]string{{"logout", "--bogus"}, {"keygen", "--bogus"}, {"source-certs", "--bogus"}, {"app", "list", "-o", "yaml"}} {
		if r := run(t, srv, nil, "", cmd...); r.code == 0 {
			t.Errorf("%v: %+v", cmd, r)
		}
	}
}

// What each command prints, beyond what the other tests check: JSON where asked, history, export, the App of Apps'
// Hosts, help, the version.
func TestCommandOutputs(t *testing.T) {
	now := time.Now()
	parent := domain.AppView{Application: domain.Application{Name: "platform", Kind: domain.KindApps}, Hosts: []string{"edge"}}
	child := domain.AppView{Application: domain.Application{Name: "old", Parent: "platform", NotInGit: true}}
	_, srv := newAPI(t, map[string]any{
		"GET /api/apps":                   []domain.AppView{parent},
		"GET /api/apps/platform":          parent,
		"GET /api/apps/old":               child,
		"POST /api/apps/platform/refresh": parent,
		"GET /api/apps/platform/history": []domain.SyncOperation{
			{ID: 2, Revision: "abcdef123", Phase: domain.OpRunning, Trigger: "manual", StartedAt: now},
			{ID: 1, Revision: "0123456789", Phase: domain.OpSucceeded, Trigger: "auto", StartedAt: now.Add(-50 * time.Hour), FinishedAt: now.Add(-50*time.Hour + 3*time.Second), Message: "synced\nto x"},
		},
		"GET /api/apps/platform/export":        http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("name: platform\n")) }),
		"GET /api/apps/platform/diff":          domain.DiffView{Services: []domain.ServiceDiff{{Service: "child", Change: "added"}}},
		"POST /api/settings/hosts/edge/invite": map[string]any{"invite": "omegacd-invite-x", "expiresAt": now},
		"GET /api/settings/hosts/edge/keys":    []domain.HostKey{{Recipient: "age1edge"}},
	})
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"app", "list", "-o", "json"}, 0, `"name": "platform"`},
		{[]string{"app", "get", "platform", "-o", "json"}, 0, `"kind": "apps"`},
		{[]string{"app", "get", "platform"}, 0, "Hosts:           edge"},
		{[]string{"app", "get", "old"}, 0, "no longer declared by platform"},
		{[]string{"app", "refresh", "platform", "-o", "json"}, 0, `"name": "platform"`},
		{[]string{"app", "history", "platform"}, 0, "3s"},
		{[]string{"app", "export", "platform"}, 0, "name: platform"},
		{[]string{"app", "diff", "platform", "-o", "json"}, 1, `"service": "child"`},
		{[]string{"app", "diff", "platform"}, 1, "+ child"},
		{[]string{"host", "add", "edge", "-o", "json"}, 0, `"invite": "omegacd-invite-x"`},
		{[]string{"host", "key", "show", "edge", "-o", "json"}, 0, `"recipient": "age1edge"`},
		{[]string{"version"}, 0, "omegacd "},
		{[]string{"help"}, 0, ""},
		{[]string{"app", "rollback", "platform", "first"}, 1, "OP_ID \"first\" is not a number"},
	} {
		r := run(t, srv, nil, "", tc.args...)
		if r.code != tc.code || !strings.Contains(r.stdout+r.stderr, tc.want) {
			t.Errorf("%v: %+v", tc.args, r)
		}
	}
	if r := run(t, srv, nil, "", "app", "history", "platform"); !strings.Contains(r.stdout, "synced to x") || !strings.Contains(r.stdout, "2d ago") {
		t.Errorf("history: %s", r.stdout)
	}
	for d, want := range map[time.Duration]string{0: "-", 30 * time.Second: "s ago", 10 * time.Minute: "m ago", 5 * time.Hour: "h ago", 72 * time.Hour: "d ago"} {
		at := time.Time{}
		if d > 0 {
			at = now.Add(-d)
		}
		if got := ago(at); !strings.HasSuffix(got, want) {
			t.Errorf("ago(%v) = %q", d, got)
		}
	}
	if (&exitError{msg: "said"}).Error() != "said" {
		t.Error("exitError")
	}
}

// app create from a file: from stdin, a file that isn't there or isn't definitions, one defining nothing, a name and a
// file at once; and JSON out.
func TestCreateFromFiles(t *testing.T) {
	_, srv := newAPI(t, map[string]any{"POST /api/apps": domain.AppView{Application: domain.Application{Name: "web"}}})
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.yaml")
	os.WriteFile(empty, []byte("# nothing\n"), 0o644)
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("name: [\n"), 0o644)
	if r := run(t, srv, nil, "name: web\nrepoURL: r\nhost: edge\n", "app", "create", "-f", "-", "-o", "json"); r.code != 0 || !strings.Contains(r.stdout, `"name": "web"`) {
		t.Errorf("stdin: %+v", r)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-f", filepath.Join(dir, "none.yaml")}, "no such file"},
		{[]string{"-f", bad}, "bad.yaml"},
		{[]string{"-f", empty}, "defines no Applications"},
	} {
		if r := run(t, srv, nil, "", append([]string{"app", "create"}, tc.args...)...); r.code != 1 || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%v: %+v", tc.args, r)
		}
	}
	if r := run(t, srv, nil, "", "app", "create", "web", "-f", empty); r.code != 2 {
		t.Errorf("a name and a file: %+v", r)
	}
}

// app logs picks the container: by name, ID or service, the only one; it refuses none, several and an unknown one;
// the stream's error is the command's.
func TestLogsPicksAContainer(t *testing.T) {
	two := domain.LiveState{Containers: []domain.Container{{ID: "c1", Name: "web-1", Service: "web"}, {ID: "c2", Name: "db-1", Service: "db"}}}
	stream := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte(body))
		})
	}
	_, srv := newAPI(t, map[string]any{
		"GET /api/apps/two/resources":  two,
		"GET /api/apps/one/resources":  domain.LiveState{Containers: two.Containers[:1]},
		"GET /api/apps/none/resources": domain.LiveState{},
		"GET /api/apps/two/logs/c1":    stream("event: log\ndata: hello\n\nevent: end\ndata:\n\n"),
		"GET /api/apps/two/logs/c2":    stream("event: log\ndata: hi\n\nevent: error\ndata: container gone\n\n"),
		"GET /api/apps/one/logs/c1":    stream("event: log\ndata: " + strings.Repeat("x", 3<<20) + "\n\n"),
	})
	for _, tc := range []struct {
		args       []string
		code       int
		out, inErr string
	}{
		{[]string{"two", "web-1"}, 0, "hello", ""},
		{[]string{"two", "web"}, 0, "hello", ""},
		{[]string{"two", "c2"}, 1, "hi", "container gone"},
		{[]string{"two", "cache"}, 1, "", `no container or service "cache"`},
		{[]string{"two"}, 1, "", "several containers; name one: web-1, db-1"},
		{[]string{"none"}, 1, "", "has no containers"},
		{[]string{"one"}, 1, "", "token too long"},
	} {
		r := run(t, srv, nil, "", append([]string{"app", "logs"}, tc.args...)...)
		if r.code != tc.code || !strings.Contains(r.stdout, tc.out) || !strings.Contains(r.stderr, tc.inErr) {
			t.Errorf("%v: %+v", tc.args, r)
		}
	}
}

// app wait waits for Synced and Healthy unless told one; a server failing while it waits fails it; the timeout stops
// a health wait that never ends.
func TestWaitEdges(t *testing.T) {
	healthy := domain.AppView{Application: domain.Application{Name: "web"}, SyncStatus: domain.Synced, Health: domain.Healthy}
	n := 0
	_, srv := newAPI(t, map[string]any{
		"GET /api/apps/web": healthy,
		"GET /api/apps/slow": func() any {
			n++
			return domain.AppView{Application: domain.Application{Name: "slow"}, SyncStatus: domain.Synced, Health: domain.Progressing, ReconciledAt: time.Now()}
		},
		"POST /api/apps/slow/sync":   map[string]int64{"opID": 1},
		"GET /api/apps/slow/history": []domain.SyncOperation{{ID: 1, Phase: domain.OpSucceeded}},
	})
	if r := run(t, srv, nil, "", "app", "wait", "web"); r.code != 0 || !strings.Contains(r.stdout, "Synced, Healthy") {
		t.Errorf("wait: %+v", r)
	}
	slowPoll := func(c *CLI) { c.Poll = time.Hour } // the timeout comes while it sleeps between looks
	if r := runWith(t, srv, nil, slowPoll, "app", "sync", "slow", "--wait", "--timeout", "50ms"); r.code != 1 || !strings.Contains(r.stderr, "timed out after 50ms") {
		t.Errorf("a health that never comes: %+v", r)
	}
	if r := runWith(t, srv, nil, slowPoll, "app", "wait", "slow", "--timeout", "50ms"); r.code != 1 || !strings.Contains(r.stderr, "timed out after 50ms") {
		t.Errorf("waiting for a health that never comes: %+v", r)
	}
	if r := run(t, srv, nil, "", "app", "wait", "ghost"); r.code != 1 {
		t.Errorf("an app the server doesn't know: %+v", r)
	}
	_, gone := newAPI(t, map[string]any{"POST /api/apps/web/sync": map[string]int64{"opID": 1},
		"GET /api/apps/web/history": []domain.SyncOperation{{ID: 1, Phase: domain.OpSucceeded}}})
	if r := run(t, gone, nil, "", "app", "sync", "web", "--wait"); r.code != 1 || !strings.Contains(r.stderr, "not found") {
		t.Errorf("the app gone while waiting for its health: %+v", r)
	}
}

// The CLI's own config: a config file that isn't JSON, a log level that isn't one, no home folder, a config path it
// can't write or remove, a server address that isn't a URL, a key file that exists, certificates under a file.
func TestConfigFailures(t *testing.T) {
	_, srv := newAPI(t, map[string]any{"GET /api/auth/me": map[string]string{"username": "admin"}})
	cfg := t.TempDir()
	os.MkdirAll(filepath.Join(cfg, "omegacd"), 0o700)
	os.WriteFile(filepath.Join(cfg, "omegacd", "config.json"), []byte("not json"), 0o600)
	if r := run(t, nil, map[string]string{"XDG_CONFIG_HOME": cfg}, "", "app", "list"); r.code != 1 || !strings.Contains(r.stderr, "config.json") {
		t.Errorf("a config that isn't JSON: %+v", r)
	}
	if r := run(t, srv, map[string]string{"OMEGACD_LOG_LEVEL": "loud"}, "", "app", "list"); r.code != 1 {
		t.Errorf("a log level that isn't one: %+v", r)
	}
	if r := run(t, srv, map[string]string{"OMEGACD_SERVER": "http://bad host"}, "", "app", "list"); r.code != 1 {
		t.Errorf("a server address that isn't a URL: %+v", r)
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if r := run(t, srv, map[string]string{"XDG_CONFIG_HOME": file}, "", "login"); r.code != 1 {
		t.Errorf("a config folder under a file: %+v", r)
	}
	dirCfg := t.TempDir()
	os.MkdirAll(filepath.Join(dirCfg, "omegacd", "config.json", "x"), 0o700) // the config file's place taken by a folder
	if r := run(t, srv, map[string]string{"XDG_CONFIG_HOME": dirCfg}, "", "login"); r.code != 1 {
		t.Errorf("a config file that is a folder: %+v", r)
	}
	if r := run(t, srv, map[string]string{"XDG_CONFIG_HOME": dirCfg}, "", "logout"); r.code != 1 {
		t.Errorf("logged out of a config it can't remove: %+v", r)
	}
	t.Setenv("HOME", "")
	for _, cmd := range [][]string{{"login"}, {"logout"}} {
		if r := run(t, srv, map[string]string{"XDG_CONFIG_HOME": "", "HOME": ""}, "", cmd...); r.code != 1 {
			t.Errorf("%v without a home: %+v", cmd, r)
		}
	}
	if r := run(t, nil, nil, "", "keygen", "--out", file); r.code != 1 {
		t.Errorf("a key over a file: %+v", r)
	}
	if r := run(t, nil, nil, "", "source-certs", "--out", filepath.Join(file, "tls"), "--host", "a", "--host", "b"); r.code != 1 {
		t.Errorf("certificates under a file: %+v", r)
	}
}

// failingWriter is a stdout that is gone.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

// app diff -o json to a stdout that is gone fails with that.
func TestDiffJSONUnwritten(t *testing.T) {
	_, srv := newAPI(t, map[string]any{"GET /api/apps/web/diff": domain.DiffView{}})
	r := runWith(t, srv, nil, func(c *CLI) { c.Stdout = failingWriter{} }, "app", "diff", "web", "-o", "json")
	if r.code != 1 || !strings.Contains(r.stderr, "stdout closed") {
		t.Fatalf("%+v", r)
	}
}

// import: a checkout without an origin, a Docker it can't reach, a server that can't say the Host Key.
func TestImportEdges(t *testing.T) {
	_, srv := newAPI(t, map[string]any{})
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Chdir(dir)
	if r := run(t, srv, nil, "", "import", "--host", "vps"); r.code != 1 || !strings.Contains(r.stderr, "no origin remote") {
		t.Errorf("no origin: %+v", r)
	}
	gitRepo(t)
	noDocker := func(c *CLI) { c.ImportHost = func() (importer.Host, error) { return nil, errors.New("no socket") } }
	if r := runWith(t, srv, nil, noDocker, "import", "--host", "vps"); r.code != 1 || !strings.Contains(r.stderr, "connect to Docker: no socket") {
		t.Errorf("no Docker: %+v", r)
	}
	withHost := func(c *CLI) { c.ImportHost = func() (importer.Host, error) { return importHost{}, nil } }
	if r := runWith(t, srv, nil, withHost, "import", "--host", "vps"); r.code != 1 || !strings.Contains(r.stdout, "look up the Host Key of vps") {
		t.Errorf("the server can't say: %+v", r)
	}
}

// app logs -f ends without an error when interrupted; keygen to a stdout that is gone fails.
func TestFollowInterruptedAndKeygenUnwritten(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	_, srv := newAPI(t, map[string]any{
		"GET /api/apps/web/resources": domain.LiveState{Containers: []domain.Container{{ID: "c1", Name: "web-1"}}},
		"GET /api/apps/web/logs/c1": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("event: log\ndata: first\n\n"))
			w.(http.Flusher).Flush()
			time.AfterFunc(200*time.Millisecond, cancel) // Ctrl-C while following, once the stream is under way
			<-r.Context().Done()
		}),
	})
	var out bytes.Buffer
	c := &CLI{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out, Poll: time.Millisecond,
		Getenv: func(k string) string {
			return map[string]string{"OMEGACD_SERVER": srv.URL, "OMEGACD_TOKEN": "ocd_good", "XDG_CONFIG_HOME": t.TempDir()}[k]
		}}
	if code := c.Run(ctx, []string{"app", "logs", "web", "-f"}); code != 0 || !strings.Contains(out.String(), "first") {
		t.Errorf("interrupted: %d %q", code, out.String())
	}
	r := runWith(t, nil, nil, func(c *CLI) { c.Stdout = failingWriter{} }, "keygen")
	if r.code != 1 || !strings.Contains(r.stderr, "stdout closed") {
		t.Errorf("keygen: %+v", r)
	}
}
