package agent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// The files the Agent decrypted are the secret ones: "x.sops.env" became "x.env", ".sops.env" became ".env".
func TestDecryptedFrom(t *testing.T) {
	rendered := []domain.File{{Path: ".sops.env"}, {Path: "app.sops.env"}, {Path: "conf/tls.sops.pem"}, {Path: "conf.txt"}}
	for file, want := range map[string]bool{
		".env": true, "app.env": true, "conf/tls.pem": true, "conf.txt": false, "other.env": false, ".other": false,
	} {
		if got := decryptedFrom(file, rendered); got != want {
			t.Errorf("decryptedFrom(%q) = %v", file, got)
		}
	}
}

// The reporter sends nothing while not connected, and once per burst of changes while connected.
func TestReporter(t *testing.T) {
	reportDelay = 50 * time.Millisecond
	t.Cleanup(func() { reportDelay = 500 * time.Millisecond })
	got := make(chan *agentv1.Envelope, 8)
	conn, _ := peer(t, func(e *agentv1.Envelope) *agentv1.Envelope { got <- e; return nil })
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	defer db.Close()
	ag := New(db, nil, nil, nil, nil, nil)
	ag.reports.soon(context.Background(), "web") // not connected: nothing
	ag.reports.connected(conn)
	for range 3 {
		ag.reports.soon(context.Background(), "web")
	}
	time.Sleep(200 * time.Millisecond)
	if len(got) != 0 { // nothing deployed: no report to send, asked once
		t.Fatalf("sent %d", len(got))
	}
	ag.reports.mu.Lock()
	pending := len(ag.reports.pending)
	ag.reports.mu.Unlock()
	if pending != 0 {
		t.Fatal("a burst left pending")
	}
	if _, err := (chunks{context.Background(), conn, 1}).Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := (chunks{context.Background(), conn, 1}).Write([]byte("x")); err == nil {
		t.Fatal("wrote logs on a closed connection")
	}
}

// failingEngine fails at the step named in fail: "status", "diff" or "files".
type failingEngine struct {
	domain.Engine
	fail string
}

func (f failingEngine) Status(context.Context, domain.Application) (domain.LiveState, error) {
	if f.fail == "status" {
		return domain.LiveState{}, errors.New("engine down")
	}
	return domain.LiveState{}, nil
}

func (f failingEngine) Diff(context.Context, domain.Application, string, string) ([]domain.ServiceDiff, error) {
	if f.fail == "diff" {
		return nil, errors.New("project won't load")
	}
	return nil, nil
}

func (f failingEngine) ProjectFiles(context.Context, domain.Application, string) ([]domain.Resource, error) {
	return nil, errors.New("files unreadable")
}

// An engine that fails while a report is built: no report, the error says why (logged, the next change tries again).
func TestStatusReportEngineFails(t *testing.T) {
	cache, db, _ := openCache(t, 10)
	src := &source{}
	r := src.revision("web", "r1", map[string]string{"compose.yaml": "services: {}\n"})
	if err := cache.Add(t.Context(), r, src.fetch); err != nil {
		t.Fatal(err)
	}
	db.SaveApp(t.Context(), App{Name: "web", Revision: "r1", Dir: t.TempDir()})
	for _, step := range []string{"status", "diff", "files"} {
		ag := New(db, cache, nil, nil, failingEngine{fail: step}, nil)
		if _, err := ag.statusReport(t.Context(), "web"); err == nil {
			t.Errorf("%s fails: a report anyway", step)
		}
	}
}
