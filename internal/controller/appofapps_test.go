package controller

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/domain"
)

func parentApp(name string) domain.Application {
	a := app(name)
	a.Kind = domain.KindApps
	return a
}

// syncNow runs a manual Sync to completion and returns its finished Sync Operation.
func (r rig) syncNow(t *testing.T, name string) domain.SyncOperation {
	t.Helper()
	id, err := r.c.Sync(t.Context(), name, domain.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, name)
	op, _ := r.st.GetOp(t.Context(), id)
	return op
}

func (r rig) mustSync(t *testing.T, name string) {
	t.Helper()
	if op := r.syncNow(t, name); op.Phase != domain.OpSucceeded {
		t.Fatalf("sync %s: %s %q", name, op.Phase, op.Message)
	}
}

func TestAppOfAppsCreatesUpdatesAndKeepsChildren(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	p := parentApp("root")
	p.ComposeFiles, p.Host = []string{"x.yaml"}, "elsewhere"
	if v, err := r.c.CreateApp(ctx, p); err != nil || v.ComposeFiles != nil || v.Host != domain.LocalHost {
		t.Fatalf("create parent: %+v %v", v.Application, err)
	}
	r.g.define("aaaaaaaa", "name: web\nrepoURL: x\n---\nname: api\nrepoURL: x\nprofiles: [prod]\n")
	r.mustSync(t, "root")

	web, err := r.st.GetApp(ctx, "web")
	if err != nil || web.Parent != "root" || web.Kind != domain.KindCompose || !slices.Equal(web.ComposeFiles, []string{"compose.yaml"}) {
		t.Fatalf("web: %+v %v", web, err)
	}
	if v, _ := r.c.Refresh(ctx, "root"); v.SyncStatus != domain.Synced {
		t.Fatalf("parent: %s %q", v.SyncStatus, v.SyncMessage)
	}

	r.g.define("bbbbbbbb", "name: web\nrepoURL: x\ntargetRevision: v2\n")
	d, err := r.c.Diff(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ServiceDiff{{Service: "web", Change: "changed", Fields: []string{"targetRevision"}}, {Service: "api", Change: "removed"}}
	if len(d.Services) != 2 || !slices.ContainsFunc(d.Services, func(s domain.ServiceDiff) bool { return s.Service == "api" && s.Change == "removed" }) ||
		!slices.ContainsFunc(d.Services, func(s domain.ServiceDiff) bool { return s.Service == "web" && slices.Equal(s.Fields, want[0].Fields) }) {
		t.Fatalf("diff: %+v", d.Services)
	}

	r.mustSync(t, "root")
	got, _ := r.st.GetApp(ctx, "web")
	if got.TargetRevision != "v2" || !got.CreatedAt.Equal(web.CreatedAt) {
		t.Fatalf("web not updated: %+v", got)
	}
	api, err := r.st.GetApp(ctx, "api")
	if err != nil || !api.NotInGit {
		t.Fatalf("api without Prune should stay, marked NotInGit: %+v %v", api, err)
	}
	if v, _ := r.c.Refresh(ctx, "root"); v.SyncStatus != domain.OutOfSync || v.SyncMessage != "not in Git: api" {
		t.Fatalf("parent: %s %q", v.SyncStatus, v.SyncMessage)
	}

	r.g.define("cccccccc", "name: web\nrepoURL: x\ntargetRevision: v2\n---\nname: api\nrepoURL: x\nprofiles: [prod]\n")
	r.mustSync(t, "root")
	if api, _ = r.st.GetApp(ctx, "api"); api.NotInGit {
		t.Fatal("re-declared child still NotInGit")
	}
}

func TestAppOfAppsPrunesChildren(t *testing.T) {
	r := newRig(t)
	p := parentApp("root")
	p.Prune = true
	r.c.CreateApp(t.Context(), p)
	r.g.define("aaaaaaaa", "name: web\nrepoURL: x\n---\nname: api\nrepoURL: x\n")
	r.mustSync(t, "root")
	r.g.define("bbbbbbbb", "name: web\nrepoURL: x\n")
	r.mustSync(t, "root")
	if _, err := r.st.GetApp(t.Context(), "api"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("pruned child still stored: %v", err)
	}
	if !slices.Equal(r.e.removedApps, []string{"api"}) || !slices.Equal(r.e.removedVols, []bool{false}) {
		t.Fatalf("removed %v, volumes %v", r.e.removedApps, r.e.removedVols)
	}
}

func TestAppOfAppsRejectsInvalidDefinitionsAndAppliesNothing(t *testing.T) {
	for _, tc := range []struct{ name, defs, msg string }{
		{"not owned", "name: api\nrepoURL: x\n---\nname: web\nrepoURL: x\n", `"web" already exists and is not managed by "root"`},
		{"nested", "name: api\nrepoURL: x\n---\nname: sub\nkind: apps\nrepoURL: x\n", "nested App of Apps is not supported"},
		{"unknown host", "name: api\nrepoURL: x\n---\nname: db\nrepoURL: x\nhost: nope\n", `host "nope" does not exist`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.c.CreateApp(t.Context(), app("web"))
			r.c.CreateApp(t.Context(), parentApp("root"))
			r.g.define("aaaaaaaa", tc.defs)
			if op := r.syncNow(t, "root"); op.Phase != domain.OpFailed || !strings.Contains(op.Message, tc.msg) {
				t.Fatalf("op %s %q", op.Phase, op.Message)
			}
			if _, err := r.st.GetApp(t.Context(), "api"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("a child was created by a failed sync")
			}
		})
	}
}

func TestChildIsOwnedByGit(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, parentApp("root"))
	r.g.define("aaaaaaaa", "name: web\nrepoURL: x\n")
	r.mustSync(t, "root")
	r.g.files = nil      // the child's path holds a compose.yaml
	r.mustSync(t, "web") // manual Sync of a child is allowed

	web, _ := r.st.GetApp(ctx, "web")
	web.TargetRevision = "v9"
	if _, err := r.c.UpdateApp(ctx, web); !errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), "change it in Git") {
		t.Fatalf("update child: %v", err)
	}
	if err := r.c.DeleteApp(ctx, "web", true, false); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("delete child: %v", err)
	}
	ops, _ := r.st.ListOps(ctx, "web")
	_, err := r.c.Rollback(ctx, "web", ops[0].ID)
	if !errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), "Child Application web is managed by App of Apps root; pin targetRevision in Git (or roll back root) instead") {
		t.Fatalf("rollback child: %v", err)
	}

	r.g.define("bbbbbbbb", "")
	r.mustSync(t, "root")
	if err := r.c.DeleteApp(ctx, "web", true, false); err != nil {
		t.Fatalf("delete NotInGit child: %v", err)
	}

	p, _ := r.st.GetApp(ctx, "root")
	p.Kind = domain.KindCompose
	if _, err := r.c.UpdateApp(ctx, p); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("kind change: %v", err)
	}
}

func TestAppOfAppsHealthIsWorstChild(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	if v, _ := r.c.CreateApp(ctx, parentApp("root")); v.Health != domain.Missing {
		t.Fatalf("no children: %s", v.Health)
	}
	r.e.health = map[string]domain.HealthStatus{"web": domain.Healthy, "api": domain.Degraded}
	r.g.define("aaaaaaaa", "name: web\nrepoURL: x\n---\nname: api\nrepoURL: x\n")
	r.mustSync(t, "root")
	if v, _ := r.c.Refresh(ctx, "root"); v.Health != domain.Degraded {
		t.Fatalf("health %s", v.Health)
	}

	r.e.mu.Lock()
	r.e.health["api"] = domain.Healthy
	r.e.mu.Unlock()
	r.c.Refresh(ctx, "api") // a child change alone refreshes the parent
	if v, _ := r.c.GetApp(ctx, "root"); v.Health != domain.Healthy {
		t.Fatalf("health after child recovered: %s", v.Health)
	}
	if l, err := r.c.Resources(ctx, "root"); err != nil || len(l.Containers) != 0 || l.Health != domain.Healthy {
		t.Fatalf("resources: %+v %v", l, err)
	}
}

func TestDeleteAppOfApps(t *testing.T) {
	for _, cascade := range []bool{true, false} {
		r := newRig(t)
		ctx := t.Context()
		r.c.CreateApp(ctx, parentApp("root"))
		r.g.define("aaaaaaaa", "name: web\nrepoURL: x\n")
		r.mustSync(t, "root")
		if err := r.c.DeleteApp(ctx, "root", cascade, false); err != nil {
			t.Fatal(err)
		}
		web, err := r.st.GetApp(ctx, "web")
		switch {
		case cascade && (!errors.Is(err, domain.ErrNotFound) || !slices.Equal(r.e.removedApps, []string{"web"})):
			t.Fatalf("cascade: child %v, removed %v", err, r.e.removedApps)
		case !cascade && (err != nil || web.Parent != "" || r.e.removed):
			t.Fatalf("orphan: %+v %v removed=%v", web, err, r.e.removed)
		}
	}
}

// waitOp waits until app has a finished Sync Operation with the given trigger.
func (r rig) waitOp(t *testing.T, app, trigger string) domain.SyncOperation {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		ops, _ := r.st.ListOps(t.Context(), app)
		if len(ops) > 0 && ops[0].Trigger == trigger && ops[0].Phase != domain.OpRunning {
			return ops[0]
		}
	}
	t.Fatalf("%s: no finished %q Sync Operation", app, trigger)
	return domain.SyncOperation{}
}

func TestManualParentSyncDeploysPendingChildren(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, parentApp("root"))
	r.g.define("aaaaaaaa", "name: web\nrepoURL: x\n---\nname: api\nrepoURL: x\n")

	// first manual Sync: both children are new (never synced) and get deployed
	r.mustSync(t, "root")
	for _, n := range []string{"web", "api"} {
		if op := r.waitOp(t, n, triggerParent); op.Phase != domain.OpSucceeded {
			t.Fatalf("%s: %+v", n, op)
		}
	}

	// web's next Sync fails (say, a dependency is missing); api stays Synced
	r.e.mu.Lock()
	r.e.syncErr = errors.New("network web declared as external, but could not be found")
	r.e.mu.Unlock()
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	r.waitIdle(t, "web")
	r.e.mu.Lock()
	r.e.syncErr = nil
	r.e.mu.Unlock()
	apiOps, _ := r.st.ListOps(ctx, "api")

	// a manual Sync of the parent retries web, and leaves the Synced api alone
	r.mustSync(t, "root")
	if op := r.waitOp(t, "web", triggerParent); op.Phase != domain.OpSucceeded {
		t.Fatalf("web was not re-synced by the parent: %+v", op)
	}
	if again, _ := r.st.ListOps(ctx, "api"); len(again) != len(apiOps) {
		t.Fatal("a Synced child was synced again")
	}
}

func TestDeleteAppOfAppsWithVolumes(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, parentApp("root"))
	r.g.define("aaaaaaaa", "name: web\nrepoURL: x\n---\nname: api\nrepoURL: x\n")
	r.mustSync(t, "root")
	if err := r.c.DeleteApp(ctx, "root", true, true); err != nil {
		t.Fatal(err)
	}
	if len(r.e.removedApps) != 2 || !slices.Equal(r.e.removedVols, []bool{true, true}) {
		t.Fatalf("removed %v, volumes %v", r.e.removedApps, r.e.removedVols)
	}
}

func TestRefreshAppOfAppsRefreshesChildren(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.c.CreateApp(ctx, parentApp("root")); err != nil {
		t.Fatal(err)
	}
	r.g.define("aaaaaaaa", "name: web\nrepoURL: https://example.com/r.git\n---\nname: api\nrepoURL: https://example.com/other.git\n")
	r.mustSync(t, "root")
	r.mustSync(t, "web")
	r.mustSync(t, "api")

	// the Git token stops working: a child's own reconcile says why
	r.g.mu.Lock()
	r.g.err = errors.New("fetch https://example.com/r.git: authentication required")
	r.g.mu.Unlock()
	if v, _ := r.c.Refresh(ctx, "web"); v.SyncStatus != domain.Unknown || !strings.Contains(v.SyncMessage, "authentication required") {
		t.Fatalf("web: %s %q", v.SyncStatus, v.SyncMessage)
	}
	// a new token: refreshing the App of Apps brings its children back right away, no poll needed
	r.g.mu.Lock()
	r.g.err = nil
	r.g.mu.Unlock()
	if v, _ := r.c.Refresh(ctx, "root"); v.SyncStatus != domain.Synced {
		t.Fatalf("root: %s %q", v.SyncStatus, v.SyncMessage)
	}
	for _, name := range []string{"web", "api"} {
		if v, _ := r.c.GetApp(ctx, name); v.SyncStatus != domain.Synced {
			t.Fatalf("%s after refreshing the App of Apps: %s %q", name, v.SyncStatus, v.SyncMessage)
		}
	}
}
