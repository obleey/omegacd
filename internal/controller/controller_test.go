package controller

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/domain"
)

// ---- fakes ----

type fakeStore struct {
	domain.Store // unimplemented methods panic
	mu           sync.Mutex
	apps         map[string]domain.Application
	ops          []domain.SyncOperation
	hosts        map[string]domain.Host
}

func newStore() *fakeStore {
	return &fakeStore{apps: map[string]domain.Application{}, hosts: map[string]domain.Host{}}
}

func (s *fakeStore) ListHosts(context.Context) ([]domain.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Values(s.hosts)), nil
}
func (s *fakeStore) GetHost(_ context.Context, n string) (domain.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hosts[n]
	if !ok {
		return h, domain.ErrNotFound
	}
	return h, nil
}
func (s *fakeStore) SaveHost(_ context.Context, h domain.Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[h.Name] = h
	return nil
}
func (s *fakeStore) DeleteHost(_ context.Context, n string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hosts, n)
	return nil
}

func (s *fakeStore) ListApps(context.Context) ([]domain.Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Application
	for _, a := range s.apps {
		out = append(out, a)
	}
	return out, nil
}
func (s *fakeStore) GetApp(_ context.Context, n string) (domain.Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[n]
	if !ok {
		return a, domain.ErrNotFound
	}
	return a, nil
}
func (s *fakeStore) CreateApp(_ context.Context, a domain.Application) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[a.Name]; ok {
		return domain.ErrAlreadyExists
	}
	s.apps[a.Name] = a
	return nil
}
func (s *fakeStore) UpdateApp(_ context.Context, a domain.Application) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apps[a.Name] = a
	return nil
}
func (s *fakeStore) DeleteApp(_ context.Context, n string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.apps, n)
	return nil
}
func (s *fakeStore) CreateOp(_ context.Context, op domain.SyncOperation) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op.ID = int64(len(s.ops) + 1)
	s.ops = append(s.ops, op)
	return op.ID, nil
}
func (s *fakeStore) FinishOp(_ context.Context, id int64, p domain.OpPhase, m string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops[id-1].Phase, s.ops[id-1].Message, s.ops[id-1].FinishedAt = p, m, time.Now()
	return nil
}
func (s *fakeStore) GetOp(_ context.Context, id int64) (domain.SyncOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 1 || int(id) > len(s.ops) {
		return domain.SyncOperation{}, domain.ErrNotFound
	}
	return s.ops[id-1], nil
}
func (s *fakeStore) ListOps(_ context.Context, app string) ([]domain.SyncOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.SyncOperation
	for _, op := range slices.Backward(s.ops) {
		if op.App == app {
			out = append(out, op)
		}
	}
	return out, nil
}
func (s *fakeStore) LastSucceededOp(ctx context.Context, app string) (domain.SyncOperation, error) {
	ops, _ := s.ListOps(ctx, app)
	for _, op := range ops {
		if op.Phase == domain.OpSucceeded {
			return op, nil
		}
	}
	return domain.SyncOperation{}, domain.ErrNotFound
}
func (s *fakeStore) ListRegistryCredentials(context.Context) ([]domain.RegistryCredential, error) {
	return nil, nil
}

type fakeGit struct {
	mu       sync.Mutex
	sha      string
	tree     string
	err      error
	resolves int
	files    map[string]string // exported instead of a compose.yaml when set
}

func (g *fakeGit) Invalidate(string) {}
func (g *fakeGit) Resolve(context.Context, string, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolves++
	return g.sha, g.err
}
func (g *fakeGit) TreeHash(_ context.Context, _, sha, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return "tree-" + sha, nil
}
func (g *fakeGit) Export(_ context.Context, _, _, _, dest string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	// definitions belong to the App of Apps (named "root" in the tests); every other app gets a compose file
	if g.files == nil || filepath.Base(dest) != "root" {
		return os.WriteFile(filepath.Join(dest, "compose.yaml"), []byte("services: {}\n"), 0o644)
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	os.MkdirAll(dest, 0o750)
	for name, content := range g.files {
		if err := os.WriteFile(filepath.Join(dest, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
func (g *fakeGit) Diff(context.Context, string, string, string, string) (string, error) {
	return "", nil
}
func (g *fakeGit) CheckAccess(_ context.Context, url string) (domain.RepoAccess, error) {
	return domain.RepoAccess{Access: "public", Message: url}, nil
}
func (g *fakeGit) set(sha string) { g.mu.Lock(); g.sha = sha; g.mu.Unlock() }

// define makes the next Revision hold one definitions file.
func (g *fakeGit) define(sha, defs string) {
	g.mu.Lock()
	g.sha, g.files = sha, map[string]string{"apps.yaml": defs}
	g.mu.Unlock()
}

type fakeSecrets struct{}

func (fakeSecrets) DecryptDir(string) ([]string, error) { return nil, nil }

type fakeEngine struct {
	mu           sync.Mutex
	syncErr      error
	syncs        int
	drift        []domain.ServiceDiff
	exists       bool
	managed      bool
	live         domain.LiveState
	removed      bool
	removedApps  []string
	health       map[string]domain.HealthStatus // per app, overrides live.Health
	changed      func(string, string)
	files        []domain.Resource
	hostsChanged []string
	lastPrune    bool
	lastForce    bool
	onRemove     func(domain.Application) // runs after Remove, outside the lock: simulates Docker events mid-delete
}

func (e *fakeEngine) Validate(context.Context, domain.Application, string) error { return nil }
func (e *fakeEngine) Diff(context.Context, domain.Application, string, string) ([]domain.ServiceDiff, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.drift, nil
}
func (e *fakeEngine) Sync(_ context.Context, _ domain.Application, _ string, opts domain.SyncOptions) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.syncs++
	e.lastPrune = opts.Prune
	e.lastForce = opts.ForceRecreate
	return e.syncErr
}
func (e *fakeEngine) Remove(_ context.Context, a domain.Application) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = true
	e.removedApps = append(e.removedApps, a.Name)
	if hook := e.onRemove; hook != nil {
		e.mu.Unlock()
		hook(a)
		e.mu.Lock()
	}
	return nil
}
func (e *fakeEngine) Status(_ context.Context, a domain.Application) (domain.LiveState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	live := e.live
	if h, ok := e.health[a.Name]; ok {
		live.Health = h
	}
	return live, nil
}
func (e *fakeEngine) ProjectExists(context.Context, string, string) (bool, bool, error) {
	return e.exists, e.managed, nil
}
func (e *fakeEngine) HostStatus(_ context.Context, host string) domain.HostStatus {
	return domain.HostStatus{Name: host, Connected: true}
}
func (e *fakeEngine) HostChanged(host string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hostsChanged = append(e.hostsChanged, host)
}
func (e *fakeEngine) ProjectFiles(context.Context, domain.Application, string) ([]domain.Resource, error) {
	return slices.Clone(e.files), nil
}
func (e *fakeEngine) Inspect(_ context.Context, _ domain.Application, kind, name string) (map[string]string, error) {
	return map[string]string{"inspected": kind + "/" + name}, nil
}
func (e *fakeEngine) Logs(context.Context, string, string, bool, int, io.Writer) error { return nil }
func (e *fakeEngine) Watch(ctx context.Context, changed func(string, string)) error {
	e.mu.Lock()
	e.changed = changed
	e.mu.Unlock()
	<-ctx.Done()
	return nil
}
func (e *fakeEngine) Close() error   { return nil }
func (e *fakeEngine) syncCount() int { e.mu.Lock(); defer e.mu.Unlock(); return e.syncs }

type rig struct {
	c  *Controller
	st *fakeStore
	g  *fakeGit
	e  *fakeEngine
}

func newRig(t *testing.T) rig {
	t.Helper()
	r := rig{st: newStore(), g: &fakeGit{sha: "aaaaaaaa"}, e: &fakeEngine{live: domain.LiveState{Health: domain.Healthy}}}
	r.c = New(Config{Store: r.st, Git: r.g, Secrets: fakeSecrets{}, Engine: r.e, WorkDir: t.TempDir(), PollInterval: time.Hour})
	return r
}

func (r rig) waitIdle(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.c.mu.Lock()
		s := r.c.apps[name]
		busy := s != nil && s.syncing
		r.c.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sync did not finish")
}

func app(name string) domain.Application {
	return domain.Application{Name: name, RepoURL: "https://example.com/r.git"}
}

// ---- tests ----

func TestCreateDefaultsAndStatus(t *testing.T) {
	r := newRig(t)
	v, err := r.c.CreateApp(t.Context(), app("web"))
	if err != nil {
		t.Fatal(err)
	}
	if v.TargetRevision != "main" || !slices.Equal(v.ComposeFiles, []string{"compose.yaml"}) || v.Host != domain.LocalHost {
		t.Fatalf("defaults not applied: %+v", v.Application)
	}
	if v.SyncStatus != domain.OutOfSync || v.SyncMessage != "never synced" {
		t.Fatalf("got %s %q", v.SyncStatus, v.SyncMessage)
	}
}

func TestCreateValidation(t *testing.T) {
	r := newRig(t)
	for _, a := range []domain.Application{
		{Name: "Bad Name", RepoURL: "x"},
		{Name: "ok"},
		{Name: "ok", RepoURL: "x", Path: "../etc"},
		{Name: "ok", RepoURL: "x", ComposeFiles: []string{"/abs.yaml"}},
	} {
		if _, err := r.c.CreateApp(t.Context(), a); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", a, err)
		}
	}
}

func TestCreateRefusesUnmanagedUnlessAdopt(t *testing.T) {
	r := newRig(t)
	r.e.exists = true
	if _, err := r.c.CreateApp(t.Context(), app("web")); !errors.Is(err, domain.ErrUnmanaged) {
		t.Fatalf("want ErrUnmanaged, got %v", err)
	}
	a := app("web")
	a.Adopt = true
	if _, err := r.c.CreateApp(t.Context(), a); err != nil {
		t.Fatal(err)
	}
}

func TestManualSyncThenSyncedThenNewRevisionAndDrift(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, app("web"))
	if _, err := r.c.Sync(ctx, "web", domain.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	v, _ := r.c.Refresh(ctx, "web")
	if v.SyncStatus != domain.Synced || v.DeployedSHA != "aaaaaaaa" {
		t.Fatalf("after sync: %s %q deployed=%s", v.SyncStatus, v.SyncMessage, v.DeployedSHA)
	}

	r.e.mu.Lock()
	r.e.drift = []domain.ServiceDiff{{Service: "web", Change: "changed"}}
	r.e.mu.Unlock()
	if v, _ = r.c.Refresh(ctx, "web"); v.SyncStatus != domain.OutOfSync {
		t.Fatalf("drift: got %s", v.SyncStatus)
	}

	r.g.set("bbbbbbbb")
	if v, _ = r.c.Refresh(ctx, "web"); v.SyncStatus != domain.OutOfSync || v.SyncMessage != "new revision bbbbbbb" {
		t.Fatalf("new revision: got %s %q", v.SyncStatus, v.SyncMessage)
	}
}

func TestGitErrorIsUnknownAndNeverAutoSyncs(t *testing.T) {
	r := newRig(t)
	r.g.err = errors.New("unreachable")
	a := app("web")
	a.AutoSync = true
	v, _ := r.c.CreateApp(t.Context(), a)
	if v.SyncStatus != domain.Unknown || v.SyncMessage != "unreachable" {
		t.Fatalf("got %s %q", v.SyncStatus, v.SyncMessage)
	}
	if r.e.syncCount() != 0 {
		t.Fatal("synced while Unknown")
	}
}

func TestAutoSyncAndSelfHeal(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	a := app("web")
	a.AutoSync = true
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")
	if r.e.syncCount() != 1 {
		t.Fatalf("auto-sync: syncs=%d", r.e.syncCount())
	}

	// drift without Self-Heal: no sync
	r.e.mu.Lock()
	r.e.drift = []domain.ServiceDiff{{Service: "web", Change: "changed"}}
	r.e.mu.Unlock()
	r.c.Refresh(ctx, "web")
	r.waitIdle(t, "web")
	if r.e.syncCount() != 1 {
		t.Fatalf("synced drift without Self-Heal")
	}

	a.SelfHeal = true
	r.c.UpdateApp(ctx, a)
	r.waitIdle(t, "web")
	if r.e.syncCount() != 2 {
		t.Fatalf("self-heal: syncs=%d", r.e.syncCount())
	}
	if ops, _ := r.st.ListOps(ctx, "web"); ops[0].Trigger != "self-heal" {
		t.Fatalf("trigger=%s", ops[0].Trigger)
	}
}

func TestAutoSyncRetriesThenGivesUpUntilNewRevision(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.e.syncErr = errors.New("pull failed")
	a := app("web")
	a.AutoSync = true
	r.c.CreateApp(ctx, a)
	for i := 1; i < maxRetries; i++ {
		r.waitIdle(t, "web")
		r.c.mu.Lock()
		s := r.c.apps["web"]
		if s.retries != i {
			r.c.mu.Unlock()
			t.Fatalf("attempt %d: retries=%d", i, s.retries)
		}
		s.nextRetry = time.Now().Add(-time.Second) // skip the backoff wait
		r.c.mu.Unlock()
		r.c.Refresh(ctx, "web")
	}
	r.waitIdle(t, "web")
	if r.e.syncCount() != maxRetries {
		t.Fatalf("syncs=%d want %d", r.e.syncCount(), maxRetries)
	}
	r.c.Refresh(ctx, "web")
	r.waitIdle(t, "web")
	if r.e.syncCount() != maxRetries {
		t.Fatal("kept retrying after giving up")
	}

	r.e.mu.Lock()
	r.e.syncErr = nil
	r.e.mu.Unlock()
	r.g.set("bbbbbbbb")
	r.c.Refresh(ctx, "web")
	r.waitIdle(t, "web")
	if r.e.syncCount() != maxRetries+1 {
		t.Fatal("new revision did not trigger auto-sync")
	}
}

func TestRollbackDisablesAutoSync(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	a := app("web")
	a.AutoSync = true
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")
	r.g.set("bbbbbbbb")
	r.c.Refresh(ctx, "web")
	r.waitIdle(t, "web")

	if _, err := r.c.Rollback(ctx, "web", 1); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	got, _ := r.st.GetApp(ctx, "web")
	if got.AutoSync || !got.RolledBack {
		t.Fatalf("after rollback: autoSync=%v rolledBack=%v", got.AutoSync, got.RolledBack)
	}
	v, _ := r.c.Refresh(ctx, "web")
	if v.DeployedSHA != "aaaaaaaa" || v.SyncStatus != domain.OutOfSync {
		t.Fatalf("deployed=%s status=%s", v.DeployedSHA, v.SyncStatus)
	}
	syncs := r.e.syncCount()

	got.AutoSync = true
	v, _ = r.c.UpdateApp(ctx, got)
	r.waitIdle(t, "web")
	if v.RolledBack || r.e.syncCount() != syncs+1 {
		t.Fatalf("re-enabling auto-sync: rolledBack=%v syncs=%d", v.RolledBack, r.e.syncCount())
	}
}

func TestRollbackRejectsFailedOrForeignOp(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, app("web"))
	r.st.CreateOp(ctx, domain.SyncOperation{App: "web", Phase: domain.OpFailed})
	r.st.CreateOp(ctx, domain.SyncOperation{App: "other", Phase: domain.OpSucceeded})
	for _, id := range []int64{1, 2} {
		if _, err := r.c.Rollback(ctx, "web", id); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("op %d: want ErrInvalid, got %v", id, err)
		}
	}
}

func TestSyncRefusesSelf(t *testing.T) {
	r := newRig(t)
	r.c.cfg.SelfID = "abc123"
	r.e.live.Containers = []domain.Container{{ID: "abc123def456"}}
	r.c.CreateApp(t.Context(), app("omegacd"))
	r.c.Sync(t.Context(), "omegacd", domain.SyncRequest{})
	r.waitIdle(t, "omegacd")
	if ops, _ := r.st.ListOps(t.Context(), "omegacd"); ops[0].Phase != domain.OpFailed || r.e.syncCount() != 0 {
		t.Fatalf("self sync not refused: %+v", ops[0])
	}
}

func TestDeleteCascade(t *testing.T) {
	r := newRig(t)
	r.c.CreateApp(t.Context(), app("web"))
	events, cancel := r.c.Subscribe()
	defer cancel()
	if err := r.c.DeleteApp(t.Context(), "web", true); err != nil {
		t.Fatal(err)
	}
	if !r.e.removed {
		t.Fatal("cascade did not remove containers")
	}
	if e := <-events; e.Type != "app-deleted" {
		t.Fatalf("event %s", e.Type)
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 3 * time.Minute}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d)=%v want %v", i+1, got, w)
		}
	}
}

func TestDockerEventsRefreshHealthWithoutGitFetch(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.c.CreateApp(ctx, app("web"))
	go r.c.watch(ctx)
	events, unsub := r.c.Subscribe()
	defer unsub()

	var changed func(string, string)
	for changed == nil {
		r.e.mu.Lock()
		changed = r.e.changed
		r.e.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	r.g.mu.Lock()
	before := r.g.resolves
	r.g.mu.Unlock()
	r.e.mu.Lock()
	r.e.live.Health = domain.Degraded
	r.e.mu.Unlock()
	for range 3 { // a burst of events is debounced into one reconcile
		changed(domain.LocalHost, "web")
	}
	changed(domain.LocalHost, "not-an-app")

	select {
	case e := <-events:
		if v := e.Data.(domain.AppView); v.Health != domain.Degraded {
			t.Fatalf("health = %s, want Degraded", v.Health)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no app event after docker event")
	}
	r.g.mu.Lock()
	if r.g.resolves != before {
		t.Fatalf("docker event caused %d git resolves", r.g.resolves-before)
	}
	r.g.mu.Unlock()

	changed("remote", "web") // same project name on another Host
	select {
	case e := <-events:
		t.Fatalf("event for another host's project reconciled the app: %+v", e)
	case <-time.After(watchDebounce + 500*time.Millisecond):
	}
}

func TestAppHostMustExistAndCannotChange(t *testing.T) {
	r := newRig(t)
	a := app("web")
	a.Host = "prod"
	if _, err := r.c.CreateApp(t.Context(), a); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unknown host: want ErrInvalid, got %v", err)
	}
	if _, err := r.c.SaveHost(t.Context(), domain.Host{Name: "prod", URL: "ssh://deploy@prod"}, true); err != nil {
		t.Fatal(err)
	}
	if v, err := r.c.CreateApp(t.Context(), a); err != nil || v.Host != "prod" {
		t.Fatalf("got %v %v", v.Host, err)
	}
	a.Host = domain.LocalHost
	if _, err := r.c.UpdateApp(t.Context(), a); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("host change: want ErrInvalid, got %v", err)
	}
}

func TestHosts(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	for _, h := range []domain.Host{
		{Name: domain.LocalHost, URL: "tcp://x"},
		{Name: "Bad Name", URL: "tcp://x"},
		{Name: "prod", URL: "http://x"},
	} {
		if _, err := r.c.SaveHost(ctx, h, true); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", h, err)
		}
	}
	prod := domain.Host{Name: "prod", URL: "tcp://prod:2376"}
	if _, err := r.c.SaveHost(ctx, prod, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update missing: got %v", err)
	}
	if _, err := r.c.SaveHost(ctx, prod, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.SaveHost(ctx, prod, true); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("create twice: got %v", err)
	}
	a := app("web")
	a.Host = "prod"
	if _, err := r.c.CreateApp(ctx, a); err != nil {
		t.Fatal(err)
	}

	hosts, err := r.c.ListHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || hosts[0].Name != domain.LocalHost || hosts[1].Name != "prod" ||
		!slices.Equal(hosts[1].Apps, []string{"web"}) || !hosts[1].Status.Connected {
		t.Fatalf("got %+v", hosts)
	}

	if err := r.c.DeleteHost(ctx, "prod"); !errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), "web") {
		t.Fatalf("delete in use: got %v", err)
	}
	if err := r.c.DeleteApp(ctx, "web", false); err != nil {
		t.Fatal(err)
	}
	if err := r.c.DeleteHost(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.e.hostsChanged, []string{"prod", "prod"}) {
		t.Fatalf("HostChanged calls: %v", r.e.hostsChanged)
	}
}

func TestResourceDetail(t *testing.T) {
	r := newRig(t)
	r.st.apps["web"] = app("web")
	dir := r.c.appDir("web")
	outside := filepath.Join(r.c.cfg.WorkDir, "apps", "outside.txt")
	for name, content := range map[string]string{
		"plain.env": "A=1\n", "db.env": "USER=root\n# PASSWORD=hunter2\nexport PASSWORD=hunter2\nCERT=\"line1\nline2\"\n", "db.sops.env": "ENC[...]",
		"app.yaml": "token: abc", "app.sops.yaml": "ENC[...]", "pw.txt": "hunter2",
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(outside, []byte("nope"), 0o600)
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	r.e.live.Resources = []domain.Resource{{Kind: "volume", Name: "web_data"}}
	r.e.files = []domain.Resource{
		{Kind: "env_file", Name: "plain.env"}, {Kind: "env_file", Name: "db.env"}, {Kind: "config", Name: "app.yaml"},
		{Kind: "secret", Name: "pw.txt"}, {Kind: "bind", Name: "link.txt"}, {Kind: "bind", Name: "../outside.txt"},
	}
	get := func(kind, name string) (domain.ResourceDetail, error) {
		return r.c.ResourceDetail(t.Context(), "web", kind, name)
	}

	for _, n := range []string{"nope.env", "../plain.env", "../../outside.txt"} {
		if _, err := get("env_file", n); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", n, err)
		}
	}
	for _, n := range []string{"link.txt", "../outside.txt"} {
		if _, err := get("bind", n); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", n, err)
		}
	}
	if d, err := get("env_file", "plain.env"); err != nil || d.Content != "A=1\n" || d.Secret || d.Masked {
		t.Errorf("plain: %+v %v", d, err)
	}
	if d, err := get("env_file", "db.env"); err != nil || !d.Secret || !d.Masked || d.Content != "USER=••••\nPASSWORD=••••\nCERT=••••" {
		t.Errorf("sops env: %+v %v", d, err)
	}
	for _, kr := range [][2]string{{"config", "app.yaml"}, {"secret", "pw.txt"}} {
		d, err := get(kr[0], kr[1])
		if err != nil || !d.Secret || d.Content != "" {
			t.Errorf("%s: %+v %v", kr[1], d, err)
		}
	}
	if d, err := get("volume", "web_data"); err != nil || d.Info["inspected"] != "volume/web_data" {
		t.Errorf("volume: %+v %v", d, err)
	}
	live, _ := r.c.Resources(t.Context(), "web")
	for _, res := range live.Resources {
		if strings.Contains(res.Name, "pw") && !res.Secret {
			t.Error("secret not flagged in Resources")
		}
	}
}

func TestUpdateResetsRetryBudget(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.e.syncErr = errors.New("compose file not found")
	a := app("web")
	a.AutoSync = true
	r.c.CreateApp(ctx, a)
	for range maxRetries {
		r.waitIdle(t, "web")
		r.c.mu.Lock()
		r.c.apps["web"].nextRetry = time.Now().Add(-time.Second)
		r.c.mu.Unlock()
		r.c.Refresh(ctx, "web")
	}
	r.waitIdle(t, "web")
	syncs := r.e.syncCount()

	r.e.mu.Lock()
	r.e.syncErr = nil
	r.e.mu.Unlock()
	a.Path = "apps/web" // the user fixes the settings
	r.c.UpdateApp(ctx, a)
	r.waitIdle(t, "web")
	if r.e.syncCount() != syncs+1 {
		t.Fatalf("fixed settings did not get a fresh auto-sync attempt: syncs %d -> %d", syncs, r.e.syncCount())
	}
}

func TestMissingComposeFileExplainsPath(t *testing.T) {
	r := newRig(t)
	a := app("web")
	a.ComposeFiles = []string{"missing.yaml"}
	r.c.CreateApp(t.Context(), a)
	r.c.Sync(t.Context(), "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	ops, _ := r.st.ListOps(t.Context(), "web")
	if ops[0].Phase != domain.OpFailed || !strings.Contains(ops[0].Message, `compose file "missing.yaml" not found in path "/"`) {
		t.Fatalf("op = %s %q", ops[0].Phase, ops[0].Message)
	}
}

func TestDeleteIsNotUndoneBySelfHeal(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	a := app("web")
	a.AutoSync, a.SelfHeal = true, true
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")
	syncs := r.e.syncCount()

	// containers vanish mid-delete; the resulting reconcile sees drift and would self-heal
	r.e.onRemove = func(a domain.Application) {
		r.e.mu.Lock()
		r.e.drift = []domain.ServiceDiff{{Service: "web", Change: "added"}}
		r.e.mu.Unlock()
		r.c.reconcileWith(ctx, a, false)
	}
	if err := r.c.DeleteApp(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // a resurrecting sync would run in the background; give it time
	if r.e.syncCount() != syncs {
		t.Fatalf("delete was undone: %d sync(s) after delete", r.e.syncCount()-syncs)
	}
	// and the name can be reused
	if _, err := r.c.CreateApp(ctx, app("web")); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedUndeclaredServicesArePrunePendingNotDrift(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	a := app("web")
	a.AutoSync, a.SelfHeal = true, true
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")
	syncs := r.e.syncCount()

	r.e.mu.Lock()
	r.e.drift = []domain.ServiceDiff{{Service: "adminer", Change: "removed", Fields: []string{domain.FieldStopped}}}
	r.e.mu.Unlock()
	v, _ := r.c.Refresh(ctx, "web")
	r.waitIdle(t, "web")
	if v.SyncStatus != domain.Synced || !slices.Equal(v.PrunePending, []string{"adminer"}) {
		t.Fatalf("status %s pending %v", v.SyncStatus, v.PrunePending)
	}
	if r.e.syncCount() != syncs {
		t.Fatal("self-heal tried to fix a stopped undeclared service")
	}

	// a running undeclared service is still drift
	r.e.mu.Lock()
	r.e.drift = []domain.ServiceDiff{{Service: "adminer", Change: "removed"}}
	r.e.mu.Unlock()
	if v, _ = r.c.Refresh(ctx, "web"); v.SyncStatus != domain.OutOfSync {
		t.Fatalf("running undeclared: %s", v.SyncStatus)
	}
}

func TestOneOffPruneIsNotSaved(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, app("web"))
	prune := true
	if _, err := r.c.Sync(ctx, "web", domain.SyncRequest{Prune: &prune}); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	if r.e.lastPrune != true {
		t.Fatal("engine did not get prune")
	}
	if got, _ := r.st.GetApp(ctx, "web"); got.Prune {
		t.Fatal("one-off prune was saved on the Application")
	}
}

func TestRestoreWorkDirsAfterReboot(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, app("web"))
	r.c.CreateApp(ctx, app("intact"))
	for _, n := range []string{"web", "intact"} {
		r.c.Sync(ctx, n, domain.SyncRequest{})
		r.waitIdle(t, n)
	}
	r.g.set("bbbbbbbb")             // a newer target must NOT be deployed by a restore
	os.RemoveAll(r.c.appDir("web")) // tmpfs wiped by a reboot
	// ...and Docker re-created a missing bind-mount source as an empty directory
	os.MkdirAll(filepath.Join(r.c.appDir("web"), "nginx", "default.conf"), 0o755)
	os.MkdirAll(r.c.appDir("never"), 0o750)
	syncs := r.e.syncCount()

	r.c.restoreWorkDirs(ctx)
	r.waitIdle(t, "web")
	if r.e.syncCount() != syncs+1 {
		t.Fatalf("restores: %d, want 1 (only web)", r.e.syncCount()-syncs)
	}
	ops, _ := r.st.ListOps(ctx, "web")
	if ops[0].Trigger != triggerRestore || ops[0].Revision != "aaaaaaaa" || ops[0].Phase != domain.OpSucceeded {
		t.Fatalf("restore op = %+v", ops[0])
	}
	if !r.e.lastForce {
		t.Fatal("restore did not force-recreate containers")
	}
}

func TestCheckRepo(t *testing.T) {
	r := newRig(t)
	if _, err := r.c.CheckRepo(t.Context(), "  "); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty URL: %v", err)
	}
	got, err := r.c.CheckRepo(t.Context(), " https://example.com/r.git ")
	if err != nil || got.Access != "public" || got.Message != "https://example.com/r.git" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestHostReconnectResyncsAllItsApps(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.c.CreateApp(ctx, app("web"))
	r.c.CreateApp(ctx, app("api"))
	go r.c.watch(ctx)
	var changed func(string, string)
	for changed == nil {
		r.e.mu.Lock()
		changed = r.e.changed
		r.e.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	events, unsub := r.c.Subscribe()
	defer unsub()
	r.g.mu.Lock()
	before := r.g.resolves
	r.g.mu.Unlock()

	// Docker came back: its containers restarted while nobody listened
	r.e.mu.Lock()
	r.e.live.Health = domain.Degraded
	r.e.mu.Unlock()
	changed(domain.LocalHost, "")
	changed("other-host", "") // nothing of ours runs there

	seen := map[string]domain.HealthStatus{}
	for len(seen) < 2 {
		select {
		case e := <-events:
			if v, ok := e.Data.(domain.AppView); ok && e.Type == "app" {
				seen[v.Name] = v.Health
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("not every app was resynced after the reconnect: %v", seen)
		}
	}
	if seen["web"] != domain.Degraded || seen["api"] != domain.Degraded {
		t.Fatalf("stale health after resync: %v", seen)
	}
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	if r.g.resolves != before {
		t.Fatal("a reconnect resync must not fetch Git")
	}
}
