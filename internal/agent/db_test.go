package agent

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

func openDB(t *testing.T, path string) *DB {
	t.Helper()
	d, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func rollback(app, rev string) *agentv1.OutboxEntry {
	return &agentv1.OutboxEntry{Event: &agentv1.OutboxEntry_SyncOperationReport{SyncOperationReport: &agentv1.SyncOperationReport{
		App: app, Revision: rev, Phase: string(domain.OpSucceeded), Trigger: "rollback"}}}
}

func TestOutboxOrderAndRestarts(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "agent.db")
	d := openDB(t, path)
	for _, rev := range []string{"a1", "a2", "a3"} {
		if _, err := d.Record(ctx, rollback("web", rev)); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

	d = openDB(t, path) // a restart while offline keeps everything, in order
	revs := func() (out []string) {
		es, err := d.Outbox(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range es {
			if i > 0 && e.Seq <= es[i-1].Seq || e.TimeUnix == 0 {
				t.Fatalf("entry %d: seq %d after %d, recorded %v", i, e.Seq, es[i-1].Seq, e.TimeUnix)
			}
			out = append(out, e.GetSyncOperationReport().Revision)
		}
		return out
	}
	if got := revs(); !slices.Equal(got, []string{"a1", "a2", "a3"}) {
		t.Fatalf("after restart: %v", got)
	}
	es, _ := d.Outbox(ctx)
	if err := d.Ack(ctx, es[1].Seq); err != nil { // the Control Plane has the first two
		t.Fatal(err)
	}
	if _, err := d.Record(ctx, rollback("web", "a4")); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d = openDB(t, path)
	defer d.Close()
	if got := revs(); !slices.Equal(got, []string{"a3", "a4"}) {
		t.Fatalf("after ack and restart: %v; want each entry exactly once", got)
	}
	// a seq is never reused, so an old acknowledgement can't swallow a new entry
	if es, _ := d.Outbox(ctx); es[0].Seq <= 2 || es[1].Seq <= es[0].Seq {
		t.Fatalf("seqs %d, %d", es[0].Seq, es[1].Seq)
	}
}

func TestAppsAndRevisions(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "agent.db")
	d := openDB(t, path)
	if _, err := d.GetApp(ctx, "web"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing app: %v", err)
	}
	web := App{Name: "web", Parent: "root", Revision: "r1", SelfHeal: true, Prune: true}
	for _, a := range []App{web, {Name: "api", Revision: "x"}} {
		if err := d.SaveApp(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	web.Revision = "r2"
	d.SaveApp(ctx, web)
	files := []domain.File{{Path: "compose.yaml", Mode: 0o644, SHA256: "aa"}, {Path: "run.sh", Mode: 0o755, SHA256: "bb"}}
	for _, rev := range []string{"r1", "r2"} {
		if err := d.AddRevision(ctx, Revision{App: "web", Revision: rev, Checksum: "c-" + rev, Files: files,
			ComposeFiles: []string{"compose.yaml"}, Profiles: []string{"prod"}}); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

	d = openDB(t, path)
	defer d.Close()
	got, err := d.GetApp(ctx, "web")
	if err != nil || got != web {
		t.Fatalf("GetApp = %+v, %v; want %+v", got, err, web)
	}
	if apps, _ := d.ListApps(ctx); len(apps) != 2 || apps[0].Name != "api" {
		t.Fatalf("ListApps = %+v", apps)
	}
	revs, err := d.Revisions(ctx, "web")
	if err != nil || len(revs) != 2 || revs[0].Revision != "r2" || !slices.Equal(revs[0].Files, files) ||
		!slices.Equal(revs[0].Profiles, []string{"prod"}) || revs[0].AddedAt.IsZero() {
		t.Fatalf("Revisions = %+v, %v; want r2 first, files intact", revs, err)
	}
	d.DeleteRevision(ctx, "web", "r1")
	if revs, _ := d.Revisions(ctx, "web"); len(revs) != 1 {
		t.Fatalf("after DeleteRevision: %+v", revs)
	}
	d.DeleteApp(ctx, "web")
	if revs, _ := d.Revisions(ctx, "web"); len(revs) != 0 {
		t.Fatal("DeleteApp left cached Revisions")
	}
}

func TestMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	d := openDB(t, path)
	var v int
	d.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) {
		t.Fatalf("user_version %d, want %d", v, len(migrations))
	}
	d.db.Exec(`PRAGMA user_version = 99`) // written by a newer Agent
	d.Close()
	if _, err := OpenDB(path); err == nil {
		t.Fatal("opened a database from a newer Agent")
	}
}
