package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
)

// unitEngine is an Engine that never reaches its engine: what these tests call fails or finishes before asking it.
func unitEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// What can't be loaded is refused before the engine is asked anything: by Validate, Sync, Diff and ProjectFiles alike;
// a file a service reads that isn't there is named.
func TestLoadFailures(t *testing.T) {
	e := unitEngine(t)
	ctx := t.Context()
	app := domain.Application{Name: "p"}
	for want, files := range map[string]map[string]string{
		`config "c:"`:               {"compose.yaml": "services:\n  a:\n    image: x\n    configs: [c]\nconfigs:\n  c:\n    file: ./nope.txt\n"},
		`secret "c:/run/secrets/c"`: {"compose.yaml": "services:\n  a:\n    image: x\n    secrets: [c]\nsecrets:\n  c:\n    file: ./nope.txt\n"},
		"unterminated quoted value": {"compose.yaml": "services:\n  a:\n    image: x\n", ".env": "A='unterminated\n"},
	} {
		dir := project(t, files)
		if err := e.Validate(ctx, app, dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate: %v, want %q", err, want)
		}
		if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err == nil {
			t.Errorf("Sync of %q", want)
		}
		if _, err := e.Diff(ctx, app, dir, ""); err == nil {
			t.Errorf("Diff of %q", want)
		}
		if _, err := e.ProjectFiles(ctx, app, dir); err == nil {
			t.Errorf("ProjectFiles of %q", want)
		}
	}
	if os.Getuid() != 0 { // root reads anything; the suite runs as a normal user
		dir := project(t, map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n    volumes: [./conf.txt:/conf.txt]\n", "conf.txt": "x"})
		os.Chmod(filepath.Join(dir, "conf.txt"), 0)
		if err := e.Validate(ctx, app, dir); err == nil || !strings.Contains(err.Error(), `bind "/conf.txt"`) {
			t.Errorf("an unreadable bind-mounted file: %v", err)
		}
	}
	dir := project(t, map[string]string{"compose.yaml": "services:\n  a:\n    build: .\n"})
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err == nil || !strings.Contains(err.Error(), `service "a" uses build:`) {
		t.Errorf("a build: %v", err)
	}
	if _, err := e.Inspect(ctx, app, "image", "x"); err == nil || !strings.Contains(err.Error(), `cannot inspect kind "image"`) {
		t.Errorf("an unknown kind: %v", err)
	}
}

// A secret is mounted under /run/secrets unless its target is absolute, as Compose does.
func TestProjectFilesSecretTargets(t *testing.T) {
	dir := project(t, map[string]string{"key.txt": "k", "cert.txt": "c",
		"compose.yaml": "services:\n  a:\n    image: x\n    secrets: [key, {source: cert, target: /etc/app/cert}, {source: cert, target: app/cert}]\n" +
			"secrets:\n  key:\n    file: ./key.txt\n  cert:\n    file: ./cert.txt\n"})
	files, err := unitEngine(t).ProjectFiles(t.Context(), domain.Application{Name: "p"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, f := range files {
		got[f.Name] = append(got[f.Name], f.Target)
	}
	if !slices.Equal(got["key.txt"], []string{"/run/secrets/key"}) || !slices.Equal(got["cert.txt"], []string{"/etc/app/cert"}) {
		t.Fatalf("targets %v", got)
	}
}

// rebase moves every path inside the staging dir to the app's folder, configs' and secrets' files too; one outside
// it stays as it is.
func TestRebase(t *testing.T) {
	a := types.ServiceConfig{Name: "a"}
	a.Volumes = []types.ServiceVolumeConfig{{Type: types.VolumeTypeBind, Source: "/stage/web/conf.txt"}, {Type: types.VolumeTypeBind, Source: "/srv/data"}}
	p := &types.Project{WorkingDir: "/stage/web", Services: types.Services{"a": a},
		Configs: types.Configs{"c": {File: "/stage/web/c.txt"}, "inline": {Content: "x"}},
		Secrets: types.Secrets{"s": {File: "/stage/web/s.txt"}}}
	rebase(p, "/stage/web", "/work/web")
	v := p.Services["a"].Volumes
	if p.WorkingDir != "/work/web" || v[0].Source != "/work/web/conf.txt" || v[1].Source != "/srv/data" ||
		p.Configs["c"].File != "/work/web/c.txt" || p.Configs["inline"].File != "" || p.Secrets["s"].File != "/work/web/s.txt" {
		t.Fatalf("%+v", p)
	}
}

// volumes_from: container:x is taken as is, a service by its first container in Compose's order (the lowest
// canonical name), and a service without a container can't be resolved.
func TestResolveRefsVolumesFrom(t *testing.T) {
	var s types.ServiceConfig
	s.VolumesFrom = []string{"container:abc", "db"}
	by := map[string][]container.Summary{"db": {
		{ID: "two", Names: []string{"/p-db-2"}},
		{ID: "one", Names: []string{"/web/db", "/p-db-1"}}, // a link alias first
	}}
	got, ok := resolveRefs(s, by)
	if !ok || got.VolumesFrom[0] != "abc" || got.VolumesFrom[1] != "one" {
		t.Fatalf("%v, %v", got.VolumesFrom, ok)
	}
	s.VolumesFrom = []string{"cache"}
	if _, ok := resolveRefs(s, by); ok {
		t.Fatal("a service without a container resolved")
	}
	if n := canonicalName(container.Summary{ID: "0123456789abcdef"}); n != "0123456789ab" {
		t.Errorf("no names: %q", n)
	}
	if n := canonicalName(container.Summary{Names: []string{"/web/db"}}); n != "web/db" {
		t.Errorf("only a link alias: %q", n)
	}
}

// A service counts as on the project's networks and volumes only when its container is: network_mode host is on
// none to check; a container without network settings, or missing a network or volume, isn't.
func TestOnProjectNetworksAndVolumes(t *testing.T) {
	p := &types.Project{Networks: types.Networks{"default": {Name: "p_default"}}, Volumes: types.Volumes{"data": {Name: "p_data"}}}
	var svc types.ServiceConfig
	svc.Networks = map[string]*types.ServiceNetworkConfig{"default": nil}
	on := container.Summary{NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{"p_default": {}}}}
	off := container.Summary{NetworkSettings: &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{"bridge": {}}}}
	if !onProjectNetworks(p, svc, on) || onProjectNetworks(p, svc, off) || onProjectNetworks(p, svc, container.Summary{}) {
		t.Error("networks")
	}
	host := svc
	host.NetworkMode = "host"
	if !onProjectNetworks(p, host, container.Summary{}) {
		t.Error("network_mode host")
	}
	svc.Volumes = []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: "data"}, {Type: types.VolumeTypeVolume}}
	if onProjectVolumes(p, svc, on) || !onProjectVolumes(p, svc, container.Summary{Mounts: []container.MountPoint{{Type: "volume", Name: "p_data"}}}) {
		t.Error("volumes")
	}
}

// Progress is per resource: layer progress (a parent) is left out, a resource without text says its status.
func TestProgressSink(t *testing.T) {
	var got []domain.ProgressEvent
	(&progressSink{}).On(api.Resource{ID: "nothing to tell"}) // no one listening
	(&progressSink{fn: func(e domain.ProgressEvent) { got = append(got, e) }}).On(
		api.Resource{ID: "Image x", ParentID: "", Text: "Pulling"},
		api.Resource{ID: "layer", ParentID: "Image x", Text: "Downloading"},
		api.Resource{ID: "Container web", Status: api.Done})
	if len(got) != 2 || got[0].Status != "Pulling" || got[1].Status == "" {
		t.Fatalf("%+v", got)
	}
}

// The hash keeps the historical key order; a key the order doesn't list (a compose-go field added later) comes after,
// sorted, and what isn't a JSON object is refused.
func TestPinRootKeyOrder(t *testing.T) {
	got, err := pinRootKeyOrder([]byte(`{"z":1,"image":"x","a":2}`), []string{"image"})
	if err != nil || string(got) != `{"image":"x","a":2,"z":1}` {
		t.Fatalf("%s, %v", got, err)
	}
	if _, err := pinRootKeyOrder([]byte(`[]`), nil); err == nil {
		t.Fatal("not an object")
	}
}

// fakeCLI is the docker CLI with the engine client a test gives.
type fakeCLI struct {
	command.Cli
	c client.APIClient
}

func (f fakeCLI) Client() client.APIClient { return f.c }

// resourcesClient lists containers as listClient does, and no networks or volumes.
type resourcesClient struct{ listClient }

func (resourcesClient) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	return client.NetworkListResult{}, nil
}

func (resourcesClient) VolumeList(context.Context, client.VolumeListOptions) (client.VolumeListResult, error) {
	return client.VolumeListResult{}, nil
}

// A service sharing another's network whose container is gone shows as changed: Compose recreates it with the other.
func TestDiffWithoutTheReferencedContainer(t *testing.T) {
	dir := project(t, map[string]string{"compose.yaml": "services:\n  db:\n    image: x\n  web:\n    image: x\n    network_mode: service:db\n"})
	web := container.Summary{ID: "0123456789ab", Labels: map[string]string{api.ServiceLabel: "web"}, State: "running"}
	e := &Engine{cfg: Config{Log: logging.Discard}, desired: map[string][]string{},
		hc: &hostConn{cli: fakeCLI{c: resourcesClient{listClient{items: []container.Summary{web}}}}}}
	diffs, err := e.Diff(t.Context(), domain.Application{Name: "p"}, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(diffs, func(d domain.ServiceDiff) bool { return d.Service == "web" && d.Change == "changed" }) {
		t.Fatalf("%+v", diffs)
	}
}

// eventsClient answers the ping, and its event stream fails at once.
type eventsClient struct{ resourcesClient }

func (eventsClient) Ping(context.Context, client.PingOptions) (client.PingResult, error) {
	return client.PingResult{}, nil
}

func (eventsClient) Events(context.Context, client.EventsListOptions) client.EventsResult {
	errs := make(chan error, 1)
	errs <- errors.New("stream broken off")
	return client.EventsResult{Messages: make(chan events.Message), Err: errs}
}

// A live event stream that breaks (the engine restarting) ends with its error, for Watch to say and retry.
func TestEventStreamBreaks(t *testing.T) {
	e := &Engine{cfg: Config{Log: logging.Discard}, hc: &hostConn{cli: fakeCLI{c: eventsClient{}}}}
	connected := false
	err := e.streamEvents(t.Context(), client.Filters{}, func(string) {}, func() { connected = true })
	if err == nil || err.Error() != "stream broken off" || !connected {
		t.Fatalf("%v, connected %v", err, connected)
	}
}

// Q179: a container's start or finish time as Docker writes it; "never" (0001-01-01) and a format it can't read are
// zero, "not known".
func TestDockerTime(t *testing.T) {
	if got := dockerTime("2026-11-17T18:05:43.436682601Z"); !got.Equal(time.Date(2026, 11, 17, 18, 5, 43, 436682601, time.UTC)) {
		t.Errorf("a start time: %v", got)
	}
	for _, s := range []string{"0001-01-01T00:00:00Z", "", "yesterday"} {
		if got := dockerTime(s); !got.IsZero() {
			t.Errorf("%q: %v", s, got)
		}
	}
}
