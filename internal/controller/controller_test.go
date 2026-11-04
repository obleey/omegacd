package controller

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/obleey/dockercd/internal/domain"
)

// ---- fakes ----

type fakeStore struct {
	domain.Store // unimplemented methods panic
	mu           sync.Mutex
	apps         map[string]domain.Application
	ops          []domain.SyncOperation
}

func newStore() *fakeStore { return &fakeStore{apps: map[string]domain.Application{}} }

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
	mu   sync.Mutex
	sha  string
	tree string
	err  error
}

func (g *fakeGit) Resolve(context.Context, string, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sha, g.err
}
func (g *fakeGit) TreeHash(_ context.Context, _, sha, _ string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return "tree-" + sha, nil
}
func (g *fakeGit) Export(context.Context, string, string, string, string) error { return nil }
func (g *fakeGit) Diff(context.Context, string, string, string, string) (string, error) {
	return "", nil
}
func (g *fakeGit) set(sha string) { g.mu.Lock(); g.sha = sha; g.mu.Unlock() }

type fakeSecrets struct{}

func (fakeSecrets) DecryptDir(string) ([]string, error) { return nil, nil }

type fakeEngine struct {
	mu      sync.Mutex
	syncErr error
	syncs   int
	drift   []domain.ServiceDiff
	exists  bool
	managed bool
	live    domain.LiveState
	removed bool
}

func (e *fakeEngine) Validate(context.Context, domain.Application, string) error { return nil }
func (e *fakeEngine) Diff(context.Context, domain.Application, string, string) ([]domain.ServiceDiff, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.drift, nil
}
func (e *fakeEngine) Sync(context.Context, domain.Application, string, domain.SyncOptions) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.syncs++
	return e.syncErr
}
func (e *fakeEngine) Remove(context.Context, domain.Application) error { e.removed = true; return nil }
func (e *fakeEngine) Status(context.Context, domain.Application) (domain.LiveState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.live, nil
}
func (e *fakeEngine) ProjectExists(context.Context, string) (bool, bool, error) {
	return e.exists, e.managed, nil
}
func (e *fakeEngine) Logs(context.Context, string, bool, int, io.Writer) error { return nil }
func (e *fakeEngine) Close() error                                             { return nil }
func (e *fakeEngine) syncCount() int                                           { e.mu.Lock(); defer e.mu.Unlock(); return e.syncs }

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
	if _, err := r.c.Sync(ctx, "web"); err != nil {
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
	r.c.CreateApp(t.Context(), app("dockercd"))
	r.c.Sync(t.Context(), "dockercd")
	r.waitIdle(t, "dockercd")
	if ops, _ := r.st.ListOps(t.Context(), "dockercd"); ops[0].Phase != domain.OpFailed || r.e.syncCount() != 0 {
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
