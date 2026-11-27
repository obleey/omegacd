package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
)

var errLocked = errors.New("database is locked")

// logTo has the Controller log to a buffer, at debug, with what its context carries (app, op), as omegacd logs.
func logTo(r rig) *logging.Buffer {
	logs := &logging.Buffer{}
	r.c.cfg.Log, _ = logging.New(logs, "debug", "text")
	return logs
}

func waitLog(t *testing.T, logs *logging.Buffer, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), want); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never said %q:\n%s", want, logs.String())
		}
	}
}

// withParent is a rig with an App of Apps "platform" whose definitions declare "child" on edge.
func withParent(t *testing.T) rig {
	t.Helper()
	r := newRig(t)
	r.g.define("aaaaaaaa", "name: child\nrepoURL: x\nhost: edge\n")
	if _, err := r.c.CreateApp(t.Context(), parentApp("platform")); err != nil {
		t.Fatal(err)
	}
	return r
}

// A store that can't list the Applications: what asked fails with its error, what runs on its own logs it (Q132).
func TestApplicationsUnlisted(t *testing.T) {
	r := withParent(t)
	ctx := t.Context()
	logs := logTo(r)
	parent := mustApp(t, r, "platform")
	r.st.breaks("ListApps", errLocked)
	for name, err := range map[string]error{
		"list":          func() error { _, err := r.c.ListApps(ctx); return err }(),
		"hosts":         func() error { _, err := r.c.ListHosts(ctx); return err }(),
		"delete host":   r.c.DeleteHost(ctx, "edge"),
		"rename host":   r.c.RenameHost(ctx, "edge", "edge-1"),
		"refresh":       func() error { _, err := r.c.Refresh(ctx, "platform"); return err }(),
		"delete parent": r.c.DeleteApp(ctx, "platform", true, false),
		"sync children": r.c.syncChildren(ctx, parent, nil),
		"child diff":    func() error { _, err := r.c.childDiff(ctx, "platform", nil); return err }(),
	} {
		if !errors.Is(err, errLocked) {
			t.Errorf("%s: %v", name, err)
		}
	}
	r.c.reconcileAll(ctx)
	r.c.RepoChanged(ctx, "https://example.com/r.git")
	r.c.syncPendingChildren(ctx, "platform")
	r.c.reconcile(ctx, parent)
	r.c.Resynced(ctx, "edge", nil, nil)
	r.c.refreshParent(ctx, "platform")
	r.c.pendingChildren(ctx, parent, "aaaaaaaa", "tree-other")
	for _, want := range []string{
		`level=ERROR msg="list applications" err="database is locked"`,
		`level=ERROR msg="list child applications" app=platform err="database is locked"`,
		`level=WARN msg="list child applications" err="database is locked" app=platform`,
		`level=DEBUG msg="app of apps' health not refreshed" app=platform err="database is locked"`,
		`level=DEBUG msg="pending children not described" app=platform err="database is locked"`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no %s", want)
		}
	}
	r.st.breaks("ListApps", nil)
	r.st.breaks("ListHosts", errLocked)
	if _, err := r.c.ListHosts(ctx); !errors.Is(err, errLocked) {
		t.Errorf("hosts: %v", err)
	}
	if t.Failed() {
		t.Log(logs.String())
	}
}

// An Application that isn't there, asked for by name: every request says so.
func TestUnknownApplication(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	for name, err := range map[string]error{
		"update":    func() error { _, err := r.c.UpdateApp(ctx, app("ghost")); return err }(),
		"sync":      func() error { _, err := r.c.Sync(ctx, "ghost", domain.SyncRequest{}); return err }(),
		"rollback":  func() error { _, err := r.c.Rollback(ctx, "ghost", 1); return err }(),
		"refresh":   func() error { _, err := r.c.Refresh(ctx, "ghost"); return err }(),
		"history":   func() error { _, err := r.c.History(ctx, "ghost"); return err }(),
		"diff":      func() error { _, err := r.c.Diff(ctx, "ghost"); return err }(),
		"resources": func() error { _, err := r.c.Resources(ctx, "ghost"); return err }(),
		"detail":    func() error { _, err := r.c.ResourceDetail(ctx, "ghost", "volume", "data"); return err }(),
		"logs":      r.c.Logs(ctx, "ghost", "", false, 1, nil),
		"start":     func() error { _, err := r.c.startSync(app("ghost"), "aaaaaaaa", "", "manual"); return err }(),
	} {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	r.c.CreateApp(ctx, app("web"))
	if _, err := r.c.ResourceDetail(ctx, "web", "volume", "data"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a resource it doesn't have: %v", err)
	}
	if _, err := r.c.Rollback(ctx, "web", 99); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a Sync Operation that isn't there: %v", err)
	}
}

// Writes the store refuses fail what asked; those a Sync makes on its own are logged (Q132).
func TestWritesRefused(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	logs := logTo(r)
	r.st.breaks("CreateApp", errLocked)
	if _, err := r.c.CreateApp(ctx, app("web")); !errors.Is(err, errLocked) {
		t.Errorf("create: %v", err)
	}
	r.st.breaks("CreateApp", nil)
	r.c.CreateApp(ctx, app("web"))
	op := r.syncNowFinished(t, "web")

	r.st.breaks("UpdateApp", errLocked)
	if _, err := r.c.UpdateApp(ctx, app("web")); !errors.Is(err, errLocked) {
		t.Errorf("update: %v", err)
	}
	if _, err := r.c.Rollback(ctx, "web", op.ID); !errors.Is(err, errLocked) {
		t.Errorf("rollback: %v", err)
	}
	r.st.breaks("UpdateApp", nil)
	a := mustApp(t, r, "web")
	a.RolledBack = true
	r.st.UpdateApp(ctx, a)
	r.st.breaks("UpdateApp", errLocked)
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	waitLog(t, logs, `level=ERROR msg="clear rolled-back flag" err="database is locked"`)
	r.waitIdle(t, "web")
	r.st.breaks("UpdateApp", nil)

	r.st.breaks("SupersedeWaitingOps", errLocked)
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	waitLog(t, logs, `level=ERROR msg="supersede waiting syncs" app=web err="database is locked"`)
	r.waitIdle(t, "web")
	r.st.breaks("SupersedeWaitingOps", nil)

	r.st.mu.Lock()
	r.st.finishErr = errLocked
	r.st.mu.Unlock()
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	waitLog(t, logs, `level=ERROR msg="record sync operation" err="database is locked"`)
	r.waitIdle(t, "web")
	r.st.mu.Lock()
	r.st.finishErr, r.st.phaseErr = nil, errLocked
	r.st.mu.Unlock()
	ag.mu.Lock()
	ag.err = agents.ErrOffline
	ag.mu.Unlock()
	recorded := `level=ERROR msg="record sync operation" err="database is locked"`
	n := strings.Count(logs.String(), recorded)
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	for deadline := time.Now().Add(5 * time.Second); strings.Count(logs.String(), recorded) == n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("a Sync left waiting, unrecorded, not logged:\n%s", logs.String())
		}
	}
	r.waitIdle(t, "web")
	ag.mu.Lock()
	ag.err = nil
	ag.mu.Unlock()

	r.st.breaks("DeleteApp", errLocked)
	if err := r.c.DeleteApp(ctx, "web", true, false); !errors.Is(err, errLocked) {
		t.Errorf("delete: %v", err)
	}
}

// syncNowFinished runs a manual Sync to its end and returns its Sync Operation.
func (r rig) syncNowFinished(t *testing.T, name string) domain.SyncOperation {
	t.Helper()
	if _, err := r.c.Sync(t.Context(), name, domain.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	op := waitOp(t, r, name)
	r.waitIdle(t, name) // written finished before its syncing ends: a Sync right after would be ErrBusy
	return op
}

// What an update may not do, and a delete while a Sync runs.
func TestUpdateAndDeleteRefused(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, app("web"))
	a := app("web")
	a.Kind = domain.KindApps
	if _, err := r.c.UpdateApp(ctx, a); !errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), "kind") {
		t.Errorf("a kind changed: %v", err)
	}
	r.c.setSyncing("web", true)
	if err := r.c.DeleteApp(ctx, "web", true, false); !errors.Is(err, domain.ErrBusy) {
		t.Errorf("delete while syncing: %v", err)
	}
	r.c.setSyncing("web", false)
	a = app("dot")
	a.Path = "."
	if v, err := r.c.CreateApp(ctx, a); err != nil || v.Path != "" {
		t.Errorf("path . is the root: %q, %v", v.Path, err)
	}
	a = app("odd")
	a.Kind = "helm"
	if _, err := r.c.CreateApp(ctx, a); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a kind it doesn't know: %v", err)
	}
}

// A Diff fails with what failed: the Repository, its diff, its render.
func TestDiffFailures(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, app("web"))
	boom := errors.New("unreachable")
	for name, set := range map[string]func(*fakeSource, error){
		"resolve": func(g *fakeSource, err error) { g.err = err },
		"diff":    func(g *fakeSource, err error) { g.diffErr = err },
		"render":  func(g *fakeSource, err error) { g.renderErr = err },
	} {
		r.g.mu.Lock()
		set(r.g, boom)
		r.g.mu.Unlock()
		if _, err := r.c.Diff(ctx, "web"); !errors.Is(err, boom) {
			t.Errorf("%s: %v", name, err)
		}
		r.g.mu.Lock()
		set(r.g, nil)
		r.g.mu.Unlock()
	}
	r.st.mu.Lock()
	r.st.renderErr = errLocked
	r.st.mu.Unlock()
	if err := r.c.apply(ctx, mustApp(t, r, "web"), domain.SyncOperation{Revision: "aaaaaaaa", TreeHash: "tree-x"}); !errors.Is(err, errLocked) ||
		!strings.Contains(err.Error(), "render: ") {
		t.Errorf("a render it can't read: %v", err)
	}
}

// An App of Apps whose children can't be written fails its Sync saying which; its Diff refuses a definition that isn't
// valid, and shows a child Git no longer declares.
func TestAppOfAppsFailures(t *testing.T) {
	r := withParent(t)
	ctx := t.Context()
	parent := mustApp(t, r, "platform")
	defs := []domain.Application{{Name: "child", RepoURL: "x", Host: "edge"}}
	r.st.breaks("CreateApp", errLocked)
	if err := r.c.syncChildren(ctx, parent, defs); !errors.Is(err, errLocked) || !strings.Contains(err.Error(), `application "child"`) {
		t.Errorf("create: %v", err)
	}
	r.st.breaks("CreateApp", nil)
	if err := r.c.syncChildren(ctx, parent, defs); err != nil {
		t.Fatal(err)
	}
	r.st.breaks("UpdateApp", errLocked)
	changed := []domain.Application{{Name: "child", RepoURL: "y", Host: "edge"}}
	if err := r.c.syncChildren(ctx, parent, changed); !errors.Is(err, errLocked) {
		t.Errorf("update: %v", err)
	}
	if err := r.c.syncChildren(ctx, parent, nil); !errors.Is(err, errLocked) { // not in Git: kept, marked so
		t.Errorf("not in Git: %v", err)
	}
	if err := r.c.DeleteApp(ctx, "platform", false, false); !errors.Is(err, errLocked) { // children orphaned: written
		t.Errorf("orphaning: %v", err)
	}
	r.st.breaks("UpdateApp", nil)
	if _, err := r.c.childDiff(ctx, "platform", []domain.Application{{Name: "Bad Name"}}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a definition that isn't valid: %v", err)
	}
	child := mustApp(t, r, "child")
	child.NotInGit = true
	r.st.UpdateApp(ctx, child)
	diffs, err := r.c.childDiff(ctx, "platform", defs)
	if err != nil || len(diffs) != 1 || !strings.Contains(strings.Join(diffs[0].Fields, ","), "notInGit") {
		t.Errorf("a child back in Git: %+v, %v", diffs, err)
	}
}

// Retries come due on Run's tick: an Application deleted meanwhile is skipped, one the store can't read is logged.
func TestRetriesComeDue(t *testing.T) {
	retryTick = 10 * time.Millisecond
	t.Cleanup(func() { retryTick = 5 * time.Second })
	r := newRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := logTo(r)
	r.c.CreateApp(ctx, app("web"))
	r.c.mu.Lock()
	for _, name := range []string{"web", "gone"} {
		s := r.c.state(name)
		s.retries, s.nextRetry = 1, time.Now().Add(-time.Second)
	}
	r.c.mu.Unlock()
	go r.c.Run(ctx)
	waitLog(t, logs, `level=DEBUG msg="retry: the application is gone" app=gone`)
	waitLog(t, logs, `msg=reconciled`)
	r.st.breaks("GetApp", errLocked)
	waitLog(t, logs, `level=ERROR msg="retry: read the application"`)
}

// What only the Agents' callbacks see: a report for an Application deleted meanwhile, and a Sync waiting for a Host
// whose Application is already syncing again (it waits for the next resync).
func TestAgentCallbackEdges(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	logs := logTo(r)
	r.c.AgentReported(ctx, "ghost")
	waitLog(t, logs, `msg="a report for an application not here (deleted meanwhile)" app=ghost`)
	r.c.CreateApp(ctx, app("web"))
	ag.mu.Lock()
	ag.err = agents.ErrOffline
	ag.mu.Unlock()
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	r.c.setSyncing("web", true)
	r.c.Resynced(ctx, "edge", map[string]string{}, nil)
	if op := mustOps(t, r, "web")[0]; op.Phase != domain.OpWaiting {
		t.Fatalf("ran while syncing: %+v", op)
	}
	r.c.setSyncing("web", false)
	// what is deployed can't be read: Unknown, saying why, never "never synced", which Auto-Sync would deploy
	r.st.breaks("LastSucceededOp", errLocked)
	a := mustApp(t, r, "web")
	a.AutoSync = true
	r.st.UpdateApp(ctx, a)
	before := len(mustOps(t, r, "web"))
	if v := r.c.reconcile(ctx, a); v.SyncStatus != domain.Unknown || v.SyncMessage != "the deployed revision can't be read: database is locked" {
		t.Errorf("status: %s %q", v.SyncStatus, v.SyncMessage)
	}
	if len(mustOps(t, r, "web")) != before {
		t.Error("Auto-Sync started a Sync")
	}
}

func mustOps(t *testing.T, r rig, app string) []domain.SyncOperation {
	t.Helper()
	ops, err := r.st.ListOps(t.Context(), app)
	if err != nil || len(ops) == 0 {
		t.Fatalf("%v, %v", ops, err)
	}
	return ops
}

// The server's Warn and Error lines an operator reads when a Host is away or a delete or Diff can't finish (Q132).
func TestOperatorLines(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	logs := logTo(r)
	r.c.CreateApp(ctx, app("web"))
	r.syncNowFinished(t, "web")

	r.g.set("bbbbbbbb") // its Diff can't show what is deployed: it says why and shows the rest
	r.g.mu.Lock()
	r.g.renderErr, r.g.renderErrRev = errors.New("gone from Git"), "aaaaaaaa"
	r.g.mu.Unlock()
	if _, err := r.c.Diff(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	waitLog(t, logs, `level=WARN msg="the deployed revision's compose files" app=web revision=aaaaaaa err="gone from Git"`)
	r.g.mu.Lock()
	r.g.renderErr, r.g.renderErrRev = nil, ""
	r.g.mu.Unlock()

	ag.mu.Lock()
	ag.err = agents.ErrOffline
	ag.mu.Unlock()
	if _, err := r.c.Sync(ctx, "web", domain.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	waitLog(t, logs, `level=WARN msg="sync not finished" phase=Waiting why="the Host's Agent is not connected"`)
	r.waitIdle(t, "web")
	if err := r.c.DeleteApp(ctx, "web", true, false); !errors.Is(err, domain.ErrWaitingForHost) {
		t.Fatal(err)
	}
	waitLog(t, logs, `level=WARN msg="delete waits for the host" app=web host=edge why="the Host's Agent is not connected"`)

	ag.mu.Lock()
	ag.err = errors.New("volume in use")
	ag.mu.Unlock()
	r.c.finishDeleting(ctx, "web")
	waitLog(t, logs, `level=ERROR msg="finish deleting" app=web err="volume in use"`)
}

// Q153: a Host's change is shown live, as its view, with the Applications on it; a Host gone, as deleted; one the store
// can't read is logged. Inviting and deleting a Host show it too.
func TestHostChangedEvents(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	logs := logTo(r)
	r.c.CreateApp(ctx, app("web"))
	events, stop := r.c.Subscribe()
	defer stop()
	next := func() domain.Event {
		t.Helper()
		select {
		case e := <-events:
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
			return domain.Event{}
		}
	}
	r.c.HostChanged(ctx, "edge")
	if e := next(); e.Type != "host" || e.Data.(domain.HostView).Name != "edge" || e.Data.(domain.HostView).Apps[0] != "web" {
		t.Fatalf("%+v", e)
	}
	if _, _, err := r.c.InviteHost(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	if e := next(); e.Type != "host" || e.Data.(domain.HostView).Name != "new" {
		t.Fatalf("invited: %+v", e)
	}
	if err := r.c.DeleteHost(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	if e := next(); e.Type != "host-deleted" || e.Data != "new" {
		t.Fatalf("deleted: %+v", e)
	}
	r.st.breaks("GetHost", errLocked)
	r.c.HostChanged(ctx, "edge")
	r.st.breaks("GetHost", nil)
	r.st.breaks("ListApps", errLocked)
	r.c.HostChanged(ctx, "edge")
	if strings.Count(logs.String(), `level=ERROR msg="a host's change not shown" host=edge err="database is locked"`) != 2 {
		t.Fatalf("logs:\n%s", logs.String())
	}
	r.st.breaks("ListApps", nil)
	r.st.breaks("DeleteHost", errLocked)
	if err := r.c.DeleteHost(ctx, "edge2"); !errors.Is(err, errLocked) {
		t.Errorf("delete: %v", err)
	}

	// renamed (Q162): the old name goes, the new one comes; refused for a bad name and for a Host in use
	if _, _, err := r.c.InviteHost(ctx, "spare"); err != nil {
		t.Fatal(err)
	}
	next()
	if err := r.c.RenameHost(ctx, "spare", " spare-1 "); err != nil {
		t.Fatal(err)
	}
	if e := next(); e.Type != "host-deleted" || e.Data != "spare" {
		t.Fatalf("renamed, the old name: %+v", e)
	}
	if e := next(); e.Type != "host" || e.Data.(domain.HostView).Name != "spare-1" {
		t.Fatalf("renamed, the new name: %+v", e)
	}
	if !strings.Contains(logs.String(), `msg="host renamed" host=spare to=spare-1`) {
		t.Errorf("logs:\n%s", logs.String())
	}
	if err := r.c.RenameHost(ctx, "spare-1", "Not A Name"); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a bad name: %v", err)
	}
	if err := r.c.RenameHost(ctx, "edge", "edge-1"); !errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), "web") {
		t.Errorf("in use: %v", err)
	}
	r.st.breaks("RenameHost", errLocked)
	if err := r.c.RenameHost(ctx, "spare-1", "spare-2"); !errors.Is(err, errLocked) {
		t.Errorf("store: %v", err)
	}
}
