package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/domain"
)

func TestPickRevision(t *testing.T) {
	revs := []Revision{{Revision: "a1b2c3d4"}, {Revision: "a1f00000"}, {Revision: "9e8d7c6b"}}
	for _, tc := range []struct{ arg, want string }{{"a1b2c3d4", "a1b2c3d4"}, {"a1b", "a1b2c3d4"}, {"9e", "9e8d7c6b"}} {
		if r, err := pick(revs, tc.arg); err != nil || r.Revision != tc.want {
			t.Errorf("pick(%q) = %q, %v; want %q", tc.arg, r.Revision, err, tc.want)
		}
	}
	if _, err := pick(revs, "a1"); !errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), "a1b2c3d4, a1f00000") {
		t.Errorf("an ambiguous prefix: %v", err)
	}
	for _, arg := range []string{"ffff", ""} {
		if _, err := pick(revs, arg); !errors.Is(err, domain.ErrNotFound) || !strings.Contains(err.Error(), "cached: a1b2c3d4, a1f00000, 9e8d7c6b") {
			t.Errorf("pick(%q): %v", arg, err)
		}
	}
	if _, err := pick(nil, "x"); err == nil || !strings.Contains(err.Error(), "cached: none") {
		t.Errorf("nothing cached: %v", err)
	}
}

// Q25: a Revision that isn't cached is refused before anything changes, with the ones that are; nothing goes to the
// outbox and the deployed Revision stays.
func TestRollbackRefusesWhatIsNotCached(t *testing.T) {
	w, src, _ := newWorkDir(t)
	ag := New(w.db, w.cache, w, NewSecrets(-1), nil, nil) // no engine: refused before it would be used
	ctx := t.Context()
	for _, rev := range []string{"r1", "r2"} {
		if err := w.cache.Add(ctx, src.revision("web", rev, map[string]string{"compose.yaml": "services: {}\n"}), src.fetch); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.db.SaveApp(ctx, App{Name: "web", Revision: "r2"}); err != nil {
		t.Fatal(err)
	}
	err := ag.Rollback(ctx, "web", "r9")
	if !errors.Is(err, domain.ErrNotFound) || !strings.Contains(err.Error(), "cached: r2, r1") {
		t.Fatalf("an uncached Revision: %v", err)
	}
	if err := ag.Rollback(ctx, "api", "r1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unknown Application: %v", err)
	}
	if out, _ := w.db.Outbox(ctx); len(out) != 0 {
		t.Fatalf("a refused rollback went to the outbox: %v", out)
	}
	revs, deployed, err := ag.Cached(ctx, "web")
	if err != nil || deployed != "r2" || len(revs) != 2 || revs[0].Revision != "r2" {
		t.Fatalf("Cached = %v %q %v", revs, deployed, err)
	}
}

// Two Agents on one state dir (the service and a rollback run by hand) take turns: the second waits for the first.
func TestLockAcrossProcesses(t *testing.T) {
	state := t.TempDir()
	open := func() *Agent { return New(openDB(t, filepath.Join(state, "agent.db")), nil, nil, nil, nil, nil) }
	first, second := open(), open()
	unlock, err := first.lock()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func())
	go func() {
		u, err := second.lock()
		if err != nil {
			t.Error(err)
		}
		got <- u
	}()
	select {
	case <-got:
		t.Fatal("both held the lock at once")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case u := <-got:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("the second never got the lock")
	}
}
