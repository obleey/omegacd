package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	"github.com/obleey/omegacd/internal/store"
)

func TestHistorySettings(t *testing.T) {
	if n, age, err := historySettings("", ""); err != nil || n != 0 || age != 0 {
		t.Errorf("unset: %d %v %v", n, age, err)
	}
	if n, age, err := historySettings(" 50 ", "720h"); err != nil || n != 50 || age != 720*time.Hour {
		t.Errorf("set: %d %v %v", n, age, err)
	}
	for _, bad := range [][2]string{{"0", ""}, {"ten", ""}, {"", "30d"}, {"", "-1h"}} {
		if _, _, err := historySettings(bad[0], bad[1]); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// waitLog waits until logs hold want.
func waitLog(t *testing.T, logs *logging.Buffer, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), want); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in the logs:\n%s", want, logs.String())
		}
	}
}

// Q112, Q125: retention runs at once and again each interval, saying what it deleted, then the payloads no Sync
// Operation uses go, renders and files; unset, history stays and payloads are still pruned; a database it can't prune
// is logged as an error.
func TestPrune(t *testing.T) {
	payloadGrace = 0
	t.Cleanup(func() { payloadGrace = time.Hour })
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	st.CreateApp(ctx, domain.Application{Name: "web"})
	for range 3 {
		op, _ := st.CreateOp(ctx, domain.SyncOperation{App: "web", Revision: "r", TreeHash: "t1", StartedAt: time.Now()})
		st.FinishOp(ctx, op, domain.OpFailed, "")
	}
	kept, _ := blobs.Put([]byte("kept"))
	gone, _ := blobs.Put([]byte("gone"))
	st.SaveRender(ctx, "t1", []domain.File{{Path: "compose.yaml", SHA256: kept}})
	st.SaveRender(ctx, "t2", []domain.File{{Path: "compose.yaml", SHA256: gone}}) // no Sync Operation uses it
	logs := &logging.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	unset, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { prune(unset, log, st, blobs, 0, 0, 10*time.Millisecond); close(done) }()
	waitLog(t, logs, "payloads: nothing to prune")
	stop()
	<-done
	if _, err := st.Render(ctx, "t2"); !errors.Is(err, domain.ErrNotFound) || blobs.Has(gone) {
		t.Errorf("the unused payload stayed: %v, file %v", err, blobs.Has(gone))
	}
	if _, err := st.Render(ctx, "t1"); err != nil || !blobs.Has(kept) {
		t.Errorf("the used payload went: %v, file %v", err, blobs.Has(kept))
	}
	if ops, _ := st.ListOps(ctx, "web"); len(ops) != 3 || !strings.Contains(logs.String(), "history kept forever") ||
		!strings.Contains(logs.String(), `msg="payloads pruned" renders=1 files=1`) {
		t.Fatalf("%d ops kept; logs:\n%s", len(ops), logs.String())
	}

	done = make(chan struct{})
	go func() { prune(ctx, log, st, blobs, 1, time.Hour, 10*time.Millisecond); close(done) }()
	waitLog(t, logs, "history: nothing to prune")
	if ops, _ := st.ListOps(ctx, "web"); len(ops) != 1 || !strings.Contains(logs.String(), "history pruned") {
		t.Fatalf("%d ops kept; logs:\n%s", len(ops), logs.String())
	}
	st.Close()
	waitLog(t, logs, "prune history")
	waitLog(t, logs, "prune payloads")
	cancel()
	<-done
}
