package agent

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Q117: an Agent an invite just enrolled drops the reports it kept for the Control Plane it reported to before,
// saying how many; with none it says so at debug; a database that fails is logged.
func TestDropOutboxWhenEnrolled(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	ctx := t.Context()
	var logs bytes.Buffer
	ag := New(db, nil, nil, nil, nil, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for _, rev := range []string{"a1", "a2"} {
		if _, err := db.Record(ctx, rollback("web", rev)); err != nil {
			t.Fatal(err)
		}
	}
	ag.dropOutbox(ctx)
	if out, _ := db.Outbox(ctx); len(out) != 0 || !strings.Contains(logs.String(), "reports=2") {
		t.Fatalf("%d left; logs:\n%s", len(out), logs.String())
	}
	ag.dropOutbox(ctx)
	if !strings.Contains(logs.String(), "no reports kept from before") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	db.Close()
	ag.dropOutbox(ctx)
	if !strings.Contains(logs.String(), "clear the outbox after enrolling") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

// Q116: the resync names the Sync Operations being deployed, not the unnumbered ones (a restore's).
func TestResyncNamesWhatIsDeploying(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	defer db.Close()
	ag := New(db, nil, nil, nil, nil, nil)
	ag.deploying.Store(uint64(7), true)
	ag.deploying.Store(uint64(0), true)
	if r := ag.checksums(t.Context()); !slices.Equal(r.Deploying, []uint64{7}) {
		t.Fatalf("deploying %v", r.Deploying)
	}
}
