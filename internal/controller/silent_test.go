package controller

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
)

// Q134: an event a subscriber that isn't reading can't take is dropped, saying so at debug; a store that can't read an
// Application's status fails a resource's details instead of showing none.
func TestNothingDroppedSilently(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	logs := &logging.Buffer{}
	r.c.cfg.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, stop := r.c.Subscribe() // never read
	defer stop()
	for range subscriberCh + 1 {
		r.c.emit(domain.Event{Type: "app", App: "web"})
	}
	if !strings.Contains(logs.String(), `msg="event dropped: a subscriber is behind" type=app app=web`) {
		t.Fatalf("logs:\n%s", logs.String())
	}

	r.st.mu.Lock()
	r.st.statusErr = errors.New("database is locked")
	r.st.mu.Unlock()
	if _, err := r.c.agentDetail(ctx, app("web"), domain.ResourceDetail{Resource: domain.Resource{Kind: "volume", Name: "data"}}); err == nil {
		t.Fatal("details without the status that holds them")
	}
}
