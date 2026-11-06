package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/obleey/omegacd/internal/domain"
)

// Agent deploys Applications on its Host (Q3, Q12): Revision → cache → work dir → decrypted → Compose.
type Agent struct {
	db      *DB
	cache   *Cache
	work    *WorkDir
	secrets *Secrets
	engine  domain.Engine // on this Host only
}

func New(db *DB, cache *Cache, work *WorkDir, secrets *Secrets, engine domain.Engine) *Agent {
	return &Agent{db: db, cache: cache, work: work, secrets: secrets, engine: engine}
}

// application is how internal/engine sees a: a Compose project on the local Host.
func application(a App, r Revision) domain.Application {
	return domain.Application{Name: a.Name, Parent: a.Parent, Kind: domain.KindCompose, ComposeFiles: r.ComposeFiles,
		Profiles: r.Profiles, Prune: a.Prune, PruneVolumes: a.Prune && a.PruneVolumes, SelfHeal: a.SelfHeal}
}

// Deploy syncs a to Revision r: the files it lacks come from fetch, then they are written into its folder and
// decrypted, and the project is rendered (with the decrypted .env) and brought up. Only then is r recorded as
// deployed: a failed Deploy leaves the previous Revision as the one to restore.
func (ag *Agent) Deploy(ctx context.Context, a App, r Revision, fetch func(hash string) ([]byte, error), progress func(domain.ProgressEvent)) error {
	if err := ag.cache.Add(ctx, r, fetch); err != nil {
		return err
	}
	if err := ag.work.Write(ctx, a, r); err != nil {
		return err
	}
	dir := ag.work.Dir(a)
	if _, err := ag.secrets.Decrypt(dir); err != nil {
		return err
	}
	app := application(a, r)
	for _, f := range app.ComposeFilesIn(dir) {
		if _, err := os.Stat(filepath.Join(dir, f)); errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: compose file %q not found in the Revision %s", domain.ErrInvalid, f, r.Revision)
		}
	}
	if err := ag.engine.Validate(ctx, app, dir); err != nil {
		return err
	}
	if err := ag.engine.Sync(ctx, app, dir, domain.SyncOptions{Prune: app.Prune, PruneVolumes: app.PruneVolumes, Progress: progress}); err != nil {
		return err
	}
	a.Revision, a.Dir = r.Revision, dir
	return ag.db.SaveApp(ctx, a)
}

// deployed is a and its deployed Revision, or ErrNotFound.
func (ag *Agent) deployed(ctx context.Context, name string) (App, Revision, error) {
	a, err := ag.db.GetApp(ctx, name)
	if err != nil {
		return a, Revision{}, err
	}
	if a.Revision == "" {
		return a, Revision{}, fmt.Errorf("application %s: nothing deployed: %w", name, domain.ErrNotFound)
	}
	r, err := ag.cache.Revision(ctx, name, a.Revision)
	return a, r, err
}

// Status is what runs for an Application, with its Health.
func (ag *Agent) Status(ctx context.Context, name string) (domain.LiveState, error) {
	a, r, err := ag.deployed(ctx, name)
	if err != nil {
		return domain.LiveState{}, err
	}
	return ag.engine.Status(ctx, application(a, r))
}

// Drift is how the running containers differ from the deployed Revision: empty when they match.
func (ag *Agent) Drift(ctx context.Context, name string) ([]domain.ServiceDiff, error) {
	a, r, err := ag.deployed(ctx, name)
	if err != nil {
		return nil, err
	}
	return ag.engine.Diff(ctx, application(a, r), a.Dir, "")
}

// Logs writes the logs of an Application's containers to w: every container, or a service's (or one container's)
// when service names it, as a LogsRequest does.
func (ag *Agent) Logs(ctx context.Context, name, service string, follow bool, tail int, w io.Writer) error {
	a, r, err := ag.deployed(ctx, name)
	if err != nil {
		return err
	}
	app := application(a, r)
	live, err := ag.engine.Status(ctx, app)
	if err != nil {
		return err
	}
	found := false
	for _, c := range live.Containers {
		if service != "" && c.Service != service && c.ID != service && c.Name != service {
			continue
		}
		found = true
		if err := ag.engine.Logs(ctx, app, c.ID, follow, tail, w); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("application %s: no container for %q: %w", name, service, domain.ErrNotFound)
	}
	return nil
}

// Watch calls changed with an Application's name whenever its containers change: the start of drift detection.
func (ag *Agent) Watch(ctx context.Context, changed func(app string)) error {
	return ag.engine.Watch(ctx, func(_, project string) {
		if project != "" {
			changed(project)
		}
	})
}

// Remove deletes an Application from the Host: its containers and networks (with volumes only when asked), its
// folder and its cached Revisions.
func (ag *Agent) Remove(ctx context.Context, name string, volumes bool) error {
	a, err := ag.db.GetApp(ctx, name)
	if err != nil {
		return err
	}
	if err := ag.engine.Remove(ctx, application(a, Revision{}), volumes); err != nil {
		return err
	}
	if a.Dir != "" {
		if err := os.RemoveAll(a.Dir); err != nil {
			return err
		}
	}
	return ag.db.DeleteApp(ctx, name)
}
