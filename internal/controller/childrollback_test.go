package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/domain"
)

// latest is an app's newest Sync Operation, or none.
func latest(r rig, app string) (domain.SyncOperation, int) {
	ops, _ := r.st.ListOps(context.Background(), app)
	if len(ops) == 0 {
		return domain.SyncOperation{}, 0
	}
	l := ops[0]
	for _, op := range ops {
		if op.ID > l.ID {
			l = op
		}
	}
	return l, len(ops)
}

// synced waits for app's latest Sync Operation to have succeeded at rev: by its App of Apps or its own Auto-Sync,
// whichever starts first.
func (r rig) synced(t *testing.T, app, rev string) domain.SyncOperation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		op, _ := latest(r, app)
		if op.Revision == rev && op.Phase == domain.OpSucceeded {
			r.waitIdle(t, app)
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not synced to %s: %+v", app, rev, op)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Q171, Q221: a Child Application is rolled back as any Application; its App of Apps' Syncs leave it rolled back,
// Auto-Sync off and unsynced; a manual Sync of it ends the rollback, Auto-Sync still off, and the next Sync of its App
// of Apps applies Git's settings again.
func TestChildRollback(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	logs := logTo(r)
	r.c.CreateApp(ctx, parentApp("root"))
	defs := "name: web\nrepoURL: x\nhost: edge\nautoSync: true\n"
	r.g.define("aaaaaaaa", defs)
	r.mustSync(t, "root")
	first := r.synced(t, "web", "aaaaaaaa")
	r.g.define("bbbbbbbb", defs)
	r.mustSync(t, "root")
	r.synced(t, "web", "bbbbbbbb")

	if _, err := r.c.Rollback(ctx, "web", first.ID); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t, "web")
	web, _ := r.st.GetApp(ctx, "web")
	op, n := latest(r, "web")
	if !web.RolledBack || web.AutoSync || op.Trigger != "rollback" || op.Revision != "aaaaaaaa" {
		t.Fatalf("rolled back: %+v %+v", web, op)
	}

	r.g.define("cccccccc", defs)
	r.mustSync(t, "root")
	r.waitIdle(t, "web")
	web, _ = r.st.GetApp(ctx, "web")
	if _, now := latest(r, "web"); !web.RolledBack || web.AutoSync || now != n {
		t.Fatalf("its App of Apps' Sync undid the rollback: %+v, %d Sync Operations (was %d)", web, now, n)
	}
	if !strings.Contains(logs.String(), `msg="child application not synced with its App of Apps" app=web parent=root why="rolled back: a manual Sync of it ends that"`) {
		t.Errorf("logs:\n%s", logs.String())
	}

	r.mustSync(t, "web")
	if web, _ = r.st.GetApp(ctx, "web"); web.RolledBack || web.AutoSync {
		t.Fatalf("a manual Sync ends the rollback, Auto-Sync still off: %+v", web)
	}
	r.mustSync(t, "root")
	if web, _ = r.st.GetApp(ctx, "web"); !web.AutoSync {
		t.Fatalf("the next Sync of its App of Apps applies Git's autoSync: %+v", web)
	}
}

// Q167: a child adopting a running stack is created by its App of Apps' Sync but not synced until a manual Sync of it,
// after its Diff; afterwards its App of Apps syncs it like any child. A store that can't tell whether it was ever
// synced holds it back too, logged.
func TestAdoptedChildHeldBack(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	logs := logTo(r)
	r.c.CreateApp(ctx, parentApp("root"))
	defs := "name: taken\nrepoURL: x\nhost: edge\nadopt: true\n"
	r.g.define("aaaaaaaa", defs)
	r.mustSync(t, "root")
	r.waitIdle(t, "taken")
	if _, n := latest(r, "taken"); n != 0 {
		t.Fatalf("an adopted child synced by its App of Apps: %d Sync Operations", n)
	}
	if !strings.Contains(logs.String(), `app=taken parent=root why="adopts a running stack and was never synced: its first Sync is a manual one, after its Diff"`) {
		t.Errorf("logs:\n%s", logs.String())
	}
	r.st.breaks("LastSucceededOp", errLocked)
	r.mustSync(t, "root")
	r.st.breaks("LastSucceededOp", nil)
	if !strings.Contains(logs.String(), `level=ERROR msg="whether a child application was ever synced" app=taken err="database is locked"`) {
		t.Errorf("logs:\n%s", logs.String())
	}

	r.mustSync(t, "taken") // the deliberate first Sync
	r.g.define("bbbbbbbb", defs)
	r.mustSync(t, "root")
	r.waitIdle(t, "taken")
	if op, _ := latest(r, "taken"); op.Trigger != triggerParent || op.Revision != "bbbbbbbb" {
		t.Fatalf("after its first Sync, its App of Apps syncs it: %+v", op)
	}
}
