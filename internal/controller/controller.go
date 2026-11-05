// Package controller reconciles Applications: it computes Sync and Health Status,
// runs Sync Operations, and applies Auto-Sync, Self-Heal and retry policy.
package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/appdef"
	"gopkg.in/yaml.v3"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/secretkey"
)

const (
	maxRetries    = 5
	retryBase     = 5 * time.Second
	retryMax      = 3 * time.Minute
	retryTick     = 5 * time.Second
	syncTimeout   = 30 * time.Minute
	subscriberCh  = 64
	watchDebounce = time.Second
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

type Config struct {
	Store        domain.Store
	Git          domain.Git
	Secrets      domain.Secrets
	MasterKey    *age.X25519Identity // seals stored secrets; also the Host Key of LocalHost
	NoLocalHost  bool                // OMEGACD_LOCAL_HOST=false: no local Docker engine, only remote Hosts
	Engine       domain.Engine
	WorkDir      string
	PollInterval time.Duration
	NormalizeURL func(string) string
	ObserveSync  func(app string, phase domain.OpPhase, d time.Duration) // may be nil
	SelfID       string                                                  // omegacd's own container ID or hostname, to refuse managing itself
	Log          *slog.Logger
}

type appState struct {
	view       domain.AppView
	syncing    bool
	deleting   bool // set before a delete touches containers, so nothing re-syncs the app meanwhile
	targetTree string
	drift      bool
	retries    int
	nextRetry  time.Time
	failedTree string // tree that exhausted its retries; auto-sync waits for a new one
}

type Controller struct {
	cfg  Config
	mu   sync.Mutex
	apps map[string]*appState
	subs map[chan domain.Event]struct{}
}

func New(cfg Config) *Controller {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.NormalizeURL == nil {
		cfg.NormalizeURL = strings.ToLower
	}
	return &Controller{cfg: cfg, apps: map[string]*appState{}, subs: map[chan domain.Event]struct{}{}}
}

// Run reconciles every Application each PollInterval (never when 0: webhooks, Refresh and restarts only)
// and drives retries until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	// restore before watching Docker events, so Self-Heal can't race the restore
	c.restoreWorkDirs(ctx)
	go c.watch(ctx)
	c.reconcileAll(ctx)
	var pollC <-chan time.Time // nil: never fires
	if c.cfg.PollInterval > 0 {
		poll := time.NewTicker(c.cfg.PollInterval)
		defer poll.Stop()
		pollC = poll.C
	}
	retry := time.NewTicker(retryTick)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pollC:
			c.reconcileAll(ctx)
		case <-retry.C:
			for _, name := range c.dueRetries() {
				c.reconcileByName(ctx, name)
			}
		}
	}
}

const triggerRestore = "restore"

func composeFilesPresent(dir string, files []string) bool {
	for _, f := range files {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || !fi.Mode().IsRegular() {
			return false
		}
	}
	return len(files) > 0
}

// restoreWorkDirs re-creates the working files of deployed Applications that are gone, typically
// because WorkDir lives on tmpfs (/run) and the machine rebooted: Docker restarts the containers
// on its own, but their bind mounts then point at missing files. It re-syncs the deployed
// Revision (not the target, so nothing new is deployed) and recreates the containers.
func (c *Controller) restoreWorkDirs(ctx context.Context) {
	apps, err := c.cfg.Store.ListApps(ctx)
	if err != nil {
		c.cfg.Log.Error("list applications", "err", err)
		return
	}
	for _, a := range apps {
		if a.Kind == domain.KindApps || c.cfg.NoLocalHost && cmp.Or(a.Host, domain.LocalHost) == domain.LocalHost {
			continue // nothing to restore into: such an app shows its Host as disabled
		}
		// the compose files are the signal: after a reboot Docker may already have re-created
		// missing bind-mount sources as empty directories, so the dir itself can be non-empty
		if composeFilesPresent(c.appDir(a.Name), a.ComposeFiles) {
			c.dropSopsCopies(a.Name)
			continue
		}
		last, err := c.cfg.Store.LastSucceededOp(ctx, a.Name)
		if err != nil {
			continue // never deployed: nothing to restore
		}
		c.cfg.Log.Warn("working files missing, restoring the deployed revision", "app", a.Name, "revision", short(last.Revision))
		if _, err := c.startSync(a, last.Revision, triggerRestore); err != nil {
			c.cfg.Log.Error("restore", "app", a.Name, "err", err)
		}
	}
}

func (c *Controller) reconcileAll(ctx context.Context) {
	apps, err := c.cfg.Store.ListApps(ctx)
	if err != nil {
		c.cfg.Log.Error("list applications", "err", err)
		return
	}
	for _, a := range apps {
		c.reconcile(ctx, a)
	}
}

func (c *Controller) reconcileByName(ctx context.Context, name string) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err == nil {
		c.reconcile(ctx, a)
	}
}

func (c *Controller) dueRetries() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var due []string
	now := time.Now()
	for name, s := range c.apps {
		if s.retries > 0 && !s.syncing && !s.nextRetry.IsZero() && now.After(s.nextRetry) {
			due = append(due, name)
		}
	}
	return due
}

func (c *Controller) state(name string) *appState {
	s, ok := c.apps[name]
	if !ok {
		s = &appState{}
		c.apps[name] = s
	}
	return s
}

func (c *Controller) appDir(name string) string {
	return filepath.Join(c.cfg.WorkDir, "apps", name)
}

// reconcile recomputes the status of one Application, publishes it, and triggers automatic Syncs.
func (c *Controller) reconcile(ctx context.Context, a domain.Application) domain.AppView {
	return c.reconcileWith(ctx, a, true)
}

// reconcileWith skips the Git fetch when fetch is false and a target is already known,
// so live container changes can be reflected immediately and cheaply.
func (c *Controller) reconcileWith(ctx context.Context, a domain.Application, fetch bool) domain.AppView {
	v := domain.AppView{Application: a, SyncStatus: domain.Unknown, Health: domain.Missing, ReconciledAt: time.Now()}
	var targetTree string
	var drift bool

	last, err := c.cfg.Store.LastSucceededOp(ctx, a.Name)
	deployed := err == nil
	if deployed {
		v.DeployedSHA = last.Revision
	} else if !errors.Is(err, domain.ErrNotFound) {
		v.SyncMessage = err.Error()
	}
	if ops, err := c.cfg.Store.ListOps(ctx, a.Name); err == nil && len(ops) > 0 {
		v.LastOperation = &ops[0]
	}

	var children []domain.Application
	if a.Kind == domain.KindApps {
		if children, err = c.children(ctx, a.Name); err != nil {
			c.cfg.Log.Warn("list child applications", "app", a.Name, "err", err)
		}
		c.mu.Lock()
		v.Health = c.worstHealth(children)
		c.mu.Unlock()
	}

	sha, targetTree, err := c.target(ctx, a, fetch)
	if err == nil {
		v.TargetSHA = sha
	}
	switch {
	case err != nil:
		v.SyncMessage = err.Error()
	case !deployed:
		v.SyncStatus, v.SyncMessage = domain.OutOfSync, "never synced"
	case targetTree != last.TreeHash:
		v.SyncStatus, v.SyncMessage = domain.OutOfSync, "new revision "+short(sha)
	case a.Kind == domain.KindApps:
		var gone []string
		for _, ch := range children {
			if ch.NotInGit {
				gone = append(gone, ch.Name)
			}
		}
		v.SyncStatus = domain.Synced
		if len(gone) > 0 {
			drift = true
			v.SyncStatus, v.SyncMessage = domain.OutOfSync, "not in Git: "+strings.Join(gone, ", ")
		}
	default:
		diffs, derr := c.cfg.Engine.Diff(ctx, a, c.appDir(a.Name), "")
		switch {
		case derr != nil:
			v.SyncMessage = derr.Error()
		default:
			// undeclared services already stopped, networks and volumes are not drift:
			// re-syncing would change nothing without Prune (or, for in-use ones, at all)
			diffs = slices.DeleteFunc(diffs, func(d domain.ServiceDiff) bool {
				switch {
				case d.Kind != "":
					v.PrunePending = append(v.PrunePending, d.Kind+" "+d.Service)
				case d.Change == "removed" && slices.Contains(d.Fields, domain.FieldStopped):
					v.PrunePending = append(v.PrunePending, d.Service)
				default:
					return false
				}
				return true
			})
			if len(diffs) > 0 {
				drift = true
				v.SyncStatus, v.SyncMessage = domain.OutOfSync, "live state drifted: "+describe(diffs)
			} else {
				v.SyncStatus = domain.Synced
			}
		}
	}

	// a Sync would fail on a remote Host without a Host Key: say why, and don't auto-sync into that failure
	var noKey bool
	if v.SyncStatus == domain.OutOfSync && a.Kind != domain.KindApps {
		if _, err := c.syncKeys(ctx, a); err != nil {
			v.SyncMessage, noKey = err.Error(), true
		}
	}

	// after Diff: loading the project tells the engine which services are declared
	if a.Kind != domain.KindApps {
		if live, err := c.cfg.Engine.Status(ctx, a); err == nil {
			v.Health = live.Health
		} else {
			// can't ask the Host: "Missing" would claim the containers are gone
			v.Health = domain.HealthUnknown
			// the most actionable fact; it explains any Sync Status error from the same outage too
			v.SyncMessage = "Host " + cmp.Or(a.Host, domain.LocalHost) + " unreachable: " + err.Error()
			c.cfg.Log.Warn("live state", "app", a.Name, "err", err)
		}
	}

	c.mu.Lock()
	s := c.state(a.Name)
	s.targetTree, s.drift = targetTree, drift
	if v.SyncStatus == domain.Synced {
		s.retries, s.nextRetry = 0, time.Time{}
	}
	if s.syncing {
		// keep showing the running op instead of a half-applied status
		v.LastOperation = s.view.LastOperation
	}
	s.view = v
	trigger := c.autoTrigger(a, s, v)
	if noKey {
		trigger = ""
	}
	c.mu.Unlock()
	c.emit(domain.Event{Type: "app", App: a.Name, Data: v})
	if a.Parent != "" {
		c.refreshParent(ctx, a.Parent)
	}

	if trigger != "" {
		if _, err := c.startSync(a, sha, trigger); err != nil && !errors.Is(err, domain.ErrBusy) {
			c.cfg.Log.Error("auto sync", "app", a.Name, "err", err)
		}
	}
	return v
}

// target resolves the Application's target Revision and its tree hash, from cache when !fetch.
func (c *Controller) target(ctx context.Context, a domain.Application, fetch bool) (sha, tree string, err error) {
	if !fetch {
		c.mu.Lock()
		s := c.state(a.Name)
		sha, tree = s.view.TargetSHA, s.targetTree
		c.mu.Unlock()
		if tree != "" {
			return sha, tree, nil
		}
	}
	if sha, err = c.cfg.Git.Resolve(ctx, a.RepoURL, a.TargetRevision); err != nil {
		return "", "", err
	}
	if tree, err = c.cfg.Git.TreeHash(ctx, a.RepoURL, sha, a.Path); err == nil && tree == "" {
		err = fmt.Errorf("path %q not found at %s", a.Path, short(sha))
	}
	return sha, tree, err
}

// watch reconciles an Application without a Git fetch whenever its containers change,
// debounced so a burst of Docker events causes one reconcile.
func (c *Controller) watch(ctx context.Context) {
	type key struct{ host, project string }
	var mu sync.Mutex
	pending := map[key]bool{}
	err := c.cfg.Engine.Watch(ctx, func(host, project string) {
		k := key{host, project}
		mu.Lock()
		defer mu.Unlock()
		if pending[k] {
			return
		}
		pending[k] = true
		time.AfterFunc(watchDebounce, func() {
			mu.Lock()
			delete(pending, k)
			mu.Unlock()
			c.mu.Lock()
			st, ok := c.apps[project]
			busy := ok && (st.syncing || st.deleting)
			c.mu.Unlock()
			if busy {
				return // the Sync reconciles when it finishes
			}
			if project == "" {
				c.reconcileHost(ctx, host)
				return
			}
			if a, err := c.cfg.Store.GetApp(ctx, project); err == nil && a.Host == host {
				c.reconcileWith(ctx, a, false)
			}
		})
	})
	if err != nil {
		c.cfg.Log.Error("watch docker events", "err", err)
	}
}

// reconcileHost re-checks every compose Application on host without a Git fetch, e.g. after its Docker
// engine was unreachable and container events were missed.
func (c *Controller) reconcileHost(ctx context.Context, host string) {
	apps, err := c.cfg.Store.ListApps(ctx)
	if err != nil {
		c.cfg.Log.Error("list applications", "err", err)
		return
	}
	for _, a := range apps {
		if a.Kind == domain.KindCompose && a.Host == host {
			c.reconcileWith(ctx, a, false)
		}
	}
}

// autoTrigger decides whether policy wants a Sync now. Caller holds c.mu.
func (c *Controller) autoTrigger(a domain.Application, s *appState, v domain.AppView) string {
	if !a.AutoSync || a.RolledBack || s.syncing || s.deleting || v.SyncStatus != domain.OutOfSync {
		return ""
	}
	if s.targetTree == s.failedTree {
		return ""
	}
	if s.retries > 0 && time.Now().Before(s.nextRetry) {
		return ""
	}
	if s.drift {
		// an App of Apps only drifts by children kept without Prune; syncing again would not change that
		if a.SelfHeal && a.Kind != domain.KindApps {
			return "self-heal"
		}
		return ""
	}
	return "auto"
}

// startSync records a Sync Operation and runs it in the background.
func (c *Controller) startSync(a domain.Application, sha, trigger string) (int64, error) {
	ctx := context.Background()
	tree, err := c.cfg.Git.TreeHash(ctx, a.RepoURL, sha, a.Path)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	s := c.state(a.Name)
	if s.syncing || s.deleting {
		c.mu.Unlock()
		return 0, domain.ErrBusy
	}
	s.syncing = true
	c.mu.Unlock()
	// a reconcile that started before a delete finished must not resurrect the app
	if _, err := c.cfg.Store.GetApp(ctx, a.Name); err != nil {
		c.setSyncing(a.Name, false)
		return 0, err
	}

	op := domain.SyncOperation{App: a.Name, Revision: sha, TreeHash: tree, Phase: domain.OpRunning, Trigger: trigger, StartedAt: time.Now()}
	id, err := c.cfg.Store.CreateOp(ctx, op)
	if err != nil {
		c.setSyncing(a.Name, false)
		return 0, err
	}
	op.ID = id
	c.mu.Lock()
	s.view.LastOperation = &op
	c.mu.Unlock()
	c.emit(domain.Event{Type: "op", App: a.Name, Data: op})

	go c.runSync(a, op)
	return id, nil
}

func (c *Controller) setSyncing(name string, v bool) {
	c.mu.Lock()
	c.state(name).syncing = v
	c.mu.Unlock()
}

func (c *Controller) runSync(a domain.Application, op domain.SyncOperation) {
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	err := c.apply(ctx, a, op)

	phase, msg := domain.OpSucceeded, "synced to "+short(op.Revision)
	if err != nil {
		phase, msg = domain.OpFailed, err.Error()
	}
	if ferr := c.cfg.Store.FinishOp(ctx, op.ID, phase, msg); ferr != nil {
		c.cfg.Log.Error("record sync operation", "app", a.Name, "err", ferr)
	}
	if c.cfg.ObserveSync != nil {
		c.cfg.ObserveSync(a.Name, phase, time.Since(op.StartedAt))
	}
	c.cfg.Log.Info("sync finished", "app", a.Name, "revision", short(op.Revision), "trigger", op.Trigger, "phase", phase, "msg", msg)

	c.mu.Lock()
	s := c.state(a.Name)
	s.syncing = false
	switch {
	case err == nil && op.Trigger != "self-heal":
		s.retries, s.nextRetry, s.failedTree = 0, time.Time{}, ""
	// a self-heal counts as an attempt until reconcile sees Synced, so persistent drift backs off instead of looping
	case op.Trigger == "auto" || op.Trigger == "self-heal":
		s.retries++
		if s.retries >= maxRetries {
			s.retries, s.nextRetry, s.failedTree = 0, time.Time{}, op.TreeHash
		} else {
			s.nextRetry = time.Now().Add(backoff(s.retries))
		}
	}
	c.mu.Unlock()

	if done, gerr := c.cfg.Store.GetOp(ctx, op.ID); gerr == nil {
		c.emit(domain.Event{Type: "op", App: a.Name, Data: done})
	}
	// a manual Sync to the target Revision ends a Rollback: drop the "rolled back" flag (Auto-Sync stays as set)
	if err == nil && op.Trigger == "manual" {
		if cur, gerr := c.cfg.Store.GetApp(ctx, a.Name); gerr == nil && cur.RolledBack {
			cur.RolledBack = false
			if uerr := c.cfg.Store.UpdateApp(ctx, cur); uerr != nil {
				c.cfg.Log.Error("clear rolled-back flag", "app", a.Name, "err", uerr)
			}
		}
	}
	if err == nil && a.Kind == domain.KindApps && op.Trigger == "manual" {
		c.syncPendingChildren(ctx, a.Name)
	}
	if fresh, gerr := c.cfg.Store.GetApp(ctx, a.Name); gerr == nil {
		c.reconcile(ctx, fresh)
	}
}

const triggerParent = "app-of-apps"

// syncPendingChildren is what a manual Sync of an App of Apps means to a person: deploy what isn't
// deployed. Children that are OutOfSync or whose last Sync failed get a Sync with a fresh retry
// budget (e.g. after they gave up waiting for a dependency); Synced ones are left alone.
func (c *Controller) syncPendingChildren(ctx context.Context, parent string) {
	children, err := c.children(ctx, parent)
	if err != nil {
		c.cfg.Log.Error("list child applications", "app", parent, "err", err)
		return
	}
	for _, ch := range children {
		if ch.NotInGit {
			continue
		}
		v := c.reconcile(ctx, ch)
		failed := v.LastOperation != nil && v.LastOperation.Phase == domain.OpFailed
		if (v.SyncStatus != domain.OutOfSync && !failed) || v.SyncStatus == domain.Unknown || v.TargetSHA == "" {
			continue
		}
		c.mu.Lock()
		s := c.state(ch.Name)
		s.retries, s.nextRetry, s.failedTree = 0, time.Time{}, ""
		c.mu.Unlock()
		if _, err := c.startSync(ch, v.TargetSHA, triggerParent); err != nil && !errors.Is(err, domain.ErrBusy) {
			c.cfg.Log.Error("sync child application", "app", ch.Name, "parent", parent, "err", err)
		}
	}
}

// apply materialises the Revision into the Application's working dir and brings the project up.
func (c *Controller) apply(ctx context.Context, a domain.Application, op domain.SyncOperation) error {
	if a.Kind == domain.KindCompose {
		if err := c.refuseSelf(ctx, a); err != nil {
			return err
		}
	}
	var keys []domain.HostKey
	if a.Kind != domain.KindApps {
		var err error
		if keys, err = c.syncKeys(ctx, a); err != nil {
			return err
		}
	}
	dir := c.appDir(a.Name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := c.cfg.Git.Export(ctx, a.RepoURL, op.Revision, a.Path, dir); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if a.Kind == domain.KindApps {
		return c.syncChildren(ctx, a, dir)
	}
	if err := c.decrypt(a, dir, keys); err != nil {
		return err
	}
	for _, f := range a.ComposeFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: compose file %q not found in path %q of the repository at %s; check the Application's Path and Compose files",
				domain.ErrInvalid, f, cmp.Or(a.Path, "/"), short(op.Revision))
		}
	}
	if err := c.cfg.Engine.Validate(ctx, a, dir); err != nil {
		return err
	}
	regs, err := c.cfg.Store.ListRegistryCredentials(ctx)
	if err != nil {
		return err
	}
	return c.cfg.Engine.Sync(ctx, a, dir, domain.SyncOptions{
		Prune:         a.Prune,
		PruneVolumes:  a.Prune && a.PruneVolumes,
		ForceRecreate: op.Trigger == triggerRestore,
		Registries:    regs,
		Progress: func(p domain.ProgressEvent) {
			p.OpID = op.ID
			c.emit(domain.Event{Type: "progress", App: a.Name, Data: p})
		},
	})
}

func (c *Controller) refuseSelf(ctx context.Context, a domain.Application) error {
	if c.cfg.SelfID == "" || a.Host != domain.LocalHost {
		return nil
	}
	live, err := c.cfg.Engine.Status(ctx, a)
	if err != nil {
		return nil
	}
	for _, ct := range live.Containers {
		if strings.HasPrefix(ct.ID, c.cfg.SelfID) || ct.Name == c.cfg.SelfID {
			return fmt.Errorf("%w: application %q contains omegacd itself", domain.ErrInvalid, a.Name)
		}
	}
	return nil
}

func backoff(attempt int) time.Duration {
	d := retryBase << (attempt - 1)
	return min(d, retryMax)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func describe(diffs []domain.ServiceDiff) string {
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		parts = append(parts, d.Service+" "+d.Change)
	}
	return strings.Join(parts, ", ")
}

// ---- domain.Controller ----

func (c *Controller) ListApps(ctx context.Context) ([]domain.AppView, error) {
	apps, err := c.cfg.Store.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]domain.AppView, 0, len(apps))
	for _, a := range apps {
		views = append(views, c.cachedView(ctx, a))
	}
	return views, nil
}

func (c *Controller) GetApp(ctx context.Context, name string) (domain.AppView, error) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return domain.AppView{}, err
	}
	return c.cachedView(ctx, a), nil
}

// cachedView returns the last reconciled view with fresh settings, reconciling on first sight.
func (c *Controller) cachedView(ctx context.Context, a domain.Application) domain.AppView {
	c.mu.Lock()
	s, ok := c.apps[a.Name]
	var v domain.AppView
	if ok {
		v = s.view
	}
	c.mu.Unlock()
	if !ok || v.ReconciledAt.IsZero() {
		return c.reconcile(ctx, a)
	}
	v.Application = a
	return v
}

func (c *Controller) CreateApp(ctx context.Context, a domain.Application) (domain.AppView, error) {
	a.Parent, a.NotInGit = "", false
	if err := c.checkNew(ctx, &a); err != nil {
		return domain.AppView{}, err
	}
	if err := c.cfg.Store.CreateApp(ctx, a); err != nil {
		return domain.AppView{}, err
	}
	return c.reconcile(ctx, a), nil
}

// checkNew normalizes an Application about to be created and checks it may be.
func (c *Controller) checkNew(ctx context.Context, a *domain.Application) error {
	if err := normalize(a); err != nil {
		return err
	}
	// an App of Apps deploys nothing, so it needs no Docker engine: it can be created where local is disabled
	if a.Kind == domain.KindCompose {
		if err := c.checkHost(ctx, a.Host); err != nil {
			return err
		}
		exists, managed, err := c.cfg.Engine.ProjectExists(ctx, a.Host, a.Name)
		if err != nil {
			return err
		}
		if exists && !managed && !a.Adopt {
			return domain.ErrUnmanaged
		}
	}
	a.CreatedAt = time.Now()
	a.RolledBack = false
	return nil
}

func (c *Controller) UpdateApp(ctx context.Context, a domain.Application) (domain.AppView, error) {
	old, err := c.cfg.Store.GetApp(ctx, a.Name)
	if err != nil {
		return domain.AppView{}, err
	}
	if old.Parent != "" {
		return domain.AppView{}, fmt.Errorf("%w: managed by App of Apps %q; change it in Git", domain.ErrInvalid, old.Parent)
	}
	a.Kind = cmp.Or(a.Kind, old.Kind)
	if err := normalize(&a); err != nil {
		return domain.AppView{}, err
	}
	if a.Kind != old.Kind {
		return domain.AppView{}, fmt.Errorf("%w: the kind of an Application cannot be changed", domain.ErrInvalid)
	}
	if err := checkUpdate(old, &a); err != nil {
		return domain.AppView{}, err
	}
	if err := c.saveUpdate(ctx, a); err != nil {
		return domain.AppView{}, err
	}
	return c.reconcile(ctx, a), nil
}

// checkUpdate carries over what an update may not change.
func checkUpdate(old domain.Application, a *domain.Application) error {
	if a.Host != old.Host {
		return fmt.Errorf("%w: delete and recreate the Application %q to move it to another Host", domain.ErrInvalid, a.Name)
	}
	a.CreatedAt, a.Adopt = old.CreatedAt, old.Adopt
	a.RolledBack = old.RolledBack && !a.AutoSync
	return nil
}

func (c *Controller) saveUpdate(ctx context.Context, a domain.Application) error {
	if err := c.cfg.Store.UpdateApp(ctx, a); err != nil {
		return err
	}
	// new settings deserve a fresh retry budget: earlier failures may have been caused by the old ones
	c.mu.Lock()
	s := c.state(a.Name)
	s.retries, s.nextRetry, s.failedTree = 0, time.Time{}, ""
	c.mu.Unlock()
	return nil
}

func (c *Controller) DeleteApp(ctx context.Context, name string, cascade, volumes bool) error {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return err
	}
	if a.Parent != "" && !a.NotInGit {
		return fmt.Errorf("%w: managed by App of Apps %q; remove it from Git", domain.ErrInvalid, a.Parent)
	}
	if err := c.deleteApp(ctx, a, cascade, volumes); err != nil {
		return err
	}
	if a.Parent != "" {
		c.refreshParent(ctx, a.Parent)
	}
	return nil
}

// deleteApp deletes a; for an App of Apps, cascade deletes its children too and otherwise orphans them.
// volumes (with cascade) also deletes the project's volumes, the children's included.
func (c *Controller) deleteApp(ctx context.Context, a domain.Application, cascade, volumes bool) error {
	c.mu.Lock()
	s := c.state(a.Name)
	if s.syncing || s.deleting {
		c.mu.Unlock()
		return domain.ErrBusy
	}
	s.deleting = true
	c.mu.Unlock()
	if err := c.deleteAppLocked(ctx, a, cascade, volumes); err != nil {
		c.mu.Lock()
		s.deleting = false
		c.mu.Unlock()
		return err
	}
	return nil
}

func (c *Controller) deleteAppLocked(ctx context.Context, a domain.Application, cascade, volumes bool) error {
	if a.Kind == domain.KindApps {
		children, err := c.children(ctx, a.Name)
		if err != nil {
			return err
		}
		for _, ch := range children {
			if cascade {
				err = c.deleteApp(ctx, ch, true, volumes)
			} else {
				ch.Parent, ch.NotInGit = "", false
				if err = c.cfg.Store.UpdateApp(ctx, ch); err == nil {
					c.reconcileWith(ctx, ch, false)
				}
			}
			if err != nil {
				return fmt.Errorf("application %q: %w", ch.Name, err)
			}
		}
	}
	if cascade {
		if a.Kind == domain.KindCompose {
			if err := c.cfg.Engine.Remove(ctx, a, volumes); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(c.appDir(a.Name)); err != nil {
			return err
		}
		if err := os.Remove(c.decryptedList(a.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := c.cfg.Store.DeleteApp(ctx, a.Name); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.apps, a.Name)
	c.mu.Unlock()
	c.emit(domain.Event{Type: "app-deleted", App: a.Name, Data: a.Name})
	return nil
}

func (c *Controller) Sync(ctx context.Context, name string, req domain.SyncRequest) (int64, error) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return 0, err
	}
	if req.Prune != nil {
		a.Prune = *req.Prune // this Sync only; not saved
	}
	c.cfg.Git.Invalidate(a.RepoURL)
	sha, err := c.cfg.Git.Resolve(ctx, a.RepoURL, a.TargetRevision)
	if err != nil {
		return 0, err
	}
	return c.startSync(a, sha, "manual")
}

func (c *Controller) Rollback(ctx context.Context, name string, opID int64) (int64, error) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return 0, err
	}
	op, err := c.cfg.Store.GetOp(ctx, opID)
	if err != nil {
		return 0, err
	}
	if a.Parent != "" {
		return 0, fmt.Errorf("%w: Child Application %s is managed by App of Apps %s; pin targetRevision in Git (or roll back %s) instead",
			domain.ErrInvalid, name, a.Parent, a.Parent)
	}
	if op.App != name || op.Phase != domain.OpSucceeded {
		return 0, fmt.Errorf("%w: can only roll back to a succeeded sync of this application", domain.ErrInvalid)
	}
	a.AutoSync, a.RolledBack = false, true
	if err := c.cfg.Store.UpdateApp(ctx, a); err != nil {
		return 0, err
	}
	return c.startSync(a, op.Revision, "rollback")
}

// Refresh re-reads Git now, past the fetch cooldown. An App of Apps refreshes its children first, so its own
// status is built from theirs; each Repository is fetched once however many children share it.
func (c *Controller) Refresh(ctx context.Context, name string) (domain.AppView, error) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return domain.AppView{}, err
	}
	c.cfg.Git.Invalidate(a.RepoURL)
	if a.Kind == domain.KindApps {
		children, err := c.children(ctx, a.Name)
		if err != nil {
			return domain.AppView{}, err
		}
		for _, ch := range children {
			if ch.RepoURL != a.RepoURL {
				c.cfg.Git.Invalidate(ch.RepoURL)
			}
		}
		for _, ch := range children {
			c.reconcile(ctx, ch)
		}
	}
	return c.reconcile(ctx, a), nil
}

func (c *Controller) History(ctx context.Context, name string) ([]domain.SyncOperation, error) {
	if _, err := c.cfg.Store.GetApp(ctx, name); err != nil {
		return nil, err
	}
	return c.cfg.Store.ListOps(ctx, name)
}

// Diff shows target vs deployed: a Git diff of the app path, plus the service-level changes
// the target Revision would make to the Live State.
func (c *Controller) Diff(ctx context.Context, name string) (domain.DiffView, error) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return domain.DiffView{}, err
	}
	c.cfg.Git.Invalidate(a.RepoURL)
	to, err := c.cfg.Git.Resolve(ctx, a.RepoURL, a.TargetRevision)
	if err != nil {
		return domain.DiffView{}, err
	}
	dv := domain.DiffView{ToSHA: to}
	if last, err := c.cfg.Store.LastSucceededOp(ctx, name); err == nil {
		dv.FromSHA = last.Revision
	}
	if dv.GitDiff, err = c.cfg.Git.Diff(ctx, a.RepoURL, dv.FromSHA, to, a.Path); err != nil {
		return domain.DiffView{}, err
	}

	var keys []domain.HostKey
	if a.Kind != domain.KindApps {
		if keys, err = c.syncKeys(ctx, a); err != nil {
			return domain.DiffView{}, err
		}
	}
	stage := filepath.Join(c.cfg.WorkDir, "staging", name)
	defer os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o750); err != nil {
		return domain.DiffView{}, err
	}
	if err := c.cfg.Git.Export(ctx, a.RepoURL, to, a.Path, stage); err != nil {
		return domain.DiffView{}, err
	}
	if a.Kind == domain.KindApps {
		dv.Services, err = c.childDiff(ctx, a.Name, stage)
		return dv, err
	}
	if err := c.decrypt(a, stage, keys); err != nil {
		return domain.DiffView{}, err
	}
	if err := c.cfg.Engine.Validate(ctx, a, stage); err != nil {
		return domain.DiffView{}, err
	}
	if dv.Services, err = c.cfg.Engine.Diff(ctx, a, stage, c.appDir(name)); err != nil {
		return domain.DiffView{}, err
	}
	if dv.Desired, err = composeText(a, stage); err != nil {
		return domain.DiffView{}, err
	}
	dv.Warnings = plainSecrets(a, stage)
	// Live: the copy of the deployed Revision on the Host (kept here too for remote Hosts)
	if live := c.appDir(name); dirExists(live) {
		if dv.Live, err = composeText(a, live); err != nil {
			c.cfg.Log.Warn("read live compose files", "app", name, "err", err)
		}
	}
	return dv, nil
}

func (c *Controller) Resources(ctx context.Context, name string) (domain.LiveState, error) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return domain.LiveState{}, err
	}
	if a.Kind == domain.KindApps {
		// its children are Applications of their own, listed with ListApps
		return domain.LiveState{Services: []string{}, Containers: []domain.Container{}, Resources: []domain.Resource{}, Health: c.cachedView(ctx, a).Health}, nil
	}
	live, err := c.cfg.Engine.Status(ctx, a)
	if err != nil {
		return domain.LiveState{}, err
	}
	dir := c.appDir(name)
	if _, err := os.Stat(dir); err != nil {
		return live, nil // never synced
	}
	files, err := c.cfg.Engine.ProjectFiles(ctx, a, dir)
	if err != nil {
		c.cfg.Log.Warn("list project files", "app", name, "err", err)
	}
	secret := c.readDecrypted(name)
	for _, f := range files {
		f.Secret = f.Secret || f.Kind == "secret" || secret[filepath.ToSlash(f.Name)] || decrypted(filepath.Join(dir, f.Name))
		live.Resources = append(live.Resources, f)
	}
	return live, nil
}

const maxResourceContent = 256 << 10

// ResourceDetail only serves Resources listed by Resources, which is what keeps it from reading arbitrary paths.
func (c *Controller) ResourceDetail(ctx context.Context, name, kind, resName string) (domain.ResourceDetail, error) {
	live, err := c.Resources(ctx, name)
	if err != nil {
		return domain.ResourceDetail{}, err
	}
	i := slices.IndexFunc(live.Resources, func(r domain.Resource) bool { return r.Kind == kind && r.Name == resName })
	if i < 0 {
		return domain.ResourceDetail{}, fmt.Errorf("%s %q: %w", kind, resName, domain.ErrNotFound)
	}
	d := domain.ResourceDetail{Resource: live.Resources[i], Info: map[string]string{}}
	if kind == "volume" || kind == "network" {
		a, err := c.cfg.Store.GetApp(ctx, name)
		if err != nil {
			return domain.ResourceDetail{}, err
		}
		d.Info, err = c.cfg.Engine.Inspect(ctx, a, kind, resName)
		return d, err
	}

	path, err := c.resolveInAppDir(name, resName)
	if err != nil {
		return domain.ResourceDetail{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return domain.ResourceDetail{}, err
	}
	d.Info["modified"] = fi.ModTime().Format(time.RFC3339)
	if fi.IsDir() {
		d.Info["type"] = "directory"
		return d, nil
	}
	d.Info["type"] = "file"
	d.Info["size"] = fmt.Sprintf("%d bytes", fi.Size())
	if d.Secret && kind != "env_file" {
		return d, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return domain.ResourceDetail{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxResourceContent+1))
	if err != nil {
		return domain.ResourceDetail{}, err
	}
	if len(b) > maxResourceContent {
		b = b[:maxResourceContent]
		d.Info["truncated"] = "showing the first 256 KiB"
	}
	d.Content = string(b)
	if d.Secret {
		d.Content, d.Masked = maskEnv(d.Content), true
	}
	return d, nil
}

// resolveInAppDir joins rel to the app's working dir and refuses anything resolving outside it, symlinks included.
func (c *Controller) resolveInAppDir(name, rel string) (string, error) {
	dir, err := filepath.EvalSymlinks(c.appDir(name))
	if err != nil {
		return "", err
	}
	outside := func(path string) bool {
		r, err := filepath.Rel(dir, path)
		return err != nil || r == ".." || strings.HasPrefix(r, "../")
	}
	path := filepath.Join(dir, rel)
	if outside(path) {
		return "", fmt.Errorf("%w: %q is outside the working directory", domain.ErrInvalid, rel)
	}
	if path, err = filepath.EvalSymlinks(path); err != nil {
		return "", err
	}
	if outside(path) {
		return "", fmt.Errorf("%w: %q links outside the working directory", domain.ErrInvalid, rel)
	}
	return path, nil
}

// plainSecrets warns about environment values in the compose files that look like secrets and are written out
// in Git instead of coming from ${VAR} (a SOPS-encrypted .env) or an env_file. Files are read as written,
// before interpolation; unreadable ones are skipped (Validate reports them).
func plainSecrets(a domain.Application, dir string) []string {
	var warnings []string
	for _, f := range a.ComposeFiles {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		var doc struct {
			Services map[string]struct {
				Environment yaml.Node `yaml:"environment"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal(b, &doc) != nil {
			continue
		}
		for _, svc := range slices.Sorted(maps.Keys(doc.Services)) {
			env := doc.Services[svc].Environment
			var pairs [][2]string
			switch env.Kind {
			case yaml.MappingNode:
				for i := 0; i+1 < len(env.Content); i += 2 {
					pairs = append(pairs, [2]string{env.Content[i].Value, env.Content[i+1].Value})
				}
			case yaml.SequenceNode:
				for _, n := range env.Content {
					k, v, _ := strings.Cut(n.Value, "=")
					pairs = append(pairs, [2]string{k, v})
				}
			}
			for _, kv := range pairs {
				k, v := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
				if secretkey.Plain(k, v) {
					warnings = append(warnings, fmt.Sprintf("%s: %s is in plain text in %s, readable by anyone with access to the repository; "+
						"put it in a SOPS-encrypted .env and write %s: ${%s}", svc, k, f, k, k))
				}
			}
		}
	}
	return warnings
}

// composeText is the app's compose files in dir as written, one after another when there are several.
func composeText(a domain.Application, dir string) (string, error) {
	var b strings.Builder
	for _, f := range a.ComposeFiles {
		content, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "", err
		}
		if len(a.ComposeFiles) > 1 {
			fmt.Fprintf(&b, "# ---- %s ----\n", f)
		}
		b.Write(content)
		if len(content) > 0 && content[len(content)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// dropSopsCopies tidies a work dir written before *.sops.* copies were deleted after decrypting: each copy whose
// decrypted file is there goes, and that file joins the decrypted list. The list is written first, so a decrypted
// file is never unlisted; a copy without its decrypted file stays.
func (c *Controller) dropSopsCopies(name string) {
	dir := c.appDir(name)
	var copies, plain []string
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() || !strings.Contains(e.Name(), ".sops.") {
			return nil
		}
		out := filepath.Join(filepath.Dir(p), strings.Replace(e.Name(), ".sops.", ".", 1))
		if _, err := os.Stat(out); err == nil {
			copies, plain = append(copies, p), append(plain, out)
		}
		return nil
	})
	if len(copies) == 0 {
		return
	}
	for rel := range c.readDecrypted(name) {
		plain = append(plain, filepath.Join(dir, filepath.FromSlash(rel)))
	}
	slices.Sort(plain)
	if err := c.writeDecrypted(name, dir, slices.Compact(plain)); err != nil {
		c.cfg.Log.Error("record decrypted files", "app", name, "err", err)
		return
	}
	for _, p := range copies {
		if err := os.Remove(p); err != nil {
			c.cfg.Log.Error("remove encrypted copy", "app", name, "file", p, "err", err)
		}
	}
	c.cfg.Log.Info("removed encrypted copies from the work dir", "app", name, "files", len(copies))
}

// decryptedList records which files in the app dir were decrypted from SOPS (their *.sops.* copies are
// deleted after decrypting). It sits next to the app dir, not in it, so Export's pruning and the copy to a
// remote Host never touch it.
func (c *Controller) decryptedList(name string) string {
	return filepath.Join(c.cfg.WorkDir, "apps", "."+name+".decrypted")
}

func (c *Controller) writeDecrypted(name, dir string, written []string) error {
	var b strings.Builder
	for _, p := range written {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b.WriteString(filepath.ToSlash(rel) + "\n")
	}
	list := c.decryptedList(name)
	if err := os.WriteFile(list+".tmp", []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(list+".tmp", list)
}

func (c *Controller) readDecrypted(name string) map[string]bool {
	b, _ := os.ReadFile(c.decryptedList(name))
	set := map[string]bool{}
	for line := range strings.Lines(string(b)) {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set
}

// decrypted reports whether path was written by Secrets.DecryptDir from a sibling "x.sops.ext" — the fallback
// for work dirs from before the decrypted list.
func decrypted(path string) bool {
	entries, _ := os.ReadDir(filepath.Dir(path))
	return slices.ContainsFunc(entries, func(e os.DirEntry) bool {
		return strings.Contains(e.Name(), ".sops.") && strings.Replace(e.Name(), ".sops.", ".", 1) == filepath.Base(path)
	})
}

var envKey = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=`)

// maskEnv keeps only the keys of an env file; any other line could be part of a multi-line value.
func maskEnv(s string) string {
	var out []string
	for line := range strings.Lines(s) {
		if m := envKey.FindStringSubmatch(line); m != nil {
			out = append(out, m[1]+"=••••")
		}
	}
	return strings.Join(out, "\n")
}

func (c *Controller) Logs(ctx context.Context, name, containerID string, follow bool, tail int, w io.Writer) error {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return err
	}
	live, err := c.cfg.Engine.Status(ctx, a)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(live.Containers, func(ct domain.Container) bool { return ct.ID == containerID || ct.Name == containerID }) {
		return fmt.Errorf("container %q: %w", containerID, domain.ErrNotFound)
	}
	return c.cfg.Engine.Logs(ctx, a.Host, containerID, follow, tail, w)
}

func (c *Controller) RepoChanged(ctx context.Context, repoURL string) {
	want := c.cfg.NormalizeURL(repoURL)
	c.cfg.Git.Invalidate(repoURL)
	apps, err := c.cfg.Store.ListApps(ctx)
	if err != nil {
		c.cfg.Log.Error("list applications", "err", err)
		return
	}
	for _, a := range apps {
		if c.cfg.NormalizeURL(a.RepoURL) == want {
			c.reconcile(ctx, a)
		}
	}
}

func (c *Controller) Subscribe() (<-chan domain.Event, func()) {
	ch := make(chan domain.Event, subscriberCh)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		if _, ok := c.subs[ch]; ok {
			delete(c.subs, ch)
			close(ch)
		}
		c.mu.Unlock()
	}
}

// emit fans out to subscribers; slow subscribers drop events rather than block reconciliation.
func (c *Controller) emit(e domain.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ch := range c.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func normalize(a *domain.Application) error {
	a.Name = strings.TrimSpace(a.Name)
	a.RepoURL = strings.TrimSpace(a.RepoURL)
	a.Path = strings.Trim(strings.TrimSpace(a.Path), "/")
	if a.Path == "." {
		a.Path = ""
	}
	a.Kind = cmp.Or(a.Kind, domain.KindCompose)
	if a.Kind != domain.KindCompose && a.Kind != domain.KindApps {
		return fmt.Errorf("%w: kind must be %q or %q", domain.ErrInvalid, domain.KindCompose, domain.KindApps)
	}
	if !nameRE.MatchString(a.Name) {
		return fmt.Errorf("%w: name must be lowercase letters, digits, '-' or '_'", domain.ErrInvalid)
	}
	if a.RepoURL == "" {
		return fmt.Errorf("%w: repoURL is required", domain.ErrInvalid)
	}
	if strings.Contains(a.Path, "..") {
		return fmt.Errorf("%w: path must stay inside the repository", domain.ErrInvalid)
	}
	if a.TargetRevision == "" {
		a.TargetRevision = "main"
	}
	a.Host = cmp.Or(strings.TrimSpace(a.Host), domain.LocalHost)
	// Prune volumes is a sub-option of Prune, and an App of Apps has no volumes of its own
	a.PruneVolumes = a.PruneVolumes && a.Prune && a.Kind == domain.KindCompose
	if a.Kind == domain.KindApps {
		// an App of Apps deploys nothing itself
		a.ComposeFiles, a.Profiles, a.Host = nil, nil, domain.LocalHost
	} else if len(a.ComposeFiles) == 0 {
		a.ComposeFiles = []string{"compose.yaml"}
	}
	for _, f := range a.ComposeFiles {
		if filepath.IsAbs(f) || strings.Contains(f, "..") {
			return fmt.Errorf("%w: compose file %q must be relative to the path", domain.ErrInvalid, f)
		}
	}
	return nil
}

func (c *Controller) checkHost(ctx context.Context, name string) error {
	if name == domain.LocalHost {
		if c.cfg.NoLocalHost {
			return domain.ErrLocalHostDisabled
		}
		return nil
	}
	_, err := c.cfg.Store.GetHost(ctx, name)
	if errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("%w: host %q does not exist", domain.ErrInvalid, name)
	}
	return err
}

// ---- App of Apps ----

func (c *Controller) children(ctx context.Context, parent string) ([]domain.Application, error) {
	apps, err := c.cfg.Store.ListApps(ctx)
	return slices.DeleteFunc(apps, func(a domain.Application) bool { return a.Parent != parent }), err
}

var healthOrder = []domain.HealthStatus{domain.Healthy, domain.Progressing, domain.HealthUnknown, domain.Missing, domain.Degraded}

// worstHealth is the worst last-known Health of children; Missing when there are none. Caller holds c.mu.
func (c *Controller) worstHealth(children []domain.Application) domain.HealthStatus {
	if len(children) == 0 {
		return domain.Missing
	}
	worst := domain.Healthy
	for _, ch := range children {
		h := domain.Missing
		if s, ok := c.apps[ch.Name]; ok && !s.view.ReconciledAt.IsZero() {
			h = s.view.Health
		}
		if slices.Index(healthOrder, h) > slices.Index(healthOrder, worst) {
			worst = h
		}
	}
	return worst
}

// refreshParent recomputes an App of Apps' Health from its children's cached views and publishes it.
func (c *Controller) refreshParent(ctx context.Context, name string) {
	children, err := c.children(ctx, name)
	if err != nil {
		return
	}
	c.mu.Lock()
	s, ok := c.apps[name]
	if !ok || s.view.ReconciledAt.IsZero() {
		c.mu.Unlock()
		return
	}
	s.view.Health = c.worstHealth(children)
	v := s.view
	c.mu.Unlock()
	c.emit(domain.Event{Type: "app", App: name, Data: v})
}

// syncChildren makes parent's Child Applications match the definitions in dir.
// Every definition is validated before anything is changed.
func (c *Controller) syncChildren(ctx context.Context, parent domain.Application, dir string) error {
	defs, err := appdef.Parse(dir)
	if err != nil {
		return err
	}
	apps, err := c.cfg.Store.ListApps(ctx)
	if err != nil {
		return err
	}
	existing := map[string]domain.Application{}
	for _, a := range apps {
		existing[a.Name] = a
	}
	declared := map[string]bool{}
	var creates, updates []domain.Application
	for _, d := range defs {
		d.Parent = parent.Name
		old, ok := existing[d.Name]
		if ok && old.Parent != parent.Name {
			return fmt.Errorf("%w: application %q already exists and is not managed by %q", domain.ErrInvalid, d.Name, parent.Name)
		}
		if !ok {
			err = c.checkNew(ctx, &d)
		} else if err = normalize(&d); err == nil {
			if err = c.checkHost(ctx, d.Host); err == nil {
				err = checkUpdate(old, &d)
			}
		}
		if err != nil {
			return fmt.Errorf("application %q: %w", d.Name, err)
		}
		declared[d.Name] = true
		switch {
		case !ok:
			creates = append(creates, d)
		case old.NotInGit || len(changedFields(old, d)) > 0:
			updates = append(updates, d)
		}
	}

	for _, d := range creates {
		if err := c.cfg.Store.CreateApp(ctx, d); err != nil {
			return fmt.Errorf("application %q: %w", d.Name, err)
		}
	}
	for _, d := range updates {
		if err := c.saveUpdate(ctx, d); err != nil {
			return fmt.Errorf("application %q: %w", d.Name, err)
		}
	}
	for _, a := range apps {
		if a.Parent != parent.Name || declared[a.Name] {
			continue
		}
		if parent.Prune {
			err = c.deleteApp(ctx, a, true, false) // pruning a child never deletes its data
		} else if !a.NotInGit {
			a.NotInGit = true
			if err = c.cfg.Store.UpdateApp(ctx, a); err == nil {
				c.reconcileWith(ctx, a, false)
			}
		}
		if err != nil {
			return fmt.Errorf("application %q: %w", a.Name, err)
		}
	}
	for _, d := range slices.Concat(creates, updates) {
		c.reconcile(ctx, d)
	}
	return nil
}

// childDiff compares the definitions in dir with parent's stored Child Applications.
func (c *Controller) childDiff(ctx context.Context, parent, dir string) ([]domain.ServiceDiff, error) {
	defs, err := appdef.Parse(dir)
	if err != nil {
		return nil, err
	}
	children, err := c.children(ctx, parent)
	if err != nil {
		return nil, err
	}
	diffs := []domain.ServiceDiff{}
	declared := map[string]bool{}
	for _, d := range defs {
		if err := normalize(&d); err != nil {
			return nil, fmt.Errorf("application %q: %w", d.Name, err)
		}
		declared[d.Name] = true
		i := slices.IndexFunc(children, func(ch domain.Application) bool { return ch.Name == d.Name })
		if i < 0 {
			diffs = append(diffs, domain.ServiceDiff{Service: d.Name, Change: "added"})
			continue
		}
		fields := changedFields(children[i], d)
		if children[i].NotInGit {
			fields = append(fields, "notInGit")
		}
		if len(fields) > 0 {
			diffs = append(diffs, domain.ServiceDiff{Service: d.Name, Change: "changed", Fields: fields})
		}
	}
	for _, ch := range children {
		if !declared[ch.Name] {
			diffs = append(diffs, domain.ServiceDiff{Service: ch.Name, Change: "removed"})
		}
	}
	return diffs, nil
}

// changedFields names the definition settings that differ between two normalized Applications.
func changedFields(old, a domain.Application) []string {
	var f []string
	for _, c := range []struct {
		name    string
		changed bool
	}{
		{"repoURL", old.RepoURL != a.RepoURL},
		{"path", old.Path != a.Path},
		{"targetRevision", old.TargetRevision != a.TargetRevision},
		{"composeFiles", !slices.Equal(old.ComposeFiles, a.ComposeFiles)},
		{"profiles", !slices.Equal(old.Profiles, a.Profiles)},
		{"host", old.Host != a.Host},
		{"autoSync", old.AutoSync != a.AutoSync},
		{"selfHeal", old.SelfHeal != a.SelfHeal},
		{"prune", old.Prune != a.Prune},
		{"pruneVolumes", old.PruneVolumes != a.PruneVolumes},
	} {
		if c.changed {
			f = append(f, c.name)
		}
	}
	return f
}

// ---- Hosts ----

func (c *Controller) ListHosts(ctx context.Context) ([]domain.HostView, error) {
	hosts, err := c.cfg.Store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	byHost, err := c.appsByHost(ctx)
	if err != nil {
		return nil, err
	}
	if !c.cfg.NoLocalHost {
		hosts = append([]domain.Host{{Name: domain.LocalHost, WorkDir: c.cfg.WorkDir}}, hosts...)
	}
	views := make([]domain.HostView, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Go(func() { views[i] = c.hostView(ctx, h, byHost[h.Name]) })
	}
	wg.Wait()
	return views, nil
}

func (c *Controller) hostView(ctx context.Context, h domain.Host, apps []string) domain.HostView {
	if apps == nil {
		apps = []string{}
	}
	v := domain.HostView{Host: h, Status: c.cfg.Engine.HostStatus(ctx, h.Name), Apps: apps}
	if keys, err := c.hostKeys(ctx, h.Name); err != nil {
		c.cfg.Log.Warn("list host keys", "host", h.Name, "err", err)
	} else if len(keys) > 0 && keys[0].Current {
		v.KeyRecipient = keys[0].Recipient
	}
	return v
}

func (c *Controller) appsByHost(ctx context.Context) (map[string][]string, error) {
	apps, err := c.cfg.Store.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	m := map[string][]string{}
	for _, a := range apps {
		m[a.Host] = append(m[a.Host], a.Name)
	}
	return m, nil
}

func (c *Controller) SaveHost(ctx context.Context, h domain.Host, create bool) (domain.HostView, error) {
	h.Name, h.URL, h.WorkDir = strings.TrimSpace(h.Name), strings.TrimSpace(h.URL), strings.TrimSpace(h.WorkDir)
	if !nameRE.MatchString(h.Name) {
		return domain.HostView{}, fmt.Errorf("%w: name must be lowercase letters, digits, '-' or '_'", domain.ErrInvalid)
	}
	if h.Name == domain.LocalHost {
		return domain.HostView{}, fmt.Errorf("%w: %q is the built-in Host", domain.ErrInvalid, domain.LocalHost)
	}
	if !strings.HasPrefix(h.URL, "ssh://") && !strings.HasPrefix(h.URL, "tcp://") {
		return domain.HostView{}, fmt.Errorf("%w: URL must start with ssh:// or tcp://", domain.ErrInvalid)
	}
	if h.WorkDir != "" && !strings.HasPrefix(h.WorkDir, "/") {
		return domain.HostView{}, fmt.Errorf("%w: work dir must be an absolute path", domain.ErrInvalid)
	}
	old, err := c.cfg.Store.GetHost(ctx, h.Name)
	switch {
	case err == nil && create:
		return domain.HostView{}, fmt.Errorf("host %q: %w", h.Name, domain.ErrAlreadyExists)
	case errors.Is(err, domain.ErrNotFound) && !create:
		return domain.HostView{}, fmt.Errorf("host %q: %w", h.Name, domain.ErrNotFound)
	case err != nil && !errors.Is(err, domain.ErrNotFound):
		return domain.HostView{}, err
	}
	h.CreatedAt = old.CreatedAt
	if err := c.cfg.Store.SaveHost(ctx, h); err != nil {
		return domain.HostView{}, err
	}
	c.cfg.Engine.HostChanged(h.Name)
	if create {
		if _, err := c.GenerateHostKey(ctx, h.Name); err != nil {
			return domain.HostView{}, err
		}
	}
	if h, err = c.cfg.Store.GetHost(ctx, h.Name); err != nil {
		return domain.HostView{}, err
	}
	byHost, err := c.appsByHost(ctx)
	if err != nil {
		return domain.HostView{}, err
	}
	return c.hostView(ctx, h, byHost[h.Name]), nil
}

func (c *Controller) DeleteHost(ctx context.Context, name string) error {
	if name == domain.LocalHost {
		return fmt.Errorf("%w: the built-in Host cannot be deleted", domain.ErrInvalid)
	}
	byHost, err := c.appsByHost(ctx)
	if err != nil {
		return err
	}
	if apps := byHost[name]; len(apps) > 0 {
		return fmt.Errorf("%w: host %q is used by Applications %s", domain.ErrInvalid, name, strings.Join(apps, ", "))
	}
	if err := c.cfg.Store.DeleteHost(ctx, name); err != nil {
		return err
	}
	c.cfg.Engine.HostChanged(name)
	return nil
}

func (c *Controller) TestHost(ctx context.Context, name string) (domain.HostStatus, error) {
	if name != domain.LocalHost {
		if _, err := c.cfg.Store.GetHost(ctx, name); err != nil {
			return domain.HostStatus{}, err
		}
	}
	return c.cfg.Engine.HostStatus(ctx, name), nil
}

func (c *Controller) CheckRepo(ctx context.Context, repoURL string) (domain.RepoAccess, error) {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return domain.RepoAccess{}, fmt.Errorf("%w: repoURL is required", domain.ErrInvalid)
	}
	return c.cfg.Git.CheckAccess(ctx, repoURL)
}

// ---- Host Keys ----

// hostKeys returns a Host's keys with their private keys, current first: the Master Key for LocalHost.
func (c *Controller) hostKeys(ctx context.Context, host string) ([]domain.HostKey, error) {
	if host == "" || host == domain.LocalHost {
		return []domain.HostKey{{Host: domain.LocalHost, Recipient: c.cfg.MasterKey.Recipient().String(),
			PrivateKey: c.cfg.MasterKey.String(), Current: true, Master: true}}, nil
	}
	return c.cfg.Store.ListHostKeys(ctx, host)
}

// syncKeys returns the keys a Sync of a decrypts with; nothing deploys to a remote Host without a Host Key.
func (c *Controller) syncKeys(ctx context.Context, a domain.Application) ([]domain.HostKey, error) {
	keys, err := c.hostKeys(ctx, a.Host)
	if err == nil && len(keys) == 0 {
		err = fmt.Errorf("Host %q has no Host Key — generate or import one in Settings → Hosts", a.Host)
	}
	return keys, err
}

// decrypt decrypts the Encrypted Secrets in dir with keys (from syncKeys), explaining a key mismatch.
func (c *Controller) decrypt(a domain.Application, dir string, keys []domain.HostKey) error {
	ids, recipients := make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		ids[i], recipients[i] = k.PrivateKey, k.Recipient
	}
	written, err := c.cfg.Secrets.DecryptDir(dir, ids)
	if dir == c.appDir(a.Name) {
		// recorded even after an error: whatever was decrypted is in the app dir now and must stay masked
		if werr := c.writeDecrypted(a.Name, dir, written); werr != nil {
			return fmt.Errorf("record decrypted files: %w", werr)
		}
	}
	var wk *domain.WrongKeyError
	switch {
	case err == nil:
		return nil
	case !errors.As(err, &wk):
		return fmt.Errorf("decrypt secrets: %w", err)
	case keys[0].Master:
		return fmt.Errorf("%w; Host %q uses the Master Key %s — re-encrypt it for this Host (sops updatekeys)", wk, domain.LocalHost, recipients[0])
	case len(keys) == 1:
		return fmt.Errorf("%w; Host %q has key %s — re-encrypt it for this Host (sops updatekeys)", wk, a.Host, recipients[0])
	default:
		return fmt.Errorf("%w; Host %q has keys %s — re-encrypt it for this Host (sops updatekeys)", wk, a.Host, strings.Join(recipients, ", "))
	}
}

// remoteHost checks that host is a stored Host: LocalHost's key is the Master Key, which is not managed here.
func (c *Controller) remoteHost(ctx context.Context, host string) error {
	if host == domain.LocalHost {
		return fmt.Errorf("%w: the local Host uses the Master Key (OMEGACD_AGE_KEY_FILE)", domain.ErrInvalid)
	}
	_, err := c.cfg.Store.GetHost(ctx, host)
	return err
}

func (c *Controller) HostKeys(ctx context.Context, host string) ([]domain.HostKey, error) {
	if host != domain.LocalHost {
		if _, err := c.cfg.Store.GetHost(ctx, host); err != nil {
			return nil, err
		}
	}
	keys, err := c.hostKeys(ctx, host)
	for i := range keys {
		keys[i].PrivateKey = ""
	}
	return keys, err
}

func (c *Controller) GenerateHostKey(ctx context.Context, host string) (domain.HostKey, error) {
	if err := c.remoteHost(ctx, host); err != nil {
		return domain.HostKey{}, err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return domain.HostKey{}, err
	}
	return c.addHostKey(ctx, host, id)
}

func (c *Controller) ImportHostKey(ctx context.Context, host, privateKey string) (domain.HostKey, error) {
	if err := c.remoteHost(ctx, host); err != nil {
		return domain.HostKey{}, err
	}
	// accepts a whole age-keygen file (with its "# public key" comments); the parse error is dropped so no key material is echoed
	ids, err := age.ParseIdentities(strings.NewReader(privateKey))
	var id *age.X25519Identity
	if err == nil && len(ids) == 1 {
		id, _ = ids[0].(*age.X25519Identity)
	}
	if id == nil {
		return domain.HostKey{}, fmt.Errorf("%w: expected one age private key (AGE-SECRET-KEY-1...)", domain.ErrInvalid)
	}
	return c.addHostKey(ctx, host, id)
}

func (c *Controller) addHostKey(ctx context.Context, host string, id *age.X25519Identity) (domain.HostKey, error) {
	k := domain.HostKey{Host: host, Recipient: id.Recipient().String(), PrivateKey: id.String(), Current: true, CreatedAt: time.Now().UTC()}
	if err := c.cfg.Store.AddHostKey(ctx, k); err != nil {
		return domain.HostKey{}, err
	}
	go c.reconcileHost(context.Background(), host) // clears "no Host Key" and lets Auto-Sync proceed
	k.PrivateKey = ""
	return k, nil
}

func (c *Controller) RemoveHostKey(ctx context.Context, host, recipient string) error {
	if err := c.remoteHost(ctx, host); err != nil {
		return err
	}
	keys, err := c.cfg.Store.ListHostKeys(ctx, host)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(keys, func(k domain.HostKey) bool { return k.Recipient == recipient })
	switch {
	case i < 0:
		return fmt.Errorf("host key %s: %w", recipient, domain.ErrNotFound)
	case keys[i].Current:
		return fmt.Errorf("%w: the current Host Key can't be removed — generate or import a new one first", domain.ErrInvalid)
	}
	return c.cfg.Store.DeleteHostKey(ctx, host, recipient)
}
