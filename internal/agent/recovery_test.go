package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/wire"
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

// Q134: names the Control Plane sends become this Agent's folders, so one that would leave the work dir is refused: a
// Desired State fails (its report says why), a Diff and a removal answer with the error, nothing written.
func TestNamesFromControlPlaneChecked(t *testing.T) {
	ctx := t.Context()
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	defer db.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := wire.Accept(w, r); err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	conn, err := wire.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close() // reports go to the outbox, where the test reads them
	logs := &logging.Buffer{}
	ag := New(db, nil, nil, nil, nil, slog.New(slog.NewTextHandler(logs, nil)))

	ag.desired(ctx, conn, nil, nil, nil, &agentv1.DesiredState{App: "../../etc", Op: 1})
	ag.desired(ctx, conn, nil, nil, nil, &agentv1.DesiredState{App: "web", Parent: "../x", Op: 2})
	entries, err := db.Outbox(ctx)
	if err != nil || len(entries) != 2 {
		t.Fatalf("outbox %v, %v", entries, err)
	}
	for _, e := range entries {
		if r := e.GetSyncOperationReport(); r.Phase != string(domain.OpFailed) || !strings.Contains(r.Message, "name must be") {
			t.Errorf("report %+v", r)
		}
	}
	if _, err := ag.diff(ctx, &agentv1.DesiredState{App: "web/../.."}, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("diff: %v", err)
	}
	ag.removeApp(ctx, conn, 3, &agentv1.RemoveApp{App: ".."})
	if !strings.Contains(logs.String(), `msg="remove application"`) || !strings.Contains(logs.String(), "name must be") {
		t.Errorf("removal: %s", logs.String())
	}
}

// watchEngine is an engine whose event stream comes back once (project ""), as after a reconnect.
type watchEngine struct{ domain.Engine }

func (watchEngine) Watch(_ context.Context, changed func(string)) error { changed(""); return nil }

// Q134: a database that can't list the Applications is logged where the Agent would check them all (the engine's
// events back) and where it reports them all (a new connection), not skipped silently.
func TestListAppsFailingIsLogged(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	db.Close()
	logs := &logging.Buffer{}
	ag := New(db, nil, nil, nil, watchEngine{}, slog.New(slog.NewTextHandler(logs, nil)))
	ag.Watch(ctx, func(string) { t.Error("an Application from a database that can't list them") })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := wire.Accept(w, r)
		if err != nil {
			return
		}
		for _, err := c.Recv(ctx); err == nil; _, err = c.Recv(ctx) {
		}
	}))
	defer srv.Close()
	conn, err := wire.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() { ag.keepInSync(ctx, conn); close(done) }()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), "list applications to report"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("logs:\n%s", logs.String())
		}
	}
	cancel()
	<-done
	if !strings.Contains(logs.String(), "after the engine's events came back: list applications to check") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}
