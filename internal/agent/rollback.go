package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
)

// triggerRollback marks a rollback run on the Host (Q25): the Control Plane shows the Application as rolled back on
// the Host and turns its Auto-Sync off.
const triggerRollback = "rollback"

// Cached is an Application's cached Revisions, newest first, and the one deployed.
func (ag *Agent) Cached(ctx context.Context, name string) (revs []Revision, deployed string, err error) {
	a, err := ag.app(ctx, name)
	if err != nil {
		return nil, "", err
	}
	revs, err = ag.db.Revisions(ctx, name)
	return revs, a.Revision, err
}

// Rollback deploys a cached Revision of an Application again, from the cache alone, so it works with the Control
// Plane unreachable (Q25). revision may be a unique prefix of one; one not cached is refused with those that are.
// The rollback goes to the outbox like any Sync the Agent runs itself.
func (ag *Agent) Rollback(ctx context.Context, name, revision string) error {
	unlock, err := ag.lock()
	if err != nil {
		return err
	}
	defer unlock()
	a, err := ag.app(ctx, name)
	if err != nil {
		return err
	}
	revs, err := ag.db.Revisions(ctx, name)
	if err != nil {
		return err
	}
	r, err := pick(revs, revision)
	if err != nil {
		return fmt.Errorf("application %s: %w", name, err)
	}
	ctx = logging.With(ctx, "app", name, "revision", r.Revision)
	ag.log.InfoContext(ctx, "rollback", "from", a.Revision)
	return ag.report(ctx, a, r, triggerRollback, func() error { return ag.apply(ctx, a, r, false, nil) })
}

// app is the Application name on this Host; ErrNotFound says so, and how to see those there are.
func (ag *Agent) app(ctx context.Context, name string) (App, error) {
	a, err := ag.db.GetApp(ctx, name)
	if errors.Is(err, domain.ErrNotFound) {
		return a, fmt.Errorf("%w: no Application %s on this Host (omegacd agent rollback lists them)", domain.ErrNotFound, name)
	}
	return a, err
}

// pick is the cached Revision revision names, exactly or as a unique prefix.
func pick(revs []Revision, revision string) (Revision, error) {
	var found []Revision
	for _, r := range revs {
		switch {
		case r.Revision == revision:
			return r, nil
		case revision != "" && strings.HasPrefix(r.Revision, revision):
			found = append(found, r)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return Revision{}, fmt.Errorf("%w: revision %q is not in the cache; cached: %s", domain.ErrNotFound, revision, list(revs))
	}
	return Revision{}, fmt.Errorf("%w: %q matches %d cached revisions, give more of it: %s", domain.ErrInvalid, revision, len(found), list(found))
}

func list(revs []Revision) string {
	if len(revs) == 0 {
		return "none"
	}
	var s []string
	for _, r := range revs {
		s = append(s, r.Revision)
	}
	return strings.Join(s, ", ")
}
