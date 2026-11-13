package controller

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
)

// lockedBuffer is a log background Syncs write while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// waitOp waits until app's newest Sync Operation is finished, and returns it.
func waitOp(t *testing.T, r rig, app string) domain.SyncOperation {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		ops, _ := r.st.ListOps(t.Context(), app)
		if len(ops) > 0 && !ops[0].FinishedAt.IsZero() {
			return ops[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: no finished Sync Operation: %+v", app, ops)
		}
	}
}

// Q120–Q122, Q124: what was rendered is kept, files and all; with the Source Server down a Rollback deploys it as it
// was, a new Revision gets no Sync Operation and the status says why, the deployed Revision still running.
func TestPayloadsWithoutSourceServer(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	r.c.CreateApp(ctx, app("web"))
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	first := waitOp(t, r, "web")
	compose := blob.Hash([]byte("services: {}\n"))
	r.st.mu.Lock()
	kept := r.st.renders["tree-aaaaaaaa"]
	r.st.mu.Unlock()
	if len(kept) != 1 || kept[0].SHA256 != compose || !r.c.cfg.Blobs.Has(compose) {
		t.Fatalf("kept %+v, file kept: %v", kept, r.c.cfg.Blobs.Has(compose))
	}
	r.g.set("bbbbbbbb")
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	waitOp(t, r, "web")

	down := fmt.Errorf("source server unreachable: %w", domain.ErrUnavailable)
	r.g.mu.Lock()
	r.g.err, r.g.renderErr, r.g.blobErr = down, down, down
	r.g.mu.Unlock()
	if _, err := r.c.Sync(ctx, "web", domain.SyncRequest{}); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("a new Revision: %v", err)
	}
	if ops, _ := r.st.ListOps(ctx, "web"); len(ops) != 2 {
		t.Fatalf("%d Sync Operations, want no new one", len(ops))
	}
	v, _ := r.c.Refresh(ctx, "web")
	if !strings.Contains(v.SyncMessage, "unreachable") || !strings.Contains(v.SyncMessage, "the deployed Revision bbbbbbb keeps running") {
		t.Fatalf("status: %q", v.SyncMessage)
	}
	if _, err := r.c.Rollback(ctx, "web", first.ID); err != nil {
		t.Fatal(err)
	}
	if op := waitOp(t, r, "web"); op.Phase != domain.OpSucceeded || op.Revision != "aaaaaaaa" || op.TreeHash != "tree-aaaaaaaa" {
		t.Fatalf("rollback: %+v", op)
	}
	if sent := ag.sent(); len(sent) != 3 || len(sent[2].Files) != 1 || sent[2].Checksum != "tree-aaaaaaaa" {
		t.Fatalf("sent %v", sent)
	}
	if b, err := r.c.Blob(ctx, compose); err != nil || string(b) != "services: {}\n" {
		t.Fatalf("a kept file: %q, %v", b, err)
	}
	if _, err := r.c.Blob(ctx, blob.Hash([]byte("never kept"))); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("a file not kept: %v, want the Source Server's error", err)
	}
}

// Q121: a Sync whose files can't be copied fails before its Agent gets anything; a render that can't be kept is
// logged and the Sync goes on (the Source Server renders it again); a store that can't read renders fails the Sync.
func TestPayloadsFailing(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	var logs lockedBuffer
	r.c.cfg.Log = slog.New(slog.NewTextHandler(&logs, nil))
	r.g.mu.Lock()
	r.g.blobErr = errors.New("connection refused")
	r.g.mu.Unlock()
	r.st.mu.Lock()
	r.st.saveErr = errors.New("disk full")
	r.st.mu.Unlock()
	r.c.CreateApp(ctx, app("web"))
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	op := waitOp(t, r, "web")
	if op.Phase != domain.OpFailed || !strings.Contains(op.Message, "keep compose.yaml from the Source Server: connection refused") || len(ag.sent()) != 0 {
		t.Fatalf("%+v, sent %d", op, len(ag.sent()))
	}
	if !strings.Contains(logs.String(), "keep the render") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	r.st.mu.Lock()
	r.st.renderErr = errors.New("database is locked")
	r.st.mu.Unlock()
	if _, err := r.c.Rollback(ctx, "web", op.ID); err == nil {
		t.Fatal("rolled back without its render") // a failed Sync can't be rolled back to either; the store said so first
	}
	if _, err := r.c.renderAt(ctx, mustApp(t, r, "web"), "aaaaaaaa", "tree-aaaaaaaa"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("renderAt: %v", err)
	}
}

// Q123: a resync from an Agent that doesn't run an Application deployed there deploys its deployed Revision again
// from the kept render; one never deployed, one it runs and one being synced are left alone; a restore that can't
// start is logged.
func TestRestoreOnHost(t *testing.T) {
	r, ag := agentRig(t)
	ctx := t.Context()
	var logs lockedBuffer
	r.c.cfg.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, name := range []string{"web", "db", "fresh"} {
		r.c.CreateApp(ctx, app(name))
	}
	r.c.Sync(ctx, "web", domain.SyncRequest{})
	waitOp(t, r, "web")
	r.c.Sync(ctx, "db", domain.SyncRequest{})
	waitOp(t, r, "db")
	r.st.CreateOp(ctx, domain.SyncOperation{App: "fresh", Revision: "r", Phase: domain.OpFailed, StartedAt: time.Now(), FinishedAt: time.Now()})
	r.g.mu.Lock()
	r.g.renderErr = errors.New("the Source Server is down") // the kept render is enough
	r.g.mu.Unlock()

	r.c.Resynced(ctx, "edge", map[string]string{"db": "tree-aaaaaaaa"}, nil) // reinstalled: web is gone
	op := waitOp(t, r, "web")
	if op.Trigger != "restore" || op.Phase != domain.OpSucceeded || op.Revision != "aaaaaaaa" ||
		!strings.Contains(op.Message, "its Agent didn't run it (reinstalled?)") {
		t.Fatalf("web: %+v", op)
	}
	if ops, _ := r.st.ListOps(ctx, "db"); len(ops) != 1 {
		t.Fatalf("db, running on its Host, synced again: %+v", ops)
	}
	if !strings.Contains(logs.String(), "never deployed: nothing to restore") || len(ag.sent()) != 3 {
		t.Fatalf("sent %d; logs:\n%s", len(ag.sent()), logs.String())
	}

	r.st.mu.Lock()
	r.st.renderErr = errors.New("database is locked")
	r.st.mu.Unlock()
	r.c.Resynced(ctx, "edge", map[string]string{"db": "tree-aaaaaaaa"}, nil)
	if !strings.Contains(logs.String(), "restore on its host") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}
