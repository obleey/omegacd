package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// projects answers ProjectExists as if the Host ran an unmanaged project named "hand", or fails.
type projects struct {
	domain.Engine
	err error
}

func (p projects) ProjectExists(_ context.Context, name string) (bool, bool, error) {
	return name == "hand", false, p.err
}

// A first deploy over a hand-started project is refused unless the Application adopts it; with adopt, and for an
// Application this Agent deployed before, nothing is checked.
func TestAdoptable(t *testing.T) {
	db := openDB(t, t.TempDir()+"/agent.db")
	ctx := t.Context()
	ag := New(db, nil, nil, nil, projects{}, nil)
	db.SaveApp(ctx, App{Name: "ours", Revision: "r1"})
	for _, tc := range []struct {
		app   string
		adopt bool
		want  error
	}{
		{"hand", false, domain.ErrUnmanaged},
		{"hand", true, nil}, // a first deploy, adopting: not in this Agent's database yet
		{"ours", false, nil},
		{"new", false, nil},
	} {
		if err := ag.adoptable(ctx, &agentv1.DesiredState{App: tc.app, Adopt: tc.adopt}); !errors.Is(err, tc.want) {
			t.Errorf("%s adopt=%v: %v", tc.app, tc.adopt, err)
		}
	}
	if err := New(db, nil, nil, nil, projects{err: errors.New("engine down")}, nil).adoptable(ctx, &agentv1.DesiredState{App: "new"}); err == nil {
		t.Error("an engine that can't answer let it deploy")
	}
	db.Close()
	if err := ag.adoptable(ctx, &agentv1.DesiredState{App: "new"}); err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a database that can't answer: %v", err)
	}
}
