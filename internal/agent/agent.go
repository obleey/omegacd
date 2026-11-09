package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
)

// Agent deploys Applications on its Host (Q3, Q12): Revision → cache → work dir → decrypted → Compose.
type Agent struct {
	db      *DB
	cache   *Cache
	work    *WorkDir
	secrets *Secrets
	engine  domain.Engine // on this Host only
	log     *slog.Logger

	// mu (and lock's file lock, across processes) makes Deploy, Remove, a restore, a Self-Heal and a rollback run one
	// at a time: two at once could bring an app up twice or back after its Remove, or a Self-Heal of the running
	// service undo a rollback run by hand. ponytail: one lock for the whole Host, per-app locks if Syncs ever need to
	// overlap.
	mu sync.Mutex

	recorded  chan struct{} // an outbox entry was recorded: send it now if connected (M3-06)
	deploying sync.Map      // the ops of Desired States being deployed: one sent again (a resync) isn't deployed twice
}

// lock takes the Agent's lock: mu within this process, and an flock on agent.lock next to the database, held by
// every omegacd agent process on the Host (the service, a rollback run by hand). unlock releases both.
func (ag *Agent) lock() (unlock func(), err error) {
	ag.mu.Lock()
	f, err := os.OpenFile(filepath.Join(filepath.Dir(ag.db.path), "agent.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	}
	if err != nil {
		if f != nil {
			f.Close()
		}
		ag.mu.Unlock()
		return nil, fmt.Errorf("agent lock: %w", err)
	}
	return func() { f.Close(); ag.mu.Unlock() }, nil // closing the file releases the flock
}

// New is the Agent; log nil logs nothing.
func New(db *DB, cache *Cache, work *WorkDir, secrets *Secrets, engine domain.Engine, log *slog.Logger) *Agent {
	return &Agent{db: db, cache: cache, work: work, secrets: secrets, engine: engine, log: cmp.Or(log, logging.Discard),
		recorded: make(chan struct{}, 1)}
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
	return ag.deploy(ctx, a, r, fetch, domain.SyncOptions{Progress: progress})
}

// deploy is Deploy with the engine's options: progress and, from a Control Plane, registry credentials (Q85).
func (ag *Agent) deploy(ctx context.Context, a App, r Revision, fetch func(hash string) ([]byte, error), opts domain.SyncOptions) error {
	unlock, err := ag.lock()
	if err != nil {
		return err
	}
	defer unlock()
	ctx = logging.With(ctx, "app", a.Name, "revision", r.Revision)
	ag.log.InfoContext(ctx, "deploy", "files", len(r.Files))
	if err := ag.cache.Add(ctx, r, fetch); err != nil {
		return err
	}
	return ag.apply(ctx, a, r, false, opts)
}

// apply writes a cached Revision into a's folder, decrypts it and brings the project up; recreate re-attaches every
// container's bind mounts (after a restore the files are new). It records r as deployed. Caller holds the lock.
func (ag *Agent) apply(ctx context.Context, a App, r Revision, recreate bool, opts domain.SyncOptions) error {
	if err := ag.work.Write(ctx, a, r); err != nil {
		return err
	}
	dir := ag.work.Dir(a)
	decrypted, err := ag.secrets.Decrypt(dir)
	if err != nil {
		return err
	}
	for _, d := range decrypted { // the file only: neither its values nor its variables' names
		ag.log.DebugContext(ctx, "decrypted", "file", d.Path)
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
	opts.Prune, opts.PruneVolumes, opts.ForceRecreate = app.Prune, app.PruneVolumes, recreate
	err = ag.engine.Sync(ctx, app, dir, opts)
	if err != nil {
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

// Watch calls changed with an Application's name whenever its containers change: the start of drift detection. After
// the engine's event stream reconnects, events may have been lost, so it calls changed for every Application.
func (ag *Agent) Watch(ctx context.Context, changed func(app string)) error {
	return ag.engine.Watch(ctx, func(_, project string) {
		if project != "" {
			changed(project)
			return
		}
		apps, _ := ag.db.ListApps(ctx)
		for _, a := range apps {
			changed(a.Name)
		}
	})
}

// Forget drops an Application from the Agent without touching its containers or folder (app delete --cascade=false):
// no Self-Heal or restore of it any more.
func (ag *Agent) Forget(ctx context.Context, name string) error {
	unlock, err := ag.lock()
	if err != nil { // coverage: unreachable, flock on the Agent's own lock file in its own state dir doesn't fail
		return err
	}
	defer unlock()
	if _, err := ag.db.GetApp(ctx, name); err != nil {
		return err
	}
	return ag.db.DeleteApp(ctx, name)
}

// Remove deletes an Application from the Host: its containers and networks (with volumes only when asked), its
// folder and its cached Revisions.
func (ag *Agent) Remove(ctx context.Context, name string, volumes bool) error {
	unlock, err := ag.lock()
	if err != nil {
		return err
	}
	defer unlock()
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
