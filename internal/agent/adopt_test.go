package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
)

// projects answers Project as if the Host ran these projects, or fails.
type projects struct {
	domain.Engine
	err error
}

func (p projects) Project(_ context.Context, name string) (domain.ProjectState, error) {
	return map[string]domain.ProjectState{
		"hand":    {Exists: true},                                           // started by hand
		"theirs":  {Exists: true, Managed: true, Owners: []string{"other"}}, // another OmegaCD's
		"old":     {Exists: true, Managed: true},                            // an OmegaCD before M7-09, not this Agent
		"oldours": {Exists: true, Managed: true},                            // the same, deployed by this Agent
		"mine":    {Exists: true, Managed: true, Owners: []string{"me"}},
		"mixed":   {Exists: true, Managed: true, Owners: []string{"me", "other"}},
	}[name], p.err
}

// Only this Agent's own project is acted on (Q336, Q337): a hand-started one, another OmegaCD's and an unlabelled one
// it didn't deploy are refused unless adopted; adopting one an OmegaCD started recreates its containers, so they carry
// this owner.
func TestClaim(t *testing.T) {
	db := openDB(t, t.TempDir()+"/agent.db")
	ctx := t.Context()
	ag := New(db, nil, nil, nil, projects{}, nil)
	db.SaveApp(ctx, App{Name: "oldours", Revision: "r1"})
	for _, tc := range []struct {
		app      string
		adopt    bool
		want     error
		recreate bool
	}{
		{"new", false, nil, false},
		{"hand", false, domain.ErrUnmanaged, false},
		{"hand", true, nil, false}, // Compose adopts a hand-started project as it is (Q119)
		{"theirs", false, domain.ErrOwned, false},
		{"theirs", true, nil, true},
		{"mixed", false, domain.ErrOwned, false},
		{"mixed", true, nil, true},
		{"old", false, domain.ErrOwned, false},
		{"old", true, nil, true},
		{"oldours", false, nil, false}, // labelled when Compose next recreates it
		{"mine", false, nil, false},
		{"mine", true, nil, false},
	} {
		recreate, err := ag.claim(ctx, App{Name: tc.app, Adopt: tc.adopt}, "me")
		if !errors.Is(err, tc.want) || recreate != tc.recreate {
			t.Errorf("%s adopt=%v: %v, recreate %v", tc.app, tc.adopt, err, recreate)
		}
	}
	if _, err := ag.claim(ctx, App{Name: "theirs"}, "me"); err == nil || !strings.Contains(err.Error(),
		"a Compose project theirs on this Host belongs to another OmegaCD (other): rename the Application, or set adopt: true") {
		t.Errorf("the refusal: %v", err)
	}
	if _, err := New(db, nil, nil, nil, projects{err: errors.New("engine down")}, nil).claim(ctx, App{Name: "new"}, "me"); err == nil {
		t.Error("an engine that can't answer let it deploy")
	}
	db.Close()
	if _, err := ag.claim(ctx, App{Name: "mine"}, "me"); err == nil || errors.Is(err, domain.ErrNotFound) {
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
