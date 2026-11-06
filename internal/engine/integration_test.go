//go:build integration

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
)

const fixture = `
services:
  web:
    image: nginx:alpine
    environment:
      GREETING: ${GREETING}
      LEAK: ${OMEGACD_IT_SECRET:-none}
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
	app := domain.Application{Name: fmt.Sprintf("omegacd-it-%d", rand.IntN(1e9))}
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

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", fixture)
	write(t, dir, ".env", "GREETING=hi\n")
	t.Setenv("OMEGACD_IT_SECRET", "leaked")

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
			if c.Labels[domain.ManagedByLabel] != domain.ManagedByValue {
				t.Errorf("%s: missing managed-by=omegacd label", svc)
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
		if err := e.Logs(ctx, app, once, false, 0, &buf); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "hello-from-once") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err := e.Logs(ctx, domain.Application{Name: app.Name + "-other"}, once, false, 0, io.Discard); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Logs of another project's container = %v; want ErrNotFound", err)
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

	if err := e.Remove(ctx, app, false); err != nil {
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

	// Plain compose SDK, as `docker compose up` would do it: no omegacd label.
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
	// the container list can lag the stop call (seen on GitHub-hosted runners); wait until Docker reports it
	var state string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if ls, _ = e.Status(ctx, app); len(ls.Containers) == 1 {
			if state = ls.Containers[0].State; state != "running" {
				break
			}
		}
	}
	if diffs, _ := e.Diff(ctx, app, dir, ""); len(diffs) != 1 || diffs[0].Change != "stopped" {
		t.Fatalf("Diff = %+v (container state %q); want web stopped", diffs, state)
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

func TestUndeclaredServicesStoppedOrPruned(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", `
services:
  web:
    image: busybox
    command: ["sleep", "3600"]
  adminer:
    image: busybox
    command: ["sleep", "3600"]
    profiles: [debug]
    restart: unless-stopped
`)
	state := func() map[string]string {
		ls, _ := e.Status(ctx, app)
		m := map[string]string{}
		for _, c := range ls.Containers {
			m[c.Service] = c.State
		}
		return m
	}
	app.Profiles = []string{"debug"}
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}

	// profile switched off, no Prune: stopped, reported as removed+stopped
	app.Profiles = nil
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if s := state(); s["adminer"] != "exited" || s["web"] != "running" {
		t.Fatalf("without prune: %v", s)
	}
	// stopped on purpose: must not make the app Degraded despite its restart policy
	if ls, _ := e.Status(ctx, app); ls.Health != domain.Healthy {
		t.Fatalf("health with a stopped undeclared service = %s, want Healthy", ls.Health)
	}
	diffs, _ := e.Diff(ctx, app, dir, "")
	if len(diffs) != 1 || diffs[0].Change != "removed" || !slices.Contains(diffs[0].Fields, domain.FieldStopped) {
		t.Fatalf("diff without prune: %+v", diffs)
	}

	// with Prune: deleted, diff empty
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{Prune: true}); err != nil {
		t.Fatal(err)
	}
	if s := state(); len(s) != 1 || s["web"] != "running" {
		t.Fatalf("with prune: %v", s)
	}
	if diffs, _ := e.Diff(ctx, app, dir, ""); len(diffs) != 0 {
		t.Fatalf("diff after prune: %+v", diffs)
	}
}

func TestForceRecreateReplacesUnchangedContainers(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir := t.TempDir()
	write(t, dir, "compose.yaml", "services:\n  web:\n    image: busybox\n    command: [\"sleep\", \"3600\"]\n")
	id := func() string { ls, _ := e.Status(ctx, app); return ls.Containers[0].ID }
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	before := id()
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil || id() != before {
		t.Fatalf("plain re-sync recreated an unchanged container (err %v)", err)
	}
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{ForceRecreate: true}); err != nil || id() == before {
		t.Fatalf("ForceRecreate kept the old container (err %v)", err)
	}
}

func TestAbsoluteBindMountIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir, data := t.TempDir(), t.TempDir() // data: an absolute host path outside the app dir, like /opt/media
	write(t, dir, "compose.yaml", fmt.Sprintf(`
services:
  web:
    image: busybox
    command: ["sh", "-c", "echo from-container > /opt/data/hello.txt; sleep 3600"]
    volumes: ["%s:/opt/data"]
`, data))
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	p, err := e.load(ctx, app, dir)
	if err != nil {
		t.Fatal(err)
	}
	if src := p.Services["web"].Volumes[0].Source; src != data {
		t.Fatalf("bind source = %q, want the absolute path %q unchanged", src, data)
	}
	if _, ok := p.Services["web"].Labels[contentHashLabel]; ok {
		t.Fatal("a data directory must not be content-hashed")
	}
	// data written into the folder is visible on the host and is not drift
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(filepath.Join(data, "hello.txt")); err == nil && strings.TrimSpace(string(b)) == "from-container" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container did not write into the absolute bind mount")
		}
		time.Sleep(200 * time.Millisecond)
	}
	write(t, data, "more.bin", "runtime data")
	if diffs, _ := e.Diff(ctx, app, dir, ""); len(diffs) != 0 {
		t.Fatalf("Diff = %+v; files in a data folder must not count as drift", diffs)
	}
	// a re-Sync keeps the data
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{ForceRecreate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "more.bin")); err != nil {
		t.Fatal("data lost after re-Sync")
	}
}

// exists reports whether a Docker volume or network exists.
func exists(t *testing.T, kind, name string) bool {
	t.Helper()
	_, err := dockerCmd(kind, "inspect", name)
	return err == nil
}

func undeclaredDiffs(t *testing.T, e *Engine, app domain.Application, dir string) []string {
	t.Helper()
	diffs, err := e.Diff(context.Background(), app, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range diffs {
		if d.Kind != "" {
			if d.Change != "removed" {
				t.Errorf("undeclared %s %s: change %q", d.Kind, d.Service, d.Change)
			}
			out = append(out, d.Kind+" "+d.Service)
		} else {
			t.Errorf("unexpected service diff %+v", d)
		}
	}
	return out
}

func TestPruneSkipsNetworkInUse(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app, other := testApp(t, e), testApp(t, e)
	dir, odir := t.TempDir(), t.TempDir()
	write(t, dir, "compose.yaml", "services:\n  web:\n    image: busybox\n    command: [\"sleep\", \"3600\"]\n    networks: [front]\nnetworks:\n  front: {}\n")
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	old := app.Name + "_front"
	// a container of another project joins the app's network
	write(t, odir, "compose.yaml", fmt.Sprintf("services:\n  peer:\n    image: busybox\n    command: [\"sleep\", \"3600\"]\n    networks: [shared]\nnetworks:\n  shared:\n    external: true\n    name: %s\n", old))
	if err := e.Sync(ctx, other, odir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}

	write(t, dir, "compose.yaml", "services:\n  web:\n    image: busybox\n    command: [\"sleep\", \"3600\"]\n    networks: [edge]\nnetworks:\n  edge: {}\n")
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{Prune: true}); err != nil {
		t.Fatalf("Sync with a network in use: %v", err)
	}
	if !exists(t, "network", old) {
		t.Fatal("network in use by another project was deleted")
	}
	if got := undeclaredDiffs(t, e, app, dir); !slices.Equal(got, []string{"network " + old}) {
		t.Fatalf("undeclared = %v", got)
	}

	if err := e.Remove(ctx, other, false); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{Prune: true}); err != nil {
		t.Fatal(err)
	}
	if exists(t, "network", old) {
		t.Fatal("unused undeclared network survived Prune")
	}
	if got := undeclaredDiffs(t, e, app, dir); len(got) != 0 {
		t.Fatalf("undeclared after prune = %v", got)
	}
}

func TestPruneVolumes(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	dir, data := t.TempDir(), t.TempDir()
	write(t, data, "keep.txt", "host data")
	ext := app.Name + "-ext"
	if out, err := dockerCmd("volume", "create", ext); err != nil {
		t.Fatal(out)
	}
	// runs before testApp's cleanup: take the project down first, or the volume is still in use
	t.Cleanup(func() { e.Remove(context.Background(), app, true); dockerCmd("volume", "rm", "-f", ext) })
	compose := func(withData bool) {
		mounts, decl := "", ""
		if withData {
			mounts, decl = ", \"data:/data\"", "  data: {}\n"
		}
		write(t, dir, "compose.yaml", fmt.Sprintf(`
services:
  web:
    image: busybox
    command: ["sleep", "3600"]
    volumes: ["ext:/ext", "%s:/host"%s]
volumes:
  ext:
    external: true
    name: %s
%s`, data, mounts, ext, decl))
	}
	compose(true)
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	vol := app.Name + "_data"
	if !exists(t, "volume", vol) {
		t.Fatal("data volume not created")
	}

	compose(false)
	// Prune alone, or PruneVolumes without Prune, keeps it: prune pending
	for _, opts := range []domain.SyncOptions{{Prune: true}, {PruneVolumes: true}} {
		if err := e.Sync(ctx, app, dir, opts); err != nil {
			t.Fatal(err)
		}
		if !exists(t, "volume", vol) {
			t.Fatalf("%+v deleted the volume", opts)
		}
	}
	if got := undeclaredDiffs(t, e, app, dir); !slices.Equal(got, []string{"volume " + vol}) {
		t.Fatalf("undeclared = %v", got)
	}

	// in use by any container, even a stopped one of no project: kept
	user, err := dockerCmd("create", "-v", vol+":/x", "busybox")
	if err != nil {
		t.Fatal(user)
	}
	t.Cleanup(func() { dockerCmd("rm", "-f", user) })
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{Prune: true, PruneVolumes: true}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, "volume", vol) {
		t.Fatal("volume in use was deleted")
	}
	dockerCmd("rm", "-f", user)

	if err := e.Sync(ctx, app, dir, domain.SyncOptions{Prune: true, PruneVolumes: true}); err != nil {
		t.Fatal(err)
	}
	if exists(t, "volume", vol) {
		t.Fatal("undeclared volume survived PruneVolumes")
	}
	if !exists(t, "volume", ext) {
		t.Fatal("external volume deleted")
	}
	if b, err := os.ReadFile(filepath.Join(data, "keep.txt")); err != nil || string(b) != "host data" {
		t.Fatalf("bind-mounted host dir touched: %q, %v", b, err)
	}
	if got := undeclaredDiffs(t, e, app, dir); len(got) != 0 {
		t.Fatalf("undeclared after prune = %v", got)
	}
}

func TestRemoveVolumes(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	dir, data := t.TempDir(), t.TempDir()
	write(t, data, "keep.txt", "host data")
	write(t, dir, "compose.yaml", fmt.Sprintf("services:\n  web:\n    image: busybox\n    command: [\"sleep\", \"3600\"]\n    volumes: [\"data:/data\", \"%s:/host\"]\nvolumes:\n  data: {}\n", data))
	for _, volumes := range []bool{false, true} {
		app := testApp(t, e)
		if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := e.Remove(ctx, app, volumes); err != nil {
			t.Fatal(err)
		}
		if got := exists(t, "volume", app.Name+"_data"); got == volumes {
			t.Errorf("Remove(volumes=%v): volume exists = %v", volumes, got)
		}
		if _, err := os.Stat(filepath.Join(data, "keep.txt")); err != nil {
			t.Fatalf("Remove(volumes=%v) touched the bind-mounted host dir: %v", volumes, err)
		}
	}
}
