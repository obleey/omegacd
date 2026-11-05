package controller

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/domain"
)

// ---- fakes ----

type fakeStore struct {
	domain.Store // unimplemented methods panic
	mu           sync.Mutex
	apps         map[string]domain.Application
	ops          []domain.SyncOperation
	hosts        map[string]domain.Host
	hostKeys     map[string][]domain.HostKey // current first
}

func newStore() *fakeStore {
	return &fakeStore{apps: map[string]domain.Application{}, hosts: map[string]domain.Host{}, hostKeys: map[string][]domain.HostKey{}}
}

func (s *fakeStore) ListHostKeys(_ context.Context, host string) ([]domain.HostKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.hostKeys[host]), nil
}
func (s *fakeStore) AddHostKey(_ context.Context, k domain.HostKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := s.hostKeys[k.Host]
	for i := range keys {
		keys[i].Current = false
	}
	k.Current = true
	s.hostKeys[k.Host] = append([]domain.HostKey{k}, keys...)
	return nil
}
func (s *fakeStore) DeleteHostKey(_ context.Context, host, recipient string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostKeys[host] = slices.DeleteFunc(s.hostKeys[host], func(k domain.HostKey) bool { return k.Recipient == recipient })
	return nil
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

// fakeSecrets records the identities of each DecryptDir call; with need set, it fails unless need is among them.
type fakeSecrets struct {
	mu     sync.Mutex
	calls  [][]string
	need   string
	writes map[string]string // relative path -> plaintext "decrypted" into the dir
	err    error             // returned after writing
}

func (f *fakeSecrets) DecryptDir(dir string, ids []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ids)
	if f.need != "" && !slices.Contains(ids, f.need) {
		return nil, &domain.WrongKeyError{File: "db.sops.env", Recipients: []string{"age1other"}}
	}
	var written []string
	for name, plain := range f.writes {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(plain), 0o600); err != nil {
			return written, err
		}
		written = append(written, p)
	}
	return written, f.err
}

func (f *fakeSecrets) lastCall() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

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
	statusErr    error
	lastPruneVol bool
	removedVols  []bool // volumes flag per Remove call
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
	e.lastPruneVol = opts.PruneVolumes
	e.lastForce = opts.ForceRecreate
	return e.syncErr
}
func (e *fakeEngine) Remove(_ context.Context, a domain.Application, volumes bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = true
	e.removedVols = append(e.removedVols, volumes)
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
	if e.statusErr != nil {
		return domain.LiveState{}, e.statusErr
	}
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
	c      *Controller
	st     *fakeStore
	g      *fakeGit
	e      *fakeEngine
	sec    *fakeSecrets
	master *age.X25519Identity
}

func newRig(t *testing.T) rig {
	t.Helper()
	master, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := rig{st: newStore(), g: &fakeGit{sha: "aaaaaaaa"}, e: &fakeEngine{live: domain.LiveState{Health: domain.Healthy}}, sec: &fakeSecrets{}, master: master}
	r.c = New(Config{Store: r.st, Git: r.g, Secrets: r.sec, MasterKey: master, Engine: r.e, WorkDir: t.TempDir(), PollInterval: time.Hour})
	// Registered after TempDir, so it runs first: background Syncs (auto, self-heal, App of Apps
	// children) must finish writing the work dir before it is removed.
	t.Cleanup(func() {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			r.c.mu.Lock()
			busy := false
			for _, s := range r.c.apps {
				busy = busy || s.syncing
			}
			r.c.mu.Unlock()
			if !busy {
				return
			}
		}
	})
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
	if err := r.c.DeleteApp(t.Context(), "web", true, false); err != nil {
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
	if err := r.c.DeleteApp(ctx, "web", false, false); err != nil {
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
	if err := r.c.DeleteApp(ctx, "web", true, false); err != nil {
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
	r.e.drift = []domain.ServiceDiff{
		{Service: "adminer", Change: "removed", Fields: []string{domain.FieldStopped}},
		{Kind: "network", Service: "web_old", Change: "removed"},
		{Kind: "volume", Service: "web_data", Change: "removed"},
	}
	r.e.mu.Unlock()
	v, _ := r.c.Refresh(ctx, "web")
	r.waitIdle(t, "web")
	if v.SyncStatus != domain.Synced || !slices.Equal(v.PrunePending, []string{"adminer", "network web_old", "volume web_data"}) {
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

func TestManualSyncAfterRollbackClearsFlag(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, app("web"))
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	r.g.set("bbbbbbbb")
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	if _, err := r.c.Rollback(ctx, "web", 1); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	if a, _ := r.st.GetApp(ctx, "web"); !a.RolledBack {
		t.Fatal("rollback did not set the flag")
	}
	r.c.Sync(ctx, "web", domain.SyncRequest{}) // back to the target Revision
	r.waitIdle(t, "web")
	if a, _ := r.st.GetApp(ctx, "web"); a.RolledBack || a.AutoSync {
		t.Fatalf("after manual Sync: rolledBack=%v autoSync=%v, want false/false", a.RolledBack, a.AutoSync)
	}
}

func TestPruneVolumesIsASubOptionOfPrune(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	a := app("web")
	a.PruneVolumes = true // without Prune: normalized away
	if v, err := r.c.CreateApp(ctx, a); err != nil || v.PruneVolumes {
		t.Fatalf("create: pruneVolumes=%v, %v", v.PruneVolumes, err)
	}
	if got, _ := r.st.GetApp(ctx, "web"); got.PruneVolumes {
		t.Fatal("pruneVolumes without prune was saved")
	}
	a.Prune = true
	if v, err := r.c.UpdateApp(ctx, a); err != nil || !v.PruneVolumes {
		t.Fatalf("update with prune: pruneVolumes=%v, %v", v.PruneVolumes, err)
	}
	r.mustSync(t, "web")
	if !r.e.lastPrune || !r.e.lastPruneVol {
		t.Fatalf("engine got prune=%v pruneVolumes=%v", r.e.lastPrune, r.e.lastPruneVol)
	}

	// a one-off Sync without Prune never prunes volumes
	off := false
	if _, err := r.c.Sync(ctx, "web", domain.SyncRequest{Prune: &off}); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	if r.e.lastPrune || r.e.lastPruneVol {
		t.Fatalf("one-off prune=false: engine got prune=%v pruneVolumes=%v", r.e.lastPrune, r.e.lastPruneVol)
	}
	// nor does a stored flag without Prune (written past normalize)
	a.Prune = false
	a.PruneVolumes = true
	r.st.UpdateApp(ctx, a)
	r.mustSync(t, "web")
	if r.e.lastPruneVol {
		t.Fatal("pruneVolumes reached the engine without Prune")
	}
}

func TestDeleteWithVolumes(t *testing.T) {
	for _, volumes := range []bool{false, true} {
		r := newRig(t)
		r.c.CreateApp(t.Context(), app("web"))
		if err := r.c.DeleteApp(t.Context(), "web", true, volumes); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(r.e.removedVols, []bool{volumes}) {
			t.Fatalf("volumes=%v: Remove got %v", volumes, r.e.removedVols)
		}
	}
}

func TestUnreachableHostIsUnknownNotMissing(t *testing.T) {
	r := newRig(t)
	r.c.CreateApp(t.Context(), app("web"))
	r.e.mu.Lock()
	r.e.statusErr = errors.New("dial unix docker.sock: connect: no such file")
	r.e.mu.Unlock()
	v, _ := r.c.Refresh(t.Context(), "web")
	if v.Health != domain.HealthUnknown || !strings.Contains(v.SyncMessage, "unreachable") {
		t.Fatalf("health %s, message %q; want Unknown with 'unreachable'", v.Health, v.SyncMessage)
	}
}

func TestHostKeysDecryptSyncs(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	sync := func(name string) domain.SyncOperation {
		t.Helper()
		if _, err := r.c.Sync(ctx, name, domain.SyncRequest{}); err != nil {
			t.Fatal(err)
		}
		r.waitIdle(t, name)
		ops, _ := r.st.ListOps(ctx, name)
		return ops[0]
	}

	// local decrypts with the Master Key
	r.c.CreateApp(ctx, app("loc"))
	if op := sync("loc"); op.Phase != domain.OpSucceeded || !slices.Equal(r.sec.lastCall(), []string{r.master.String()}) {
		t.Fatalf("local: %+v %v", op, r.sec.lastCall())
	}
	if keys, _ := r.c.HostKeys(ctx, domain.LocalHost); len(keys) != 1 || !keys[0].Master || keys[0].Recipient != r.master.Recipient().String() || keys[0].PrivateKey != "" {
		t.Fatalf("local keys %+v", keys)
	}
	if _, err := r.c.GenerateHostKey(ctx, domain.LocalHost); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("generate for local: %v", err)
	}

	// creating a remote Host generates its current key
	v, err := r.c.SaveHost(ctx, domain.Host{Name: "prod", URL: "ssh://deploy@prod"}, true)
	if err != nil {
		t.Fatal(err)
	}
	first := r.st.hostKeys["prod"][0]
	if v.KeyRecipient == "" || v.KeyRecipient != first.Recipient || !strings.HasPrefix(first.PrivateKey, "AGE-SECRET-KEY-1") {
		t.Fatalf("view %q, stored %+v", v.KeyRecipient, first)
	}
	a := app("web")
	a.Host = "prod"
	r.c.CreateApp(ctx, a)
	if op := sync("web"); op.Phase != domain.OpSucceeded || !slices.Equal(r.sec.lastCall(), []string{first.PrivateKey}) {
		t.Fatalf("remote: %+v %v", op, r.sec.lastCall())
	}

	// rotation: the previous key is still tried, after the new current one
	second, err := r.c.GenerateHostKey(ctx, "prod")
	if err != nil || second.PrivateKey != "" || !second.Current {
		t.Fatalf("%+v %v", second, err)
	}
	r.sec.need = first.PrivateKey
	if op := sync("web"); op.Phase != domain.OpSucceeded ||
		!slices.Equal(r.sec.lastCall(), []string{r.st.hostKeys["prod"][0].PrivateKey, first.PrivateKey}) {
		t.Fatalf("rotation: %+v %v", op, r.sec.lastCall())
	}
	keys, _ := r.c.HostKeys(ctx, "prod")
	if len(keys) != 2 || keys[0].Recipient != second.Recipient || keys[1].Recipient != first.Recipient || keys[1].Current {
		t.Fatalf("keys %+v", keys)
	}
	for _, k := range keys {
		if k.PrivateKey != "" {
			t.Fatal("HostKeys returned a private key")
		}
	}

	// the current key can't be removed; once the previous one is, its files no longer decrypt
	if err := r.c.RemoveHostKey(ctx, "prod", second.Recipient); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("remove current: %v", err)
	}
	if err := r.c.RemoveHostKey(ctx, "prod", "age1nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("remove unknown: %v", err)
	}
	if err := r.c.RemoveHostKey(ctx, "prod", first.Recipient); err != nil {
		t.Fatal(err)
	}
	want := `db.sops.env is encrypted for age1other; Host "prod" has key ` + second.Recipient + ` — re-encrypt it for this Host (sops updatekeys)`
	if op := sync("web"); op.Phase != domain.OpFailed || op.Message != want {
		t.Fatalf("wrong key: %q", op.Message)
	}
	r.sec.need = r.master.String()
	if op := sync("loc"); op.Phase != domain.OpSucceeded {
		t.Fatalf("local: %q", op.Message)
	}

	// import: a whole age-keygen file is fine, anything else is not
	if _, err := r.c.ImportHostKey(ctx, "prod", "not a key"); !errors.Is(err, domain.ErrInvalid) || strings.Contains(err.Error(), "not a key") {
		t.Fatalf("import garbage: %v", err)
	}
	id, _ := age.GenerateX25519Identity()
	k, err := r.c.ImportHostKey(ctx, "prod", "# created: now\n# public key: "+id.Recipient().String()+"\n"+id.String()+"\n")
	if err != nil || k.Recipient != id.Recipient().String() || !k.Current || r.st.hostKeys["prod"][0].PrivateKey != id.String() {
		t.Fatalf("import: %+v %v", k, err)
	}
}

func TestKeylessHostBlocksSyncs(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.st.hosts["prod"] = domain.Host{Name: "prod", URL: "ssh://deploy@prod"} // e.g. created before Host Keys existed
	a := app("web")
	a.Host, a.AutoSync = "prod", true
	want := `Host "prod" has no Host Key — generate or import one in Settings → Hosts`

	v, err := r.c.CreateApp(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if v.SyncStatus != domain.OutOfSync || v.SyncMessage != want {
		t.Fatalf("status: %s %q", v.SyncStatus, v.SyncMessage)
	}
	r.waitIdle(t, "web")
	if ops, _ := r.st.ListOps(ctx, "web"); len(ops) != 0 {
		t.Fatalf("auto-synced into a keyless Host: %+v", ops)
	}
	if hosts, _ := r.c.ListHosts(ctx); hosts[1].KeyRecipient != "" || hosts[0].KeyRecipient != r.master.Recipient().String() {
		t.Fatalf("%+v", hosts)
	}

	// even without Encrypted Secrets, nothing deploys
	if _, err := r.c.Sync(ctx, "web", domain.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	ops, _ := r.st.ListOps(ctx, "web")
	if ops[0].Phase != domain.OpFailed || ops[0].Message != want || r.e.syncCount() != 0 || len(r.sec.calls) != 0 {
		t.Fatalf("%+v syncs=%d", ops[0], r.e.syncCount())
	}
	if _, err := r.c.Diff(ctx, "web"); err == nil || err.Error() != want {
		t.Fatalf("diff: %v", err)
	}

	// a key unblocks it, and Auto-Sync proceeds
	if _, err := r.c.GenerateHostKey(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for r.e.syncCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.e.syncCount() != 1 {
		t.Fatal("no auto-sync after a Host Key was generated")
	}
	r.waitIdle(t, "web")
}

func TestComposeText(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("# mine\nservices:\n  web: {image: nginx}"), 0o644)
	os.WriteFile(filepath.Join(dir, "prod.yaml"), []byte("services:\n  web: {restart: always}\n"), 0o644)

	got, err := composeText(domain.Application{ComposeFiles: []string{"compose.yaml"}}, dir)
	if want := "# mine\nservices:\n  web: {image: nginx}\n"; err != nil || got != want {
		t.Fatalf("one file: %q, %v", got, err)
	}
	got, err = composeText(domain.Application{ComposeFiles: []string{"compose.yaml", "prod.yaml"}}, dir)
	want := "# ---- compose.yaml ----\n# mine\nservices:\n  web: {image: nginx}\n# ---- prod.yaml ----\nservices:\n  web: {restart: always}\n"
	if err != nil || got != want {
		t.Fatalf("two files:\n%s\n%v", got, err)
	}
	if _, err := composeText(domain.Application{ComposeFiles: []string{"missing.yaml"}}, dir); err == nil {
		t.Fatal("missing file: no error")
	}
}

func TestDecryptedFilesAreRecordedAndMasked(t *testing.T) {
	ctx := t.Context()
	r := newRig(t)
	r.sec.writes = map[string]string{"db.env": "PASSWORD=first-secret\n"}
	r.e.files = []domain.Resource{{Kind: "env_file", Name: "db.env"}, {Kind: "env_file", Name: "plain.env"}}
	if _, err := r.c.CreateApp(ctx, app("web")); err != nil {
		t.Fatal(err)
	}
	sync := func() {
		t.Helper()
		if _, err := r.c.Sync(ctx, "web", domain.SyncRequest{}); err != nil {
			t.Fatal(err)
		}
		r.waitIdle(t, "web")
	}
	sync()
	os.WriteFile(filepath.Join(r.c.appDir("web"), "plain.env"), []byte("A=1\n"), 0o644)

	list := r.c.decryptedList("web")
	if b, err := os.ReadFile(list); err != nil || string(b) != "db.env\n" {
		t.Fatalf("decrypted list: %q, %v", b, err)
	}
	if filepath.Dir(list) == r.c.appDir("web") {
		t.Fatal("the list must live outside the app dir (Export prunes it, remote Hosts get a copy)")
	}
	live, err := r.c.Resources(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	secret := map[string]bool{}
	for _, f := range live.Resources {
		secret[f.Name] = f.Secret
	}
	if !secret["db.env"] || secret["plain.env"] {
		t.Fatalf("secret flags: %v (db.env has no *.sops.* sibling any more, only the list says it was decrypted)", secret)
	}
	d, err := r.c.ResourceDetail(ctx, "web", "env_file", "db.env")
	if err != nil || !d.Masked || strings.Contains(d.Content, "first-secret") || d.Content != "PASSWORD=••••" {
		t.Fatalf("db.env not masked: %+v, %v", d, err)
	}

	// a Diff decrypts into staging: the app's list must stay as it is
	r.sec.writes = map[string]string{"db.env": "PASSWORD=x\n", "new.env": "B=2\n"}
	if _, err := r.c.Diff(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(list); string(b) != "db.env\n" {
		t.Fatalf("Diff changed the list: %q", b)
	}

	// a Sync failing after decrypting some files still records them: they are in the app dir now
	r.sec.err = errors.New("boom")
	sync()
	if set := r.c.readDecrypted("web"); !set["db.env"] || !set["new.env"] {
		t.Fatalf("after a failed decrypt: %v", set)
	}

	// deleting the app removes the list with the app dir
	r.sec.err = nil
	if err := r.c.DeleteApp(ctx, "web", true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(list); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("list left behind: %v", err)
	}
}

func TestStartupDropsLeftoverSopsCopies(t *testing.T) {
	r := newRig(t)
	a := app("web")
	a.ComposeFiles = []string{"compose.yaml"}
	r.st.apps["web"] = a
	dir := r.c.appDir("web")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o750)
	for name, content := range map[string]string{
		"compose.yaml": "services: {}\n",
		"db.sops.env":  "ENC[...]", "db.env": "PW=x\n", // an old-style pair
		"sub/app.sops.yaml": "ENC[...]", "sub/app.yaml": "k: v\n",
		"lonely.sops.env": "ENC[...]", // never decrypted: kept
	} {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
	}
	os.WriteFile(r.c.decryptedList("web"), []byte("earlier.env\n"), 0o600)

	r.c.restoreWorkDirs(t.Context())

	for name, want := range map[string]bool{"db.sops.env": false, "sub/app.sops.yaml": false, "db.env": true, "sub/app.yaml": true, "lonely.sops.env": true, "compose.yaml": true} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s: exists=%v, want %v", name, err == nil, want)
		}
	}
	if b, _ := os.ReadFile(r.c.decryptedList("web")); string(b) != "db.env\nearlier.env\nsub/app.yaml\n" {
		t.Fatalf("list: %q", b)
	}
	if r.e.syncCount() != 0 {
		t.Fatal("tidying must not sync")
	}
	// a second start finds nothing to do
	r.c.restoreWorkDirs(t.Context())
	if b, _ := os.ReadFile(r.c.decryptedList("web")); string(b) != "db.env\nearlier.env\nsub/app.yaml\n" {
		t.Fatalf("list after a second start: %q", b)
	}
}

func TestPlainSecrets(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`services:
  db:
    image: mariadb
    environment:
      MARIADB_ROOT_PASSWORD: hunter2        # warn
      MARIADB_PASSWORD: ${DB_PASSWORD}      # from a SOPS .env: fine
      MARIADB_PASSWORD_FILE: /run/secrets/x # a path: fine
      MARIADB_DATABASE: wordpress           # not a secret name
      REQUIRE_PASSWORD: "true"              # a flag
  web:
    image: app
    environment:
      - API_TOKEN=abc123                    # warn (list form)
      - SESSION_SECRET=$${KEEP}             # $ escapes: treated as templated
      - GF_SECURITY_ADMIN_PASSWORD          # no value: passed through from the env
      - EMPTY_SECRET=
`), 0o644)
	os.WriteFile(filepath.Join(dir, "prod.yaml"), []byte("services:\n  web:\n    environment: {AWS_SECRET_ACCESS_KEY: wJalr}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("services: [\n"), 0o644)

	got := plainSecrets(domain.Application{ComposeFiles: []string{"compose.yaml", "prod.yaml", "broken.yaml", "missing.yaml"}}, dir)
	var keys []string
	for _, w := range got {
		keys = append(keys, strings.SplitN(w, " is in plain text", 2)[0])
	}
	if want := []string{"db: MARIADB_ROOT_PASSWORD", "web: API_TOKEN", "web: AWS_SECRET_ACCESS_KEY"}; !slices.Equal(keys, want) {
		t.Fatalf("warned about %q, want %q", keys, want)
	}
	if want := "db: MARIADB_ROOT_PASSWORD is in plain text in compose.yaml, readable by anyone with access to the repository; " +
		"put it in a SOPS-encrypted .env and write MARIADB_ROOT_PASSWORD: ${MARIADB_ROOT_PASSWORD}"; got[0] != want {
		t.Fatalf("message:\n%s", got[0])
	}
	if strings.Contains(strings.Join(got, ""), "hunter2") {
		t.Fatal("a warning must not repeat the secret")
	}
}

func TestNoLocalHost(t *testing.T) {
	ctx := t.Context()
	r := newRig(t)
	r.c.cfg.NoLocalHost = true
	if _, err := r.c.SaveHost(ctx, domain.Host{Name: "prod", URL: "ssh://deploy@prod"}, true); err != nil {
		t.Fatal(err)
	}
	hosts, err := r.c.ListHosts(ctx)
	if err != nil || len(hosts) != 1 || hosts[0].Name != "prod" {
		t.Fatalf("hosts: %+v, %v", hosts, err)
	}
	// local, explicitly or by default, is refused with the reason
	for _, h := range []string{"", domain.LocalHost} {
		a := app("web")
		a.Host = h
		if _, err := r.c.CreateApp(ctx, a); !errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), "OMEGACD_LOCAL_HOST=false") {
			t.Fatalf("host %q: %v", h, err)
		}
	}
	a := app("web")
	a.Host = "prod"
	if _, err := r.c.CreateApp(ctx, a); err != nil {
		t.Fatal(err)
	}
	// an app left on local from before is not restored (nothing to restore into)
	r.st.apps["old"] = app("old")
	r.st.ops = append(r.st.ops, domain.SyncOperation{App: "old", Phase: domain.OpSucceeded, Revision: "aaaaaaa"})
	r.c.restoreWorkDirs(ctx)
	r.waitIdle(t, "old") // restores run in the background
	if r.e.syncCount() != 0 {
		t.Fatal("restored an app on the disabled local Host")
	}
}

func TestRunWithPollingOff(t *testing.T) {
	r := newRig(t)
	r.c.cfg.PollInterval = 0 // webhooks and Refresh only; time.NewTicker(0) would panic
	if _, err := r.c.CreateApp(t.Context(), app("web")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.c.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // startup: one reconcile of everything
	r.g.mu.Lock()
	after := r.g.resolves
	r.g.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	r.g.mu.Lock()
	later := r.g.resolves
	r.g.mu.Unlock()
	cancel()
	<-done
	if after == 0 || later != after {
		t.Fatalf("Git resolves: %d at startup, %d later (want no polling)", after, later)
	}
}
