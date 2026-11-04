//go:build integration

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/obleey/dockercd/internal/domain"
)

const fixture = `
services:
  web:
    image: nginx:alpine
    environment:
      GREETING: ${GREETING}
      LEAK: ${DOCKERCD_IT_SECRET:-none}
    volumes: [data:/data]
  once:
    image: busybox
    command: ["sh", "-c", "echo hello-from-once"]
  extra:
    image: busybox
    command: ["sleep", "3600"]
    volumes: [cache:/cache]
volumes:
  data: {}
  cache: {}
`

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func (e *Engine) local() *hostConn { return e.conns[domain.LocalHost] }

func testApp(t *testing.T, e *Engine) domain.Application {
	t.Helper()
	app := domain.Application{Name: fmt.Sprintf("dockercd-it-%d", rand.IntN(1e9))}
	t.Cleanup(func() {
		_ = e.local().compose.Down(context.Background(), app.Name, api.DownOptions{RemoveOrphans: true, Volumes: true})
	})
	return app
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsBuild(t *testing.T) {
	e := newTestEngine(t)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", "services:\n  app:\n    build: .\n")
	err := e.Validate(context.Background(), domain.Application{Name: "dockercd-it-build"}, dir)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Validate = %v, want ErrInvalid", err)
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", fixture)
	write(t, dir, ".env", "GREETING=hi\n")
	t.Setenv("DOCKERCD_IT_SECRET", "leaked")

	if err := e.Validate(ctx, app, dir); err != nil {
		t.Fatal(err)
	}
	var events []domain.ProgressEvent
	err := e.Sync(ctx, app, dir, domain.SyncOptions{Progress: func(ev domain.ProgressEvent) { events = append(events, ev) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Error("no progress events")
	}

	ls, err := e.Status(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	if ls.Health != domain.Healthy || len(ls.Containers) != 3 {
		t.Fatalf("Status = %+v, want 3 containers Healthy", ls)
	}

	t.Run("vendored hash equals compose config-hash label", func(t *testing.T) {
		p, err := e.load(ctx, app, dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := *p.Services["web"].Environment["LEAK"]; got != "none" {
			t.Errorf("process env leaked into interpolation: LEAK=%q", got)
		}
		ctrs, err := containers(ctx, e.local().cli.Client(), app.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range ctrs {
			svc := c.Labels[api.ServiceLabel]
			want, err := serviceHash(p.Services[svc])
			if err != nil {
				t.Fatal(err)
			}
			if got := c.Labels[api.ConfigHashLabel]; got != want {
				t.Errorf("%s: compose hash %s, vendored hash %s", svc, got, want)
			}
			if c.Labels[domain.ManagedLabel] != app.Name {
				t.Errorf("%s: missing managed label", svc)
			}
		}
	})

	if diffs, err := e.Diff(ctx, app, dir, ""); err != nil || len(diffs) != 0 {
		t.Fatalf("Diff after sync = %+v, %v; want empty", diffs, err)
	}

	write(t, dir, ".env", "GREETING=bye\n")
	diffs, err := e.Diff(ctx, app, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Service != "web" || diffs[0].Change != "changed" {
		t.Fatalf("Diff after env change = %+v", diffs)
	}

	var once string
	for _, c := range ls.Containers {
		if c.Service == "once" {
			once = c.ID
		}
	}
	var buf bytes.Buffer
	for range 20 {
		buf.Reset()
		if err := e.Logs(ctx, domain.LocalHost, once, false, 0, &buf); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "hello-from-once") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "hello-from-once") {
		t.Errorf("Logs = %q", buf.String())
	}

	pruned := strings.Replace(fixture, "  extra:\n    image: busybox\n    command: [\"sleep\", \"3600\"]\n    volumes: [cache:/cache]\n", "", 1)
	pruned = strings.Replace(pruned, "  cache: {}\n", "", 1)
	write(t, dir, "compose.yaml", pruned)
	if diffs, _ := e.Diff(ctx, app, dir, ""); !slices.ContainsFunc(diffs, func(d domain.ServiceDiff) bool { return d.Service == "extra" && d.Change == "removed" }) {
		t.Errorf("Diff before prune = %+v, want extra removed", diffs)
	}
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{Prune: true}); err != nil {
		t.Fatal(err)
	}
	ls, err = e.Status(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range ls.Containers {
		if c.Service == "extra" {
			t.Errorf("extra container survived prune: %+v", c)
		}
	}
	if !slices.ContainsFunc(ls.Resources, func(r domain.Resource) bool { return r.Kind == "volume" && r.Name == app.Name+"_cache" }) {
		t.Errorf("cache volume pruned; resources = %+v", ls.Resources)
	}
	if !slices.Equal(ls.Services, []string{"once", "web"}) {
		t.Errorf("Services = %v", ls.Services)
	}

	if exists, managed, err := e.ProjectExists(ctx, "", app.Name); err != nil || !exists || !managed {
		t.Errorf("ProjectExists(managed) = %v, %v, %v", exists, managed, err)
	}

	if err := e.Remove(ctx, app); err != nil {
		t.Fatal(err)
	}
	if exists, _, err := e.ProjectExists(ctx, "", app.Name); err != nil || exists {
		t.Errorf("ProjectExists after Remove = %v, %v", exists, err)
	}
}

func TestProjectExistsUnmanaged(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", "services:\n  sleeper:\n    image: busybox\n    command: [\"sleep\", \"3600\"]\n")

	// Plain compose SDK, as `docker compose up` would do it: no dockercd label.
	p, err := e.local().compose.LoadProject(ctx, api.ProjectLoadOptions{
		ProjectName: app.Name, WorkingDir: dir, ConfigPaths: []string{filepath.Join(dir, "compose.yaml")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.local().compose.Up(ctx, p, api.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	exists, managed, err := e.ProjectExists(ctx, "", app.Name)
	if err != nil || !exists || managed {
		t.Fatalf("ProjectExists(unmanaged) = %v, %v, %v; want true, false", exists, managed, err)
	}
	// Adopting must not recreate: our loader and labels hash the same as plain compose.
	if diffs, err := e.Diff(ctx, app, dir, ""); err != nil || len(diffs) != 0 {
		t.Errorf("Diff of unmanaged project = %+v, %v; want empty", diffs, err)
	}
	if exists, _, _ := e.ProjectExists(ctx, "", app.Name+"-nope"); exists {
		t.Error("ProjectExists(missing) = true")
	}
}

func TestDiffFromStagingDir(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir, stage := t.TempDir(), t.TempDir()
	const compose = `
services:
  web:
    image: busybox
    command: ["sleep", "3600"]
    env_file: app.env
    volumes: [./conf:/conf:ro]
`
	for _, d := range []string{dir, stage} {
		write(t, d, "compose.yaml", compose)
		write(t, d, "app.env", "A=1\n")
		os.Mkdir(filepath.Join(d, "conf"), 0o755)
	}
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if diffs, err := e.Diff(ctx, app, stage, dir); err != nil || len(diffs) != 0 {
		t.Fatalf("identical staged copy: Diff = %+v, %v; want empty", diffs, err)
	}
	write(t, stage, "app.env", "A=2\n")
	if diffs, _ := e.Diff(ctx, app, stage, dir); len(diffs) != 1 || diffs[0].Service != "web" {
		t.Fatalf("staged env change: Diff = %+v; want web changed", diffs)
	}
}

func TestStoppedLongRunningServiceIsDrift(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", `
services:
  web:
    image: busybox
    command: ["sleep", "3600"]
    restart: unless-stopped
`)
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	ls, _ := e.Status(ctx, app)
	timeout := 0
	if _, err := e.local().cli.Client().ContainerStop(ctx, ls.Containers[0].ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		t.Fatal(err)
	}
	if diffs, _ := e.Diff(ctx, app, dir, ""); len(diffs) != 1 || diffs[0].Change != "stopped" {
		t.Fatalf("Diff = %+v; want web stopped", diffs)
	}
	if ls, _ = e.Status(ctx, app); ls.Health != domain.Degraded {
		t.Fatalf("Health = %s; want Degraded", ls.Health)
	}
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if diffs, _ := e.Diff(ctx, app, dir, ""); len(diffs) != 0 {
		t.Fatalf("after re-sync Diff = %+v; want empty", diffs)
	}
}

func TestDiffResolvesServiceReferences(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", `
services:
  db:
    image: busybox
    command: ["sleep", "3600"]
    volumes: [/data]
    ipc: shareable
  app:
    image: busybox
    command: ["sleep", "3600"]
    network_mode: "service:db"
    ipc: "service:db"
    pid: "service:db"
    volumes_from: [db]
`)
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if diffs, err := e.Diff(ctx, app, dir, ""); err != nil || len(diffs) != 0 {
		t.Fatalf("Diff after sync = %+v, %v; want empty", diffs, err)
	}
}

func TestWatchReportsContainerChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", "services:\n  web:\n    image: busybox\n    command: [\"sleep\", \"3600\"]\n")

	seen := make(chan string, 64)
	go e.Watch(ctx, func(_, p string) { seen <- p })
	time.Sleep(500 * time.Millisecond) // let the event stream connect
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case p := <-seen:
			if p == app.Name {
				return
			}
		case <-deadline:
			t.Fatal("no docker event for the project")
		}
	}
}

func TestContentChangesRecreate(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", `
services:
  web:
    image: busybox
    command: ["sleep", "3600"]
    env_file: app.env
    configs: [{source: conf, target: /etc/app.conf}]
    secrets: [pw]
    volumes: ["./nginx.conf:/etc/nginx.conf:ro", "./data:/data"]
  plain:
    image: busybox
    command: ["sleep", "3600"]
configs:
  conf: {file: ./app.conf}
secrets:
  pw: {file: ./pw.txt}
`)
	write(t, dir, "app.env", "A=1\n")
	write(t, dir, "app.conf", "v1\n")
	write(t, dir, "pw.txt", "s1\n")
	write(t, dir, "nginx.conf", "n1\n")
	os.Mkdir(filepath.Join(dir, "data"), 0o755)
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	webID := func() string {
		ls, _ := e.Status(ctx, app)
		for _, c := range ls.Containers {
			if c.Service == "web" {
				return c.ID
			}
		}
		return ""
	}
	cases := []struct {
		file, content string
		recreate      bool
	}{
		{"app.conf", "v2\n", true},
		{"pw.txt", "s2\n", true},
		{"nginx.conf", "n2\n", true},
		{"app.env", "A=2\n", true},
		{"data/runtime.db", "written by the app\n", false},
	}
	for _, c := range cases {
		before := webID()
		write(t, dir, c.file, c.content)
		diffs, err := e.Diff(ctx, app, dir, "")
		if err != nil {
			t.Fatal(err)
		}
		changed := len(diffs) == 1 && diffs[0].Service == "web" && diffs[0].Change == "changed"
		if changed != c.recreate || (!c.recreate && len(diffs) != 0) {
			t.Errorf("%s: diff = %+v, want web changed = %v", c.file, diffs, c.recreate)
		}
		if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
			t.Fatal(err)
		}
		if recreated := before != webID(); recreated != c.recreate {
			t.Errorf("%s: recreated = %v, want %v", c.file, recreated, c.recreate)
		}
	}
}

func TestProjectFilesAndInspect(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", `
services:
  web:
    image: busybox
    command: ["sleep", "3600"]
    env_file: [web.env]
    volumes: [data:/data, ./site.conf:/etc/site.conf, ./html:/html, /tmp:/host-tmp]
    configs: [{source: app, target: /etc/app.yaml}]
    secrets: [pw]
configs:
  app: {file: ./app.yaml}
secrets:
  pw: {file: ./pw.txt}
volumes:
  data: {}
`)
	for _, f := range []string{".env", "web.env", "site.conf", "app.yaml", "pw.txt"} {
		write(t, dir, f, "X=1\n")
	}
	if err := os.Mkdir(filepath.Join(dir, "html"), 0o755); err != nil {
		t.Fatal(err)
	}

	files, err := e.ProjectFiles(ctx, app, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Resource{
		{Kind: "bind", Name: "html", Services: []string{"web"}, Target: "/html"},
		{Kind: "bind", Name: "site.conf", Services: []string{"web"}, Target: "/etc/site.conf"},
		{Kind: "config", Name: "app.yaml", Services: []string{"web"}, Target: "/etc/app.yaml"},
		{Kind: "env_file", Name: ".env"},
		{Kind: "env_file", Name: "web.env", Services: []string{"web"}},
		{Kind: "secret", Name: "pw.txt", Services: []string{"web"}, Target: "/run/secrets/pw", Secret: true},
	}
	if fmt.Sprint(files) != fmt.Sprint(want) {
		t.Errorf("ProjectFiles =\n%+v\nwant\n%+v", files, want)
	}

	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	ls, err := e.Status(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	vol := app.Name + "_data"
	if !slices.ContainsFunc(ls.Resources, func(r domain.Resource) bool { return r.Name == vol && slices.Equal(r.Services, []string{"web"}) }) {
		t.Errorf("Status resources = %+v, want %s used by web", ls.Resources, vol)
	}
	info, err := e.Inspect(ctx, app, "volume", vol)
	if err != nil {
		t.Fatal(err)
	}
	if info["driver"] != "local" || info["mountpoint"] == "" || !strings.Contains(info["containers"], app.Name+"-web-1") {
		t.Errorf("Inspect volume = %+v", info)
	}
	info, err = e.Inspect(ctx, app, "network", app.Name+"_default")
	if err != nil {
		t.Fatal(err)
	}
	if info["subnet"] == "" || !strings.Contains(info["containers"], app.Name+"-web-1") {
		t.Errorf("Inspect network = %+v", info)
	}
}
