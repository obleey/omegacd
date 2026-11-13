package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/appdef"
	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// ---- fakes ----

type fakeStore struct {
	domain.Store                               // unimplemented methods panic
	statuses     map[string]domain.AgentStatus // reports of Agents (M4-02)
	diffs        map[string]domain.AgentDiff   // Diffs Agents computed (M4-11)
	diffErr      error                         // SaveAgentDiff fails with it
	unmanaged    map[string][]string           // by Host (Q118)
	unmanagedErr error                         // SetUnmanaged fails with it
	finishErr    error                         // FinishOp fails with it
	opsErr       error                         // ListOps fails with it
	phaseErr     error                         // SetOpPhase fails with it
	renders      map[string][]domain.File      // kept renders by tree (Q120)
	renderErr    error                         // Render fails with it
	saveErr      error                         // SaveRender fails with it
	createOpErr  error                         // CreateOp fails with it
	statusErr    error                         // AgentStatus fails with it
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
	if s.createOpErr != nil {
		return 0, s.createOpErr
	}
	op.ID = int64(len(s.ops) + 1)
	s.ops = append(s.ops, op)
	return op.ID, nil
}
func (s *fakeStore) SetAgentApps(_ context.Context, host string, apps map[string]string, unmanaged []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unmanagedErr != nil {
		return s.unmanagedErr
	}
	if s.unmanaged == nil {
		s.unmanaged = map[string][]string{}
	}
	s.unmanaged[host] = unmanaged
	h := s.hosts[host]
	h.AgentApps, h.Unmanaged = apps, unmanaged
	s.hosts[host] = h
	return nil
}
func (s *fakeStore) FinishOp(_ context.Context, id int64, p domain.OpPhase, m string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finishErr != nil {
		return s.finishErr
	}
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
	if s.opsErr != nil {
		return nil, s.opsErr
	}
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
func (s *fakeStore) SaveRender(_ context.Context, tree string, files []domain.File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.renders == nil {
		s.renders = map[string][]domain.File{}
	}
	s.renders[tree] = files
	return nil
}
func (s *fakeStore) Render(_ context.Context, tree string) ([]domain.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renderErr != nil {
		return nil, s.renderErr
	}
	files, ok := s.renders[tree]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return files, nil
}
func (s *fakeStore) ListRegistryCredentials(context.Context) ([]domain.RegistryCredential, error) {
	return nil, nil
}

type fakeSource struct {
	domain.Source // credentials: unused here, panic
	mu            sync.Mutex
	sha           string
	err           error
	resolves      int
	renders       int               // of App of Apps definitions
	files         map[string]string // an App of Apps' files when set; every other app gets a compose.yaml
	blobs         map[string][]byte
	appFiles      map[string]string // more files every compose app gets
	renderErr     error             // Render fails with it
	renderErrRev  string            // and only Renders of this Revision fail with it, when set
	blobErr       error             // Blob fails with it
	badHash       bool              // Render names files by a hash that isn't hex
	warnings      []string          // Render's
}

func (g *fakeSource) Invalidate(string) {}
func (g *fakeSource) Resolve(context.Context, string, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolves++
	return g.sha, g.err
}
func (g *fakeSource) Render(_ context.Context, spec domain.RenderSpec) (domain.Rendered, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := domain.Rendered{Checksum: "tree-" + spec.Revision, Warnings: g.warnings}
	if g.renderErr != nil && (g.renderErrRev == "" || g.renderErrRev == spec.Revision) {
		return r, g.renderErr
	}
	files := map[string]string{"compose.yaml": "services: {}\n"}
	maps.Copy(files, g.appFiles)
	if spec.Apps {
		g.renders++
		files = g.files
	}
	if g.blobs == nil {
		g.blobs = map[string][]byte{}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		h := blob.Hash([]byte(files[name]))
		g.blobs[h] = []byte(files[name])
		if g.badHash {
			h = "not-hex"
		}
		r.Files = append(r.Files, domain.File{Path: name, Mode: 0o644, SHA256: h})
	}
	if spec.Apps {
		dir, err := os.MkdirTemp("", "defs")
		if err != nil {
			return r, err
		}
		defer os.RemoveAll(dir)
		for name, content := range files {
			os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
		}
		r.Apps, r.Invalid = appdef.Parse(dir)
	}
	return r, nil
}
func (g *fakeSource) Blob(_ context.Context, hash string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blobErr != nil {
		return nil, g.blobErr
	}
	return g.blobs[hash], nil
}
func (g *fakeSource) Diff(context.Context, string, string, string, string) (string, error) {
	return "", nil
}
func (g *fakeSource) CheckAccess(_ context.Context, url string) (domain.RepoAccess, error) {
	return domain.RepoAccess{Access: "public", Message: url}, nil
}
func (g *fakeSource) set(sha string) { g.mu.Lock(); g.sha = sha; g.mu.Unlock() }

// define makes the next Revision hold one definitions file.
func (g *fakeSource) define(sha, defs string) {
	g.mu.Lock()
	g.sha, g.files = sha, map[string]string{"apps.yaml": defs}
	g.mu.Unlock()
}

type rig struct {
	c  *Controller
	st *fakeStore
	g  *fakeSource
	ag *fakeAgents
}

// newRig is a Controller whose Host "edge" has an enrolled Agent.
func newRig(t *testing.T) rig {
	t.Helper()
	r := rig{st: newStore(), g: &fakeSource{sha: "aaaaaaaa"}, ag: &fakeAgents{}}
	r.st.hosts["edge"] = domain.Host{Name: "edge", Fingerprint: "SHA256:edge", Approved: true}
	blobs, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.c = New(Config{Store: r.st, Source: r.g, Agents: r.ag, Blobs: blobs, PollInterval: time.Hour})
	// background Syncs (auto, App of Apps children) finish before the test's temporary dirs go
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

// syncCount is how many Desired States the Agents were sent.
func (r rig) syncCount() int { return len(r.ag.sent()) }

// lastSent is the last Desired State the Agents were sent.
func (r rig) lastSent(t *testing.T) *agentv1.DesiredState {
	t.Helper()
	sent := r.ag.sent()
	if len(sent) == 0 {
		t.Fatal("nothing sent to an Agent")
	}
	return sent[len(sent)-1]
}

// failDeploys makes every Deploy (and Remove) fail with err; nil: succeed.
func (r rig) failDeploys(err error) {
	r.ag.mu.Lock()
	r.ag.err = err
	r.ag.mu.Unlock()
}

func app(name string) domain.Application {
	return domain.Application{Name: name, RepoURL: "https://example.com/r.git", Host: "edge"}
}

// ---- tests ----

func TestCreateDefaultsAndStatus(t *testing.T) {
	r := newRig(t)
	v, err := r.c.CreateApp(t.Context(), app("web"))
	if err != nil {
		t.Fatal(err)
	}
	if v.TargetRevision != "main" || v.ComposeFiles != nil || v.Host != "edge" {
		t.Fatalf("defaults not applied: %+v", v.Application)
	}
	if v.SyncStatus != domain.OutOfSync || v.SyncMessage != "never synced" {
		t.Fatalf("got %s %q", v.SyncStatus, v.SyncMessage)
	}
}

func TestCreateValidation(t *testing.T) {
	r := newRig(t)
	for _, a := range []domain.Application{
		{Name: "Bad Name", RepoURL: "x", Host: "edge"},
		{Name: "ok", Host: "edge"},
		{Name: "ok", RepoURL: "x"}, // no Host: there is no default (Q104)
		{Name: "ok", RepoURL: "x", Host: "edge", Path: "../etc"},
		{Name: "ok", RepoURL: "x", Host: "edge", ComposeFiles: []string{"/abs.yaml"}},
		{Name: "ok", RepoURL: "x", Host: "edge", WorkDir: "srv/apps"},
		{Name: "ok", RepoURL: "x", Host: "edge", WorkDir: "/"},
		{Name: "ok", RepoURL: "x", Host: "edge", WorkDir: "/srv/../etc"},
	} {
		if _, err := r.c.CreateApp(t.Context(), a); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%+v: want ErrInvalid, got %v", a, err)
		}
	}
}

// Q57: an Application's own work dir is kept clean and absolute; an App of Apps has no folder, so none.
func TestWorkDirIsNormalized(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	v, err := r.c.CreateApp(ctx, domain.Application{Name: "web", RepoURL: "x", Host: "edge", WorkDir: " /srv/apps/ "})
	if err != nil || v.WorkDir != "/srv/apps" {
		t.Fatalf("got %q, %v", v.WorkDir, err)
	}
	if got, _ := r.st.GetApp(ctx, "web"); got.WorkDir != "/srv/apps" {
		t.Fatalf("stored %q", got.WorkDir)
	}
	v, err = r.c.CreateApp(ctx, domain.Application{Name: "root", RepoURL: "x", Kind: domain.KindApps, WorkDir: "/srv/apps"})
	if err != nil || v.WorkDir != "" {
		t.Fatalf("an App of Apps: %q, %v", v.WorkDir, err)
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

	r.st.setStatus("web", domain.AgentStatus{Revision: "aaaaaaaa", Drifted: true, Drift: []domain.ServiceDiff{{Service: "web", Change: "changed"}}})
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
	if r.syncCount() != 0 {
		t.Fatal("synced while Unknown")
	}
}

// Auto-Sync deploys a new Revision; drift its Agent reports is its Agent's to heal (Self-Heal), never re-synced from
// here (Q82).
func TestAutoSyncAndAgentDrift(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	a := app("web")
	a.AutoSync, a.SelfHeal = true, true
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")
	if r.syncCount() != 1 {
		t.Fatalf("auto-sync: syncs=%d", r.syncCount())
	}
	r.st.setStatus("web", domain.AgentStatus{Revision: "aaaaaaaa", Drifted: true, Drift: []domain.ServiceDiff{{Service: "web", Change: "changed"}}})
	v, _ := r.c.Refresh(ctx, "web")
	r.waitIdle(t, "web")
	if v.SyncStatus != domain.OutOfSync || r.syncCount() != 1 {
		t.Fatalf("drift: %s, syncs=%d", v.SyncStatus, r.syncCount())
	}
}

func TestAutoSyncRetriesThenGivesUpUntilNewRevision(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.failDeploys(errors.New("pull failed"))
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
	if r.syncCount() != maxRetries {
		t.Fatalf("syncs=%d want %d", r.syncCount(), maxRetries)
	}
	r.c.Refresh(ctx, "web")
	r.waitIdle(t, "web")
	if r.syncCount() != maxRetries {
		t.Fatal("kept retrying after giving up")
	}

	r.failDeploys(nil)
	r.g.set("bbbbbbbb")
	r.c.Refresh(ctx, "web")
	r.waitIdle(t, "web")
	if r.syncCount() != maxRetries+1 {
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
	syncs := r.syncCount()

	got.AutoSync = true
	v, _ = r.c.UpdateApp(ctx, got)
	r.waitIdle(t, "web")
	if v.RolledBack || r.syncCount() != syncs+1 {
		t.Fatalf("re-enabling auto-sync: rolledBack=%v syncs=%d", v.RolledBack, r.syncCount())
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

func TestDeleteCascade(t *testing.T) {
	r := newRig(t)
	r.c.CreateApp(t.Context(), app("web"))
	events, cancel := r.c.Subscribe()
	defer cancel()
	if err := r.c.DeleteApp(t.Context(), "web", true, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.ag.removed, []string{"web false false"}) {
		t.Fatalf("cascade: the Agent was asked %v", r.ag.removed)
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

func TestAppHostMustExistAndCannotChange(t *testing.T) {
	r := newRig(t)
	a := app("web")
	a.Host = "prod"
	if _, err := r.c.CreateApp(t.Context(), a); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unknown host: want ErrInvalid, got %v", err)
	}
	r.st.mu.Lock()
	r.st.hosts["prod"] = domain.Host{Name: "prod"}
	r.st.mu.Unlock()
	if v, err := r.c.CreateApp(t.Context(), a); err != nil || v.Host != "prod" {
		t.Fatalf("got %v %v", v.Host, err)
	}
	a.Host = "edge"
	if _, err := r.c.UpdateApp(t.Context(), a); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("host change: want ErrInvalid, got %v", err)
	}
}

func TestHosts(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.st.mu.Lock()
	r.st.hosts["prod"] = domain.Host{Name: "prod", AgeRecipient: "age1prod",
		Agent: &domain.AgentInfo{EngineVersion: "29.0", OS: "Debian", LeftAt: time.Now()}}
	r.st.mu.Unlock()
	a := app("web")
	a.Host = "prod"
	if _, err := r.c.CreateApp(ctx, a); err != nil {
		t.Fatal(err)
	}

	hosts, err := r.c.ListHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(hosts, func(a, b domain.HostView) int { return strings.Compare(a.Name, b.Name) })
	if len(hosts) != 2 || hosts[0].Name != "edge" || hosts[1].Name != "prod" || !slices.Equal(hosts[1].Apps, []string{"web"}) ||
		hosts[1].Status.ServerVersion != "29.0" || hosts[1].AgeRecipient != "age1prod" {
		t.Fatalf("got %+v", hosts)
	}
	if keys, err := r.c.HostKeys(ctx, "prod"); err != nil || len(keys) != 1 || keys[0].Recipient != "age1prod" || !keys[0].Current {
		t.Fatalf("host keys: %+v %v", keys, err)
	}
	if keys, err := r.c.HostKeys(ctx, "edge"); err != nil || len(keys) != 0 {
		t.Fatalf("no Agent's key yet: %+v %v", keys, err)
	}
	if _, err := r.c.HostKeys(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown Host: %v", err)
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
}

func TestUpdateResetsRetryBudget(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.failDeploys(errors.New("compose file not found"))
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
	syncs := r.syncCount()

	r.failDeploys(nil)
	a.Path = "apps/web" // the user fixes the settings
	r.c.UpdateApp(ctx, a)
	r.waitIdle(t, "web")
	if r.syncCount() != syncs+1 {
		t.Fatalf("fixed settings did not get a fresh auto-sync attempt: syncs %d -> %d", syncs, r.syncCount())
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
	if !r.lastSent(t).Prune {
		t.Fatal("the Agent did not get prune")
	}
	if got, _ := r.st.GetApp(ctx, "web"); got.Prune {
		t.Fatal("one-off prune was saved on the Application")
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
	if ds := r.lastSent(t); !ds.Prune || !ds.PruneVolumes {
		t.Fatalf("the Agent got prune=%v pruneVolumes=%v", ds.Prune, ds.PruneVolumes)
	}

	// a one-off Sync without Prune never prunes volumes
	off := false
	if _, err := r.c.Sync(ctx, "web", domain.SyncRequest{Prune: &off}); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	if ds := r.lastSent(t); ds.Prune || ds.PruneVolumes {
		t.Fatalf("one-off prune=false: the Agent got prune=%v pruneVolumes=%v", ds.Prune, ds.PruneVolumes)
	}
	// nor does a stored flag without Prune (written past normalize)
	a.Prune = false
	a.PruneVolumes = true
	r.st.UpdateApp(ctx, a)
	r.mustSync(t, "web")
	if r.lastSent(t).PruneVolumes {
		t.Fatal("pruneVolumes reached the Agent without Prune")
	}
}

func TestDeleteWithVolumes(t *testing.T) {
	for _, volumes := range []bool{false, true} {
		r := newRig(t)
		r.c.CreateApp(t.Context(), app("web"))
		if err := r.c.DeleteApp(t.Context(), "web", true, volumes); err != nil {
			t.Fatal(err)
		}
		if want := []string{fmt.Sprint("web ", volumes, " false")}; !slices.Equal(r.ag.removed, want) {
			t.Fatalf("volumes=%v: the Agent was asked %v", volumes, r.ag.removed)
		}
	}
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

func TestComposeTextFindsDefaultFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {web: {image: nginx}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := composeText(domain.Application{}, dir)
	if err != nil || !strings.Contains(got, "nginx") {
		t.Fatalf("composeText without named files: %q %v", got, err)
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

// Q62: everything a Sync logs, up to what it sends the Agent, carries its app and op; a reconcile says why it didn't sync.
func TestLogsFollowASync(t *testing.T) {
	r := newRig(t)
	var out logging.Buffer
	log, _ := logging.New(&out, "debug", "text")
	r.c.cfg.Log = log.With("component", "controller")
	ctx := t.Context()
	r.c.CreateApp(ctx, app("web"))
	id, err := r.c.Sync(ctx, "web", domain.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	op := fmt.Sprintf(" app=web op=%d revision=aaaaaaa host=edge", id)
	for _, want := range []string{
		`msg=reconciled component=controller status=OutOfSync message="never synced" health=Unknown target=aaaaaaa deployed="" drift=false autoSync=no why="Auto-Sync off" app=web`,
		`msg="sync started" component=controller trigger=manual` + op,
		`msg="desired state to the agent" component=controller files=1 tree=tree-aaaaaaaa` + op,
		`msg="sync finished" component=controller trigger=manual phase=Succeeded msg="synced to aaaaaaa"` + op,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
}

func TestAutoTriggerSaysWhy(t *testing.T) {
	c := New(Config{})
	out := domain.AppView{SyncStatus: domain.OutOfSync}
	for _, tc := range []struct {
		a            domain.Application
		s            appState
		v            domain.AppView
		trigger, why string
	}{
		{domain.Application{AutoSync: true}, appState{targetTree: "t"}, domain.AppView{SyncStatus: domain.Synced}, "", "not OutOfSync"},
		{domain.Application{}, appState{targetTree: "t"}, out, "", "Auto-Sync off"},
		{domain.Application{AutoSync: true, RolledBack: true}, appState{targetTree: "t"}, out, "", "rolled back"},
		{domain.Application{AutoSync: true}, appState{targetTree: "t", syncing: true}, out, "", "busy"},
		{domain.Application{AutoSync: true}, appState{targetTree: "t", failedTree: "t"}, out, "", "this tree used up its retries"},
		{domain.Application{AutoSync: true}, appState{targetTree: "t", drift: true}, out, "", "drifted, Self-Heal off"},
		{domain.Application{AutoSync: true, SelfHeal: true}, appState{targetTree: "t", drift: true}, out, "self-heal", "drifted"},
		{domain.Application{AutoSync: true}, appState{targetTree: "t"}, out, "auto", "new revision"},
	} {
		if trigger, why := c.autoTrigger(tc.a, &tc.s, tc.v); trigger != tc.trigger || why != tc.why {
			t.Errorf("%+v %+v: got %q %q, want %q %q", tc.a, tc.s, trigger, why, tc.trigger, tc.why)
		}
	}
}

func (s *fakeStore) CreateInvite(_ context.Context, host string, expires time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[host] = domain.Host{Name: host}
	return "omegacd-invite-" + host, nil
}

// An admin adds a Host by name for its Agent: the name is checked like any Host's, the invite lasts an hour (Q73),
// and the Host, not yet reporting through its Agent, isn't dialed for a status.
func TestInviteHost(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	for _, name := range []string{"Web", ""} {
		if _, _, err := r.c.InviteHost(ctx, name); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("InviteHost(%q): %v", name, err)
		}
	}
	if _, _, err := r.c.InviteHost(ctx, "local"); err != nil { // an ordinary name now (Q110)
		t.Fatal(err)
	}
	invite, expires, err := r.c.InviteHost(ctx, " web ")
	if err != nil || invite != "omegacd-invite-web" || time.Until(expires) < 59*time.Minute || time.Until(expires) > time.Hour {
		t.Fatalf("%q %v %v", invite, expires, err)
	}
	hosts, err := r.c.ListHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hosts {
		if h.Name == "web" && (h.Status != domain.HostStatus{Name: "web"}) {
			t.Fatalf("an Agent's Host got an engine status: %+v", h.Status)
		}
	}
}

func (s *fakeStore) SetOpPhase(_ context.Context, id int64, p domain.OpPhase, m string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phaseErr != nil {
		return s.phaseErr
	}
	s.ops[id-1].Phase, s.ops[id-1].Message = p, m
	return nil
}
func (s *fakeStore) SupersedeWaitingOps(_ context.Context, app string, by int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.ops {
		if s.ops[i].App == app && s.ops[i].Phase == domain.OpWaiting && s.ops[i].ID != by {
			s.ops[i].Phase, s.ops[i].FinishedAt = domain.OpSuperseded, time.Now()
		}
	}
	return nil
}

// fakeAgents is the Hosts' Agents: Deploy records each Desired State and answers with err.
type fakeAgents struct {
	mu      sync.Mutex
	got     []*agentv1.DesiredState
	err     error
	only    string   // when set, err is only that Host's Agent's
	removed []string // "app volumes keep"
	offline bool     // Online answers !offline
	logs    string   // what Logs writes
	diff    []domain.ServiceDiff
	diffErr error
	asked   []string // the Revisions Diff was asked about
}

func (f *fakeAgents) Diff(_ context.Context, host string, ds *agentv1.DesiredState) ([]domain.ServiceDiff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, ds.Revision)
	return f.diff, f.diffErr
}

func (f *fakeAgents) Online(string) bool { f.mu.Lock(); defer f.mu.Unlock(); return !f.offline }

func (f *fakeAgents) Logs(_ context.Context, host, app, container string, follow bool, tail int, w io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	_, err := fmt.Fprintf(w, "%s %s/%s %s follow=%v tail=%d", f.logs, host, app, container, follow, tail)
	return err
}

func (f *fakeAgents) Remove(_ context.Context, host, app string, volumes, keep bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.only != "" && f.only != host {
		return nil
	}
	if f.err == nil {
		f.removed = append(f.removed, fmt.Sprint(app, " ", volumes, " ", keep))
	}
	return f.err
}

func (f *fakeAgents) Deploy(_ context.Context, host string, ds *agentv1.DesiredState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, ds)
	return f.err
}

func (f *fakeAgents) sent() []*agentv1.DesiredState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// agentRig is newRig with its fake Agents at hand.
func agentRig(t *testing.T) (rig, *fakeAgents) {
	r := newRig(t)
	return r, r.ag
}

// M4-01: a Sync to a Host with an Agent sends it the Desired State, the files as rendered, and records its report;
// nothing is written, decrypted or run on the Control Plane's side.
func TestSyncThroughAgent(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	a := app("web")
	a.Host, a.WorkDir, a.SelfHeal, a.Prune, a.Profiles = "edge", "/srv/apps", true, true, []string{"debug"}
	if _, err := r.c.CreateApp(ctx, a); err != nil {
		t.Fatal(err)
	}
	id, err := r.c.Sync(ctx, "web", domain.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	sent := ag.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d Desired States", len(sent))
	}
	ds := sent[0]
	if ds.App != "web" || ds.Op != uint64(id) || ds.Revision != "aaaaaaaa" || ds.WorkDir != "/srv/apps" || !ds.SelfHeal ||
		!ds.Prune || !slices.Equal(ds.Profiles, []string{"debug"}) || len(ds.Files) == 0 || ds.Checksum == "" {
		t.Fatalf("Desired State %v", ds)
	}
	op, _ := r.st.GetOp(ctx, id)
	v, _ := r.c.GetApp(ctx, "web")
	if op.Phase != domain.OpSucceeded || v.SyncStatus != domain.Synced || v.Health != domain.HealthUnknown {
		t.Fatalf("op %s, app %s %s", op.Phase, v.SyncStatus, v.Health)
	}

	ag.mu.Lock()
	ag.err = errors.New("image not found")
	ag.mu.Unlock()
	id, _ = r.c.Sync(ctx, "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	if op, _ := r.st.GetOp(ctx, id); op.Phase != domain.OpFailed || op.Message != "image not found" {
		t.Fatalf("the Agent's failure: %+v", op)
	}
}

// Q81: a Sync to an offline Host waits for it, Auto-Sync doesn't pile up more, and a newer Sync supersedes it.
// Q93: one whose connection dropped stays Running.
func TestSyncWaitsForAgent(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	ag.err = fmt.Errorf("host edge: %w", agents.ErrOffline)
	a := app("web")
	a.Host, a.AutoSync = "edge", true
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")
	ops, _ := r.st.ListOps(ctx, "web")
	if len(ops) != 1 || ops[0].Phase != domain.OpWaiting || !strings.Contains(ops[0].Message, "waiting for Host edge") {
		t.Fatalf("ops %+v", ops)
	}
	for range 3 {
		r.c.Refresh(ctx, "web")
		r.waitIdle(t, "web")
	}
	if ops, _ := r.st.ListOps(ctx, "web"); len(ops) != 1 {
		t.Fatalf("Auto-Sync piled up %d Syncs for an offline Host", len(ops))
	}

	ag.mu.Lock()
	ag.err = fmt.Errorf("host edge: %w", agents.ErrDisconnected)
	ag.mu.Unlock()
	id, _ := r.c.Sync(ctx, "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	ops, _ = r.st.ListOps(ctx, "web")
	if ops[1].Phase != domain.OpSuperseded || ops[0].ID != id || ops[0].Phase != domain.OpRunning ||
		!strings.Contains(ops[0].Message, "disconnected") {
		t.Fatalf("ops %+v", ops)
	}
}

// M3-06/Q81: a Sync that waited for its Host runs, as the same Sync Operation, once its Agent resyncs.
func TestWaitingSyncRunsOnResync(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	ag.err = fmt.Errorf("host edge: %w", agents.ErrOffline)
	a := app("web")
	a.Host = "edge"
	r.c.CreateApp(ctx, a)
	id, _ := r.c.Sync(ctx, "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	if op, _ := r.st.GetOp(ctx, id); op.Phase != domain.OpWaiting {
		t.Fatalf("op %+v", op)
	}
	r.c.Resynced(ctx, "other", nil, nil) // another Host's Agent: nothing
	r.waitIdle(t, "web")
	if op, _ := r.st.GetOp(ctx, id); op.Phase != domain.OpWaiting {
		t.Fatalf("ran for another Host: %+v", op)
	}

	ag.mu.Lock()
	ag.err = nil // back
	ag.mu.Unlock()
	r.c.Resynced(ctx, "edge", nil, nil)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if op, _ := r.st.GetOp(ctx, id); op.Phase == domain.OpSucceeded {
			break
		}
		if time.Now().After(deadline) {
			op, _ := r.st.GetOp(ctx, id)
			t.Fatalf("the waiting Sync didn't run: %+v", op)
		}
	}
	if sent := ag.sent(); len(sent) != 2 || sent[1].Op != uint64(id) {
		t.Fatalf("sent %v", sent)
	}
	if ops, _ := r.st.ListOps(ctx, "web"); len(ops) != 1 {
		t.Fatalf("%d Sync Operations, want the one that waited", len(ops))
	}
}

func (s *fakeStore) SetAppDeleting(_ context.Context, name, how string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[name]
	if !ok {
		return domain.ErrNotFound
	}
	a.Deleting = how
	s.apps[name] = a
	return nil
}

// Q83: deleting an Application on an Agent Host goes through its Agent; offline, it waits (recorded) and finishes
// when the Host resyncs; --cascade=false only has the Agent forget it. Q92: forgetting asks no Agent.
func TestDeleteThroughAgent(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	for _, name := range []string{"web", "db", "cache", "old"} {
		a := app(name)
		a.Host, a.AutoSync = "edge", true
		if _, err := r.c.CreateApp(ctx, a); err != nil {
			t.Fatal(err)
		}
		r.waitIdle(t, name)
	}
	if err := r.c.DeleteApp(ctx, "web", true, true); err != nil {
		t.Fatal(err)
	}
	if err := r.c.DeleteApp(ctx, "cache", false, false); err != nil {
		t.Fatal(err)
	}
	ag.mu.Lock()
	if !slices.Equal(ag.removed, []string{"web true false", "cache false true"}) {
		t.Fatalf("removed %v", ag.removed)
	}
	ag.err = fmt.Errorf("host edge: %w", agents.ErrOffline)
	ag.mu.Unlock()

	err := r.c.DeleteApp(ctx, "db", true, false)
	if !errors.Is(err, domain.ErrWaitingForHost) {
		t.Fatalf("offline: %v", err)
	}
	a, _ := r.st.GetApp(ctx, "db")
	if a.Deleting != "containers" {
		t.Fatalf("not recorded as being deleted: %+v", a)
	}
	ops, _ := r.st.ListOps(ctx, "db")
	r.c.Refresh(ctx, "db")
	r.waitIdle(t, "db")
	if now, _ := r.st.ListOps(ctx, "db"); len(now) != len(ops) {
		t.Fatal("Auto-Sync synced an Application being deleted")
	}

	ag.mu.Lock()
	ag.err = nil
	ag.mu.Unlock()
	r.c.Resynced(ctx, "edge", map[string]string{"old": "tree-aaaaaaaa"}, nil) // what its Agent runs: nothing to restore (Q123)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := r.st.GetApp(ctx, "db"); errors.Is(err, domain.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the waiting delete didn't finish on resync")
		}
	}

	if err := r.c.ForgetApp(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if _, err := r.st.GetApp(ctx, "old"); !errors.Is(err, domain.ErrNotFound) || slices.Contains(ag.removed, "old false false") {
		t.Fatalf("forget: %v, removed %v", err, ag.removed)
	}
}

// awaitEvent returns the first event of typ for app, failing after a while.
func awaitEvent(t *testing.T, events <-chan domain.Event, typ, app string) domain.Event {
	t.Helper()
	for deadline := time.After(2 * time.Second); ; {
		select {
		case e := <-events:
			if e.Type == typ && e.App == app {
				return e
			}
		case <-deadline:
			t.Fatalf("no %s event for %s", typ, app)
		}
	}
}

// The hub's callbacks (M3-06, M4-08): a report shows the Sync Operation and the status, progress goes to the UI as
// it comes, a removal from the outbox shows the app gone.
func TestAgentCallbacks(t *testing.T) {
	r, _ := agentRig(t)
	ctx := t.Context()
	a := app("web")
	a.Host = "edge"
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")
	events, unsub := r.c.Subscribe()
	defer unsub()

	id, _ := r.st.CreateOp(ctx, domain.SyncOperation{App: "web", Revision: "r1", Trigger: "self-heal"})
	r.st.FinishOp(ctx, id, domain.OpSucceeded, "")
	r.c.AgentReported(ctx, "web")
	if op := awaitEvent(t, events, "op", "web").Data.(domain.SyncOperation); op.ID != id || op.Trigger != "self-heal" {
		t.Fatalf("op event %+v", op)
	}
	awaitEvent(t, events, "app", "web")

	r.c.AgentProgress(ctx, "web", domain.ProgressEvent{OpID: id, Resource: "Image busybox", Status: "Pulling"})
	if p := awaitEvent(t, events, "progress", "web").Data.(domain.ProgressEvent); p.OpID != id || p.Status != "Pulling" {
		t.Fatalf("progress %+v", p)
	}

	r.st.DeleteApp(ctx, "web") // the store dropped it when it applied the Agent's removal
	r.c.AgentRemoved(ctx, "edge", "web")
	awaitEvent(t, events, "app-deleted", "web")
	r.c.mu.Lock()
	_, kept := r.c.apps["web"]
	r.c.mu.Unlock()
	if kept {
		t.Fatal("the controller still holds the removed app")
	}
}

// Q83: an App of Apps deleted with a child on an offline Host waits for it, and goes once that child is gone.
func TestAppOfAppsDeleteWaitsForChild(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	parent := app("platform")
	parent.Kind = domain.KindApps
	r.st.CreateApp(ctx, parent)
	r.st.hosts["prod"] = domain.Host{Name: "prod"}
	for _, name := range []string{"prod-child", "edge-child"} {
		ch := app(name)
		ch.Parent, ch.Kind = "platform", domain.KindCompose
		if name == "prod-child" {
			ch.Host = "prod"
		}
		r.st.CreateApp(ctx, ch)
	}
	ag.err, ag.only = fmt.Errorf("host edge: %w", agents.ErrOffline), "edge"

	err := r.c.DeleteApp(ctx, "platform", true, false)
	if !errors.Is(err, domain.ErrWaitingForHost) || !strings.Contains(err.Error(), "edge-child") {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.st.GetApp(ctx, "prod-child"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the child on a reachable Host should be gone: %v", err)
	}
	p, _ := r.st.GetApp(ctx, "platform")
	ch, _ := r.st.GetApp(ctx, "edge-child")
	if p.Deleting != "containers" || ch.Deleting != "containers" {
		t.Fatalf("recorded as being deleted: parent %q, child %q", p.Deleting, ch.Deleting)
	}

	ag.mu.Lock()
	ag.err = nil
	ag.mu.Unlock()
	r.c.Resynced(ctx, "edge", nil, nil)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		_, cerr := r.st.GetApp(ctx, "edge-child")
		_, perr := r.st.GetApp(ctx, "platform")
		if errors.Is(cerr, domain.ErrNotFound) && errors.Is(perr, domain.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %v, App of Apps %v: want both gone once the Host is back", cerr, perr)
		}
	}
}

// The rest of deleting through Agents: what forget refuses, an Agent's own error, a retry that fails, a delete that
// keeps containers waiting, Auto-Sync leaving an Application being deleted alone, and a removal from the outbox
// finishing its App of Apps.
func TestDeleteThroughAgentEdges(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	parent := app("platform")
	parent.Kind = domain.KindApps
	r.st.CreateApp(ctx, parent)
	if err := r.c.ForgetApp(ctx, "platform"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("forget an App of Apps: %v", err)
	}
	if err := r.c.ForgetApp(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("forget an unknown app: %v", err)
	}
	for _, name := range []string{"web", "keep", "child"} {
		a := app(name)
		a.Host, a.Kind = "edge", domain.KindCompose
		if name == "child" {
			a.Parent = "platform"
		}
		r.st.CreateApp(ctx, a)
	}
	r.c.mu.Lock()
	r.c.state("web").syncing = true
	r.c.mu.Unlock()
	if err := r.c.ForgetApp(ctx, "web"); !errors.Is(err, domain.ErrBusy) {
		t.Fatalf("forget while syncing: %v", err)
	}
	r.c.setSyncing("web", false)

	ag.mu.Lock()
	ag.err = errors.New("permission denied")
	ag.mu.Unlock()
	if err := r.c.DeleteApp(ctx, "web", true, false); err == nil || err.Error() != "permission denied" {
		t.Fatalf("the Agent's error: %v", err)
	}
	r.st.SetAppDeleting(ctx, "web", "containers")
	r.c.finishDeleting(ctx, "web") // a retry that fails again: logged, the app stays
	if a, err := r.st.GetApp(ctx, "web"); err != nil || a.Deleting != "containers" {
		t.Fatalf("after a failed retry: %+v %v", a, err)
	}

	ag.mu.Lock()
	ag.err = fmt.Errorf("host edge: %w", agents.ErrOffline)
	ag.mu.Unlock()
	if err := r.c.DeleteApp(ctx, "keep", false, false); !errors.Is(err, domain.ErrWaitingForHost) {
		t.Fatal(err)
	}
	k, _ := r.st.GetApp(ctx, "keep")
	if k.Deleting != "keep" {
		t.Fatalf("keep-containers recorded as %q", k.Deleting)
	}
	v := app("vols")
	v.Host, v.Kind = "edge", domain.KindCompose
	r.st.CreateApp(ctx, v)
	if err := r.c.DeleteApp(ctx, "vols", true, true); !errors.Is(err, domain.ErrWaitingForHost) {
		t.Fatal(err)
	}
	if v, _ := r.st.GetApp(ctx, "vols"); v.Deleting != "volumes" {
		t.Fatalf("with volumes recorded as %q", v.Deleting)
	}
	k.AutoSync = true
	r.st.UpdateApp(ctx, k)
	r.c.reconcile(ctx, k) // never synced: OutOfSync, Auto-Sync on, but being deleted
	r.waitIdle(t, "keep")
	if ops, _ := r.st.ListOps(ctx, "keep"); len(ops) != 0 {
		t.Fatalf("Auto-Sync synced an app being deleted: %+v", ops)
	}

	if err := r.c.DeleteApp(ctx, "platform", true, false); !errors.Is(err, domain.ErrWaitingForHost) {
		t.Fatal(err)
	}
	r.c.reconcile(ctx, func() domain.Application { a, _ := r.st.GetApp(ctx, "child"); return a }())
	ag.mu.Lock()
	ag.err = nil
	ag.mu.Unlock()
	r.st.DeleteApp(ctx, "child") // the store applied the Agent's removal from its outbox
	r.c.AgentRemoved(ctx, "edge", "child")
	if _, err := r.st.GetApp(ctx, "platform"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the App of Apps stays after its last child: %v", err)
	}
}

// Forgetting an App of Apps' last child, while the App of Apps waits to be deleted, finishes it.
func TestForgetLastChildFinishesParent(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	parent := app("platform")
	parent.Kind = domain.KindApps
	r.st.CreateApp(ctx, parent)
	ch := app("child")
	ch.Host, ch.Kind, ch.Parent = "edge", domain.KindCompose, "platform"
	r.st.CreateApp(ctx, ch)
	ag.err = fmt.Errorf("host edge: %w", agents.ErrOffline)
	if err := r.c.DeleteApp(ctx, "platform", true, false); !errors.Is(err, domain.ErrWaitingForHost) {
		t.Fatal(err)
	}
	if err := r.c.ForgetApp(ctx, "child"); err != nil { // its Host is gone for good
		t.Fatal(err)
	}
	if _, err := r.st.GetApp(ctx, "platform"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the App of Apps stays: %v", err)
	}
}

func (s *fakeStore) AgentStatus(_ context.Context, app string) (domain.AgentStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusErr != nil {
		return domain.AgentStatus{}, s.statusErr
	}
	st, ok := s.statuses[app]
	if !ok {
		return st, domain.ErrNotFound
	}
	return st, nil
}

func (s *fakeStore) setStatus(app string, st domain.AgentStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statuses == nil {
		s.statuses = map[string]domain.AgentStatus{}
	}
	s.statuses[app] = st
}

// M4-02: an Agent Host's Applications show what their Agent reported: Health, drift (never re-synced from here, Q82),
// resources and details, logs; offline the last report, with since when the Host is gone and how old it is (Q38).
func TestAgentViews(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	a := app("web")
	a.Host, a.AutoSync, a.SelfHeal = "edge", true, true
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")
	a, _ = r.st.GetApp(ctx, "web") // as created: kind compose
	if v, _ := r.c.GetApp(ctx, "web"); v.Health != domain.HealthUnknown || v.SyncStatus != domain.Synced {
		t.Fatalf("before any report: %s %s", v.Health, v.SyncStatus)
	}
	if live, err := r.c.Resources(ctx, "web"); err != nil || live.Health != domain.HealthUnknown || live.Containers == nil {
		t.Fatalf("resources before any report: %+v %v", live, err)
	}

	reported := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	st := domain.AgentStatus{Revision: "aaaaaaaa", Health: domain.Healthy, At: reported,
		Live: domain.LiveState{Services: []string{"web"}, Health: domain.Healthy,
			Containers: []domain.Container{{ID: "c1", Name: "web-1", Service: "web", State: "running"}},
			Resources: []domain.Resource{{Kind: "volume", Name: "web_data"}, {Kind: "bind", Name: "compose.yaml"},
				{Kind: "env_file", Name: ".env", Secret: true}, {Kind: "bind", Name: "uploads"}}},
		Details: map[string]map[string]string{"volume/web_data": {"driver": "local"}}}
	r.st.setStatus("web", st)
	events, unsub := r.c.Subscribe()
	defer unsub()
	r.c.AgentStatusChanged(ctx, "web")
	if v := awaitEvent(t, events, "app", "web").Data.(domain.AppView); v.Health != domain.Healthy || v.SyncStatus != domain.Synced {
		t.Fatalf("reported Healthy: %s %s", v.Health, v.SyncStatus)
	}
	r.c.AgentStatusChanged(ctx, "nope") // a report for an app the Control Plane doesn't have: nothing

	st.Drifted, st.Drift = true, []domain.ServiceDiff{{Service: "web", Change: "changed", Fields: []string{"image"}}}
	r.st.setStatus("web", st)
	ops, _ := r.st.ListOps(ctx, "web")
	v := r.c.reconcile(ctx, a)
	r.waitIdle(t, "web")
	if v.SyncStatus != domain.OutOfSync || !strings.Contains(v.SyncMessage, "live state drifted") {
		t.Fatalf("drift: %s %q", v.SyncStatus, v.SyncMessage)
	}
	if now, _ := r.st.ListOps(ctx, "web"); len(now) != len(ops) {
		t.Fatal("the Control Plane re-synced drift its Agent heals")
	}
	st.Revision = "older" // drift reported for a Revision no longer deployed: not this one's
	r.st.setStatus("web", st)
	if v := r.c.reconcile(ctx, a); v.SyncStatus != domain.Synced {
		t.Fatalf("an old report's drift: %s", v.SyncStatus)
	}

	live, _ := r.c.Resources(ctx, "web")
	if len(live.Containers) != 1 || len(live.Resources) != 4 {
		t.Fatalf("resources: %+v", live)
	}
	for _, tc := range []struct{ kind, name, want string }{
		{"volume", "web_data", "local"},
		{"bind", "compose.yaml", "services: {}"},
		{"env_file", ".env", "decrypted on the Host"},
		{"bind", "uploads", "on the Host only"},
	} {
		d, err := r.c.ResourceDetail(ctx, "web", tc.kind, tc.name)
		if err != nil || !strings.Contains(fmt.Sprint(d.Content, d.Info), tc.want) {
			t.Fatalf("%s %s: %+v %v", tc.kind, tc.name, d, err)
		}
	}
	var logs strings.Builder
	ag.logs = "line"
	if err := r.c.Logs(ctx, "web", "c1", true, 50, &logs); err != nil || logs.String() != "line edge/web c1 follow=true tail=50" {
		t.Fatalf("logs %q %v", logs.String(), err)
	}

	left := reported.Add(time.Hour)
	r.st.mu.Lock()
	h := r.st.hosts["edge"]
	h.Agent = &domain.AgentInfo{Version: "v0.2.0", OS: "linux", EngineVersion: "29.1.0", LeftAt: left}
	r.st.hosts["edge"] = h
	r.st.mu.Unlock()
	ag.mu.Lock()
	ag.offline = true
	ag.mu.Unlock()
	v = r.c.reconcile(ctx, a)
	if v.Health != domain.Healthy || !strings.Contains(v.SyncMessage, "Host edge offline since "+left.Local().Format(time.DateTime)) ||
		!strings.Contains(v.SyncMessage, "last known state from "+reported.Local().Format(time.DateTime)) || strings.HasPrefix(v.SyncMessage, ";") {
		t.Fatalf("offline: %s %q", v.Health, v.SyncMessage)
	}
	hosts, _ := r.c.ListHosts(ctx)
	i := slices.IndexFunc(hosts, func(h domain.HostView) bool { return h.Name == "edge" })
	if hs := hosts[i].Status; hs.Connected || hs.ServerVersion != "29.1.0" || hs.OS != "linux" || !strings.Contains(hs.Error, "offline since") {
		t.Fatalf("host card offline: %+v", hs)
	}
	ag.mu.Lock()
	ag.offline = false
	ag.mu.Unlock()
	hosts, _ = r.c.ListHosts(ctx)
	if hs := hosts[i].Status; !hs.Connected || hs.Error != "" {
		t.Fatalf("host card online: %+v", hs)
	}
}

// A file's details for an Agent Host's app, when its content can't be had: never deployed, the render or the blob
// failing; a big file shows its first 256 KiB.
func TestAgentDetailEdges(t *testing.T) {
	r, _ := agentRig(t)
	ctx := t.Context()
	r.g.mu.Lock()
	r.g.appFiles = map[string]string{"big.txt": strings.Repeat("x", maxResourceContent+10)}
	r.g.mu.Unlock()
	a := app("web")
	a.Host, a.Kind = "edge", domain.KindCompose
	r.st.CreateApp(ctx, a)
	r.st.setStatus("web", domain.AgentStatus{Live: domain.LiveState{Resources: []domain.Resource{
		{Kind: "bind", Name: "compose.yaml"}, {Kind: "bind", Name: "big.txt"}}}})
	if _, err := r.c.ResourceDetail(ctx, "web", "bind", "compose.yaml"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("never deployed: %v", err)
	}
	id, _ := r.st.CreateOp(ctx, domain.SyncOperation{App: "web", Revision: "aaaaaaaa"})
	r.st.FinishOp(ctx, id, domain.OpSucceeded, "")
	d, err := r.c.ResourceDetail(ctx, "web", "bind", "big.txt")
	if err != nil || len(d.Content) != maxResourceContent || d.Info["truncated"] == "" {
		t.Fatalf("a big file: %d bytes, %v, %v", len(d.Content), d.Info, err)
	}
	r.g.mu.Lock()
	r.g.blobErr = errors.New("source server down")
	r.g.mu.Unlock()
	if _, err := r.c.ResourceDetail(ctx, "web", "bind", "compose.yaml"); err == nil || err.Error() != "source server down" {
		t.Fatalf("blob: %v", err)
	}
	r.g.mu.Lock()
	r.g.renderErr = errors.New("render failed")
	r.g.mu.Unlock()
	if _, err := r.c.ResourceDetail(ctx, "web", "bind", "compose.yaml"); err == nil || err.Error() != "render failed" {
		t.Fatalf("render: %v", err)
	}
}

func (s *fakeStore) SaveAgentDiff(_ context.Context, app string, d domain.AgentDiff) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.diffErr != nil {
		return s.diffErr
	}
	if s.diffs == nil {
		s.diffs = map[string]domain.AgentDiff{}
	}
	s.diffs[app] = d
	return nil
}

func (s *fakeStore) AgentDiff(_ context.Context, app string) (domain.AgentDiff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.diffs[app]
	if !ok {
		return d, domain.ErrNotFound
	}
	return d, nil
}

// M4-11: the Diff of a Revision not deployed comes from the Agent; offline, the last one it computed, saying when and
// for which Revision; never a Diff from nothing.
func TestAgentDiff(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	a := app("web")
	a.Host = "edge"
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")
	r.c.Sync(ctx, "web", domain.SyncRequest{}) // deployed: aaaaaaaa
	r.waitIdle(t, "web")
	r.g.set("bbbbbbbb")
	ag.diff = []domain.ServiceDiff{{Service: "web", Change: "changed", Fields: []string{"image"}}}

	r.g.mu.Lock()
	r.g.warnings = []string{"web: API_TOKEN is in plain text in compose.yaml"}
	r.g.mu.Unlock()
	dv, err := r.c.Diff(ctx, "web")
	if err != nil || len(dv.Services) != 1 || dv.FromSHA != "aaaaaaaa" || dv.ToSHA != "bbbbbbbb" ||
		!strings.Contains(dv.Desired, "services") || !strings.Contains(dv.Live, "services") ||
		len(dv.Warnings) != 1 || !strings.Contains(dv.Warnings[0], "plain text") {
		t.Fatalf("online: %+v %v", dv, err)
	}
	r.g.mu.Lock()
	r.g.warnings = nil
	r.g.mu.Unlock()
	if slices.Contains(ag.asked, "") || ag.asked[len(ag.asked)-1] != "bbbbbbbb" {
		t.Fatalf("asked about %v", ag.asked)
	}
	if d, _ := r.st.AgentDiff(ctx, "web"); d.Revision != "bbbbbbbb" || len(d.Services) != 1 {
		t.Fatalf("kept %+v", d)
	}

	ag.mu.Lock()
	ag.diffErr = fmt.Errorf("host edge: %w", agents.ErrOffline)
	ag.mu.Unlock()
	if dv, err := r.c.Diff(ctx, "web"); err != nil || len(dv.Services) != 1 ||
		!strings.Contains(strings.Join(dv.Warnings, " "), "Host edge offline: the services' changes as its Agent computed them at") {
		t.Fatalf("offline: %+v %v", dv, err)
	}
	r.g.set("cccccccc")
	if dv, err := r.c.Diff(ctx, "web"); err != nil || !strings.Contains(strings.Join(dv.Warnings, " "), "for Revision bbbbbbb (not the target)") {
		t.Fatalf("offline, another target: %+v %v", dv.Warnings, err)
	}

	b := app("db")
	b.Host = "edge"
	r.c.CreateApp(ctx, b)
	if _, err := r.c.Diff(ctx, "db"); err == nil || !strings.Contains(err.Error(), "never computed a Diff") {
		t.Fatalf("offline without any: %v", err)
	}
	ag.mu.Lock()
	ag.diffErr = errors.New("compose file not found")
	ag.mu.Unlock()
	if _, err := r.c.Diff(ctx, "web"); err == nil || err.Error() != "compose file not found" {
		t.Fatalf("the Agent's error: %v", err)
	}

	ag.mu.Lock()
	ag.diffErr = nil
	ag.mu.Unlock()
	r.g.mu.Lock()
	r.g.renderErr, r.g.renderErrRev = errors.New("gone from Git"), "aaaaaaaa"
	r.g.mu.Unlock()
	if dv, err := r.c.Diff(ctx, "web"); err != nil || dv.Live != "" || len(dv.Services) != 1 {
		t.Fatalf("the deployed Revision unrendered: %+v %v", dv, err)
	}
}

// M4-11: what goes wrong around the Agent's Diff: a Rendered file not named by its hash fails Syncs and Diffs alike, a
// Diff not kept is still shown (and logged), and compose files the Source Server won't serve fail the Diff.
func TestAgentDiffFails(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	var logs bytes.Buffer
	r.c.cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
	a := app("web")
	a.Host = "edge"
	r.c.CreateApp(ctx, a)
	r.waitIdle(t, "web")

	r.g.mu.Lock()
	r.g.badHash = true
	r.g.mu.Unlock()
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	if ops, _ := r.st.ListOps(ctx, "web"); ops[0].Phase != domain.OpFailed || !strings.Contains(ops[0].Message, "file compose.yaml") {
		t.Fatalf("a file not named by its hash deployed: %+v", ops[0])
	}
	if _, err := r.c.Diff(ctx, "web"); err == nil || !strings.Contains(err.Error(), "file compose.yaml") {
		t.Fatalf("a file not named by its hash diffed: %v", err)
	}
	r.g.mu.Lock()
	r.g.badHash = false
	r.g.mu.Unlock()

	ag.diff = []domain.ServiceDiff{{Service: "web", Change: "added"}}
	r.st.mu.Lock()
	r.st.diffErr = errors.New("disk full")
	r.st.mu.Unlock()
	if dv, err := r.c.Diff(ctx, "web"); err != nil || len(dv.Services) != 1 || !strings.Contains(logs.String(), "keep the agent's diff") {
		t.Fatalf("a Diff not kept: %+v %v\n%s", dv, err, logs.String())
	}

	r.g.mu.Lock()
	r.g.blobErr = errors.New("source server unreachable")
	r.g.mu.Unlock()
	if _, err := r.c.Diff(ctx, "web"); err == nil || !strings.Contains(err.Error(), "source server unreachable") {
		t.Fatalf("compose files not served: %v", err)
	}
}

// Q102: adopt may be set on an existing Application, where an Agent refused its first Sync over a hand-started project.
func TestUpdateSetsAdopt(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	a := app("web")
	r.c.CreateApp(ctx, a)
	a.Adopt = true
	if _, err := r.c.UpdateApp(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.st.GetApp(ctx, "web"); !got.Adopt {
		t.Fatal("adopt not kept")
	}
}

// Staging a Revision's files fails, saying why, when there is no temporary folder to stage them in.
func TestStageWithoutTempDir(t *testing.T) {
	r := newRig(t)
	file := filepath.Join(t.TempDir(), "a-file")
	os.WriteFile(file, nil, 0o600)
	t.Setenv("TMPDIR", file)
	if _, _, err := r.c.stage(t.Context(), app("web"), domain.Rendered{}); err == nil {
		t.Fatal("staged without a temporary folder")
	}
}

// Q116, Q118: an Agent's resync lists the Applications it runs that the Control Plane doesn't know; a Sync Operation
// still Running that is neither active on the Agent nor being run here is closed as Unknown; failures are logged.
func TestResyncClosesLostOpsAndListsUnmanaged(t *testing.T) {
	r, _ := agentRig(t)
	ctx := t.Context()
	var logs bytes.Buffer
	r.c.cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
	for _, name := range []string{"web", "db", "busy", "fresh"} { // fresh: never synced
		a := app(name)
		a.Host = "edge"
		r.c.CreateApp(ctx, a)
		r.waitIdle(t, name)
	}
	running := func(app string) int64 {
		id, _ := r.st.CreateOp(ctx, domain.SyncOperation{App: app, Revision: "r", Phase: domain.OpRunning, StartedAt: time.Now()})
		return id
	}
	lost, active, mine := running("web"), running("db"), running("busy")
	r.c.setSyncing("busy", true) // a Sync this Control Plane runs right now
	defer r.c.setSyncing("busy", false)

	r.c.Resynced(ctx, "edge", map[string]string{"web": "t1", "db": "t2", "busy": "t3", "handmade": "t4"}, []uint64{uint64(active)})
	if op, _ := r.st.GetOp(ctx, lost); op.Phase != domain.OpUnknown || !strings.Contains(op.Message, "result unknown") {
		t.Fatalf("lost: %+v", op)
	}
	for _, id := range []int64{active, mine} {
		if op, _ := r.st.GetOp(ctx, id); op.Phase != domain.OpRunning {
			t.Fatalf("op %d closed: %+v", id, op)
		}
	}
	r.st.mu.Lock()
	got := r.st.unmanaged["edge"]
	r.st.mu.Unlock()
	if !slices.Equal(got, []string{"handmade"}) || !strings.Contains(logs.String(), "result is unknown") {
		t.Fatalf("unmanaged %v; logs:\n%s", got, logs.String())
	}

	waiting, _ := r.st.CreateOp(ctx, domain.SyncOperation{App: "db", Revision: "r", Phase: domain.OpWaiting, StartedAt: time.Now()})
	r.st.mu.Lock()
	r.st.phaseErr = errors.New("disk full")
	r.st.mu.Unlock()
	r.c.Resynced(ctx, "edge", nil, []uint64{uint64(active)}) // a waiting Sync the store can't start
	if op, _ := r.st.GetOp(ctx, waiting); op.Phase != domain.OpWaiting || !strings.Contains(logs.String(), "run the sync that waited for the host") {
		t.Fatalf("waiting: %+v\n%s", op, logs.String())
	}
	running("web") // lost again, while the store fails
	r.st.mu.Lock()
	r.st.finishErr, r.st.unmanagedErr = errors.New("disk full"), errors.New("disk full")
	r.st.mu.Unlock()
	r.c.Resynced(ctx, "edge", nil, []uint64{uint64(active)})
	r.st.mu.Lock()
	r.st.opsErr = errors.New("disk full")
	r.st.mu.Unlock()
	r.c.Resynced(ctx, "edge", nil, nil)
	for _, want := range []string{"close a lost sync operation", "record the applications on the host, not managed", "list sync operations"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("no %q:\n%s", want, logs.String())
		}
	}
}

// Q119: an Application created over one its Agent already runs at the target's tree is taken over: its first Sync
// sends nothing and records one Succeeded Sync Operation saying so; later Syncs, and one over another tree, deploy.
func TestTakeover(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	r.st.mu.Lock()
	h := r.st.hosts["edge"]
	h.AgentApps = map[string]string{"web": "tree-aaaaaaaa", "db": "tree-older"}
	r.st.hosts["edge"] = h
	r.st.mu.Unlock()
	for _, name := range []string{"web", "db"} {
		a := app(name)
		a.Host = "edge"
		r.c.CreateApp(ctx, a)
		r.waitIdle(t, name)
		r.c.Sync(ctx, name, domain.SyncRequest{})
		r.waitIdle(t, name)
	}
	ops, _ := r.st.ListOps(ctx, "web")
	if len(ops) != 1 || ops[0].Phase != domain.OpSucceeded || !strings.Contains(ops[0].Message, "taken over") {
		t.Fatalf("web: %+v", ops)
	}
	sent := ag.sent()
	if len(sent) != 1 || sent[0].App != "db" {
		t.Fatalf("sent %d, want only db's (another tree)", len(sent))
	}
	if v := r.c.reconcile(ctx, mustApp(t, r, "web")); v.SyncStatus != domain.Synced || v.DeployedSHA != "aaaaaaaa" {
		t.Fatalf("taken over: %s %q", v.SyncStatus, v.DeployedSHA)
	}
	r.g.set("bbbbbbbb")
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	if sent := ag.sent(); sent[len(sent)-1].App != "web" {
		t.Fatal("a later Sync wasn't sent")
	}

	ghost := app("ghost") // its Host gone from the store after it was created: no takeover, a normal Sync
	ghost.Host = "edge"
	r.c.CreateApp(ctx, ghost)
	r.waitIdle(t, "ghost")
	r.st.mu.Lock()
	delete(r.st.hosts, "edge")
	r.st.mu.Unlock()
	r.c.Sync(ctx, "ghost", domain.SyncRequest{})
	r.waitIdle(t, "ghost")
	if ops, _ := r.st.ListOps(ctx, "ghost"); len(ops) == 0 || strings.Contains(ops[0].Message, "taken over") {
		t.Fatalf("ghost: %+v", ops)
	}
}

func mustApp(t *testing.T, r rig, name string) domain.Application {
	t.Helper()
	a, err := r.st.GetApp(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
