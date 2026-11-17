//go:build integration

package engine

import (
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/enginetest"
	"github.com/obleey/omegacd/internal/logging"
)

// faulty is an Engine whose engine fails the API calls answer picks (Q142), with its client; DOCKER_HOST is the proxy
// for the rest of the (sub)test.
func faulty(t *testing.T, log *slog.Logger, answer func(*http.Request) int) (*Engine, client.APIClient) {
	t.Helper()
	t.Setenv("DOCKER_HOST", enginetest.Faulty(t, answer))
	e, err := New(Config{Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e, e.hc.cli.Client()
}

// fail fails every call to method on pattern.
func fail(method, pattern string) func(*http.Request) int {
	return func(r *http.Request) int {
		if enginetest.Is(r, method, pattern) {
			return http.StatusInternalServerError
		}
		return 0
	}
}

const faultsV1 = `
services:
  web:
    image: busybox
    command: ["sleep", "3600"]
    networks: [front, back]
    volumes: ["va:/a", "vb:/b"]
  extra:
    image: busybox
    command: ["sleep", "3600"]
    profiles: [debug]
    volumes: ["cache:/cache"]
networks: {front: {}, back: {}}
volumes: {va: {}, vb: {}, cache: {}}
`

// web without its networks and volumes (undeclared now, two of each), extra's profile off: its volume still declared
const faultsV2 = `
services:
  web:
    image: busybox
    command: ["sleep", "3600"]
  extra:
    image: busybox
    command: ["sleep", "3600"]
    profiles: [debug]
    volumes: ["cache:/cache"]
volumes: {cache: {}}
`

// Every call the engine can fail fails what asked it, saying what failed; the Sync, the status, the logs, a removal
// and the prune each with its own.
func TestEngineFailures(t *testing.T) {
	ctx := t.Context()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", faultsV1)
	app.Profiles = []string{"debug"}
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	ls, err := e.Status(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range ls.Containers {
		ids[c.Service] = c.ID
	}
	write(t, dir, "compose.yaml", faultsV2)
	app.Profiles = nil

	t.Run("stopping an undeclared service fails the Sync", func(t *testing.T) {
		f, _ := faulty(t, nil, fail("POST", "/containers/"+ids["extra"]+"/stop"))
		if err := f.Sync(ctx, app, dir, domain.SyncOptions{}); err == nil || !strings.Contains(err.Error(), "stop undeclared service extra") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("listing the containers fails the Sync before anything changes", func(t *testing.T) {
		f, _ := faulty(t, nil, fail("GET", "/containers/json"))
		if err := f.Sync(ctx, app, dir, domain.SyncOptions{}); err == nil || !strings.Contains(err.Error(), "list containers") {
			t.Fatalf("%v", err)
		}
	})
	p, err := e.load(ctx, app, dir)
	if err != nil {
		t.Fatal(err)
	}
	// undeclared: two networks and two volumes, sorted; extra's volume is declared still (its profile is only off)
	diffs, err := e.Diff(ctx, app, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, d := range diffs {
		if d.Kind != "" {
			kinds = append(kinds, d.Kind+" "+strings.TrimPrefix(d.Service, app.Name+"_"))
		}
	}
	if strings.Join(kinds, ",") != "network back,network front,volume va,volume vb" {
		t.Fatalf("undeclared %v", kinds)
	}

	t.Run("pruning", func(t *testing.T) {
		_, c := faulty(t, nil, func(r *http.Request) int {
			if enginetest.Is(r, "DELETE", "/containers/*") || enginetest.Is(r, "DELETE", "/networks/*") || enginetest.Is(r, "DELETE", "/volumes/*") {
				return http.StatusInternalServerError
			}
			return 0
		})
		err := settleUndeclared(ctx, logging.Discard, c, p, true)
		if err == nil || !strings.Contains(err.Error(), "prune service extra") {
			t.Fatalf("containers: %v", err)
		}
		err = pruneUndeclared(ctx, logging.Discard, c, p, true)
		for _, want := range []string{"prune network " + app.Name + "_back", "prune network " + app.Name + "_front",
			"prune volume " + app.Name + "_va", "prune volume " + app.Name + "_vb"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("no %q: %v", want, err)
			}
		}
	})
	t.Run("listing for the prune", func(t *testing.T) {
		_, c := faulty(t, nil, fail("GET", "/containers/json"))
		if err := settleUndeclared(ctx, logging.Discard, c, p, true); err == nil || !strings.Contains(err.Error(), "list containers") {
			t.Fatalf("%v", err)
		}
		_, c = faulty(t, nil, fail("GET", "/networks"))
		if err := pruneUndeclared(ctx, logging.Discard, c, p, true); err == nil || !strings.Contains(err.Error(), "list networks") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("a container gone while the status is read is skipped (Q146)", func(t *testing.T) {
		ls, err := e.Status(ctx, app) // web was recreated by the Sync above
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range ls.Containers {
			ids[c.Service] = c.ID
		}
		logs := &logging.Buffer{}
		var gone atomic.Bool
		f, _ := faulty(t, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), func(r *http.Request) int {
			if enginetest.Is(r, "GET", "/containers/"+ids["web"]+"/json") && !gone.Swap(true) {
				return http.StatusNotFound
			}
			return 0
		})
		ls, err = f.Status(ctx, app)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range ls.Containers {
			if c.Service == "web" {
				t.Fatalf("a gone container listed: %+v", ls.Containers)
			}
		}
		if !strings.Contains(logs.String(), "container gone while reading the project: being replaced") {
			t.Fatalf("logs:\n%s", logs.String())
		}
	})
	for _, tc := range []struct{ name, method, pattern, want string }{
		{"inspecting a container", "GET", "/containers/*/json", "inspect container"},
		{"listing the networks", "GET", "/networks", "list networks"},
		{"listing the volumes", "GET", "/volumes", "list volumes"},
	} {
		t.Run("the status fails "+tc.name, func(t *testing.T) {
			f, _ := faulty(t, nil, fail(tc.method, tc.pattern))
			if _, err := f.Status(ctx, app); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v", err)
			}
		})
	}
	t.Run("the rest", func(t *testing.T) {
		f, _ := faulty(t, nil, fail("GET", "/containers/json"))
		if _, _, err := f.ProjectExists(ctx, app.Name); err == nil {
			t.Error("ProjectExists")
		}
		if _, err := f.Inspect(ctx, app, "volume", app.Name+"_cache"); err == nil || !strings.Contains(err.Error(), "list containers") {
			t.Errorf("a volume's containers: %v", err)
		}
		f, _ = faulty(t, nil, fail("GET", "/volumes/*"))
		if _, err := f.Inspect(ctx, app, "volume", app.Name+"_cache"); err == nil || !strings.Contains(err.Error(), "inspect volume") {
			t.Errorf("a volume: %v", err)
		}
		f, _ = faulty(t, nil, fail("GET", "/networks/*"))
		if _, err := f.Inspect(ctx, app, "network", app.Name+"_front"); err == nil || !strings.Contains(err.Error(), "inspect network") {
			t.Errorf("a network: %v", err)
		}
		f, _ = faulty(t, nil, fail("GET", "/containers/*/json"))
		if err := f.Logs(ctx, app, ids["extra"], false, 10, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "failed on purpose") {
			t.Errorf("logs: %v", err)
		}
		f, _ = faulty(t, nil, fail("DELETE", "/containers/*"))
		if err := f.Remove(ctx, app, false); err == nil || !strings.Contains(err.Error(), "compose down") {
			t.Errorf("remove: %v", err)
		}
	})
}

const initFailing = `
services:
  migrate:
    image: busybox
    command: ["sh", "-c", "echo migrating; exit %s"]
  app:
    image: busybox
    command: ["sleep", "3600"]
    depends_on:
      migrate: {condition: service_completed_successfully}
`

// A Sync whose init service can't be brought up, found, waited for or read fails saying so, before the rest changes.
func TestInitServiceFailures(t *testing.T) {
	ctx := t.Context()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", strings.Replace(initFailing, "%s", "0", 1))
	for _, tc := range []struct {
		name, want string
		answer     func(*http.Request) int
	}{
		{"brought up", "compose up (init services migrate)", fail("POST", "/containers/create")},
		{"found", "list init service migrate", func(r *http.Request) int {
			if enginetest.Is(r, "GET", "/containers/json") && strings.Contains(r.URL.Query().Get("filters"), "com.docker.compose.service=migrate") {
				return http.StatusInternalServerError
			}
			return 0
		}},
		{"waited for", "init service migrate", fail("POST", "/containers/*/wait")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := faulty(t, nil, tc.answer)
			if err := f.Sync(ctx, app, dir, domain.SyncOptions{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v", err)
			}
		})
	}
	write(t, dir, "compose.yaml", strings.Replace(initFailing, "%s", "3", 1))
	f, _ := faulty(t, nil, fail("GET", "/containers/*/logs"))
	if err := f.Sync(ctx, app, dir, domain.SyncOptions{}); err == nil || !strings.Contains(err.Error(), "exited 3") || !strings.Contains(err.Error(), "(no logs: ") {
		t.Fatalf("%v", err)
	}
}

// A container with a TTY logs one stream, copied as it is.
func TestLogsOfATTY(t *testing.T) {
	ctx := t.Context()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", "services:\n  tty:\n    image: busybox\n    tty: true\n    command: [\"sh\", \"-c\", \"echo from-a-tty; sleep 3600\"]\n")
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	ls, err := e.Status(ctx, app)
	if err != nil || len(ls.Containers) != 1 {
		t.Fatalf("%+v, %v", ls, err)
	}
	var out strings.Builder
	for range 50 {
		out.Reset()
		if err := e.Logs(ctx, app, ls.Containers[0].ID, false, 10, &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "from-a-tty") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("logs: %q", out.String())
}
