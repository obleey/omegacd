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

// selfEngine is an engine whose project "omegacd" holds the container cafe1234beef, the Agent's own.
type selfEngine struct {
	domain.Engine
	err error
}

func (r selfEngine) Status(_ context.Context, a domain.Application) (domain.LiveState, error) {
	if a.Name != "omegacd" {
		return domain.LiveState{}, r.err
	}
	return domain.LiveState{Containers: []domain.Container{{ID: "cafe1234beef00", Name: "omegacd-agent-1"}}}, r.err
}

// Q109: an Agent in a container refuses the project it runs in, found by its hostname (the container ID's prefix) or
// its container's name; any other, the Control Plane's included, deploys; outside a container nothing is refused.
func TestRefuseSelf(t *testing.T) {
	ctx := t.Context()
	ag := New(nil, nil, nil, nil, selfEngine{}, nil)
	ag.self = "cafe1234beef"
	if err := ag.refuseSelf(ctx, "omegacd"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("its own project: %v", err)
	}
	if err := ag.refuseSelf(ctx, "controlplane"); err != nil {
		t.Fatalf("another project: %v", err)
	}
	ag.self = "omegacd-agent-1"
	if err := ag.refuseSelf(ctx, "omegacd"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("by its container's name: %v", err)
	}
	ag.engine = selfEngine{err: errors.New("engine down")}
	if err := ag.refuseSelf(ctx, "web"); err == nil {
		t.Fatal("an engine that can't answer let it deploy")
	}
	ag.self = ""
	if err := ag.refuseSelf(ctx, "omegacd"); err != nil {
		t.Fatalf("outside a container: %v", err)
	}
}
