package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/store"
)

// lockedBuffer is a log written while the test reads it.
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

// Q112: retention runs at once and again each interval, saying what it deleted; unset, it does nothing; a database it
// can't prune is logged as an error.
func TestPruneHistory(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	st.CreateApp(ctx, domain.Application{Name: "web"})
	for range 3 {
		op, _ := st.CreateOp(ctx, domain.SyncOperation{App: "web", Revision: "r", StartedAt: time.Now()})
		st.FinishOp(ctx, op, domain.OpFailed, "")
	}
	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	pruneHistory(ctx, log, st, 0, 0, time.Hour) // unset: returns at once
	if !strings.Contains(logs.String(), "history kept forever") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	done := make(chan struct{})
	go func() { pruneHistory(ctx, log, st, 1, time.Hour, 10*time.Millisecond); close(done) }()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), "nothing to prune"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("logs:\n%s", logs.String())
		}
	}
	if ops, _ := st.ListOps(ctx, "web"); len(ops) != 1 || !strings.Contains(logs.String(), "history pruned") {
		t.Fatalf("%d ops kept; logs:\n%s", len(ops), logs.String())
	}
	st.Close()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), "prune history"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("logs:\n%s", logs.String())
		}
	}
	cancel()
	<-done
}
