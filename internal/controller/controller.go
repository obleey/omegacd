// Package controller reconciles Applications: it computes Sync and Health Status,
// runs Sync Operations, and applies Auto-Sync, Self-Heal and retry policy.
package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

const (
	maxRetries   = 5
	retryBase    = 5 * time.Second
	retryMax     = 3 * time.Minute
	retryTick    = 5 * time.Second
	syncTimeout  = 30 * time.Minute
	subscriberCh = 64
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

type Config struct {
	Store  domain.Store
	Source domain.Source
	// Blobs keeps the files of every payload delivered, so what was rendered deploys without the Source Server (Q121)
	Blobs        *blob.Store
	PollInterval time.Duration
	NormalizeURL func(string) string
	ObserveSync  func(app string, phase domain.OpPhase, d time.Duration) // may be nil
	// Agents deploys to every Host: each runs an Agent (Q9, Q107)
	Agents Agents
	Log    *slog.Logger
}

// Agents sends Desired States to Hosts' Agents: Deploy returns once the Agent deployed (nil) or failed (why), or with
// agents.ErrOffline / ErrNotApproved when there's no Agent to send to, ErrDisconnected when the connection dropped.
type Agents interface {
	Deploy(ctx context.Context, host string, ds *agentv1.DesiredState) error
	// Remove has the Agent remove app, or with keepContainers forget it (Q83); errors like Deploy's
	Remove(ctx context.Context, host, app string, volumes, keepContainers bool) error
	// Logs writes an Application's logs from its Agent to w (M4-02)
	Logs(ctx context.Context, host, app, container string, follow bool, tail int, w io.Writer) error
	// Online reports whether the Host's Agent is connected now
	Online(host string) bool
	// Diff has the Agent tell how a Revision not deployed would change what runs (M4-11); errors like Deploy's
	Diff(ctx context.Context, host string, ds *agentv1.DesiredState) ([]domain.ServiceDiff, error)
}

type appState struct {
	view       domain.AppView
	syncing    bool
	deleting   bool // set before a delete touches containers, so nothing re-syncs the app meanwhile
	targetTree string
	drift      bool
	agentDrift bool // drifted on an Agent's Host: its Agent heals it (Self-Heal) or it stays; never re-synced from here (Q82)
	retries    int
	nextRetry  time.Time
	failedTree string // tree that exhausted its retries; auto-sync waits for a new one
	// an App of Apps' pendingChildren message, computed once per target tree
	pendingTree, pending string
}

type Controller struct {
	cfg     Config
	mu      sync.Mutex
	claimMu sync.Mutex // children syncing at once claim their App of Apps' folder together
	apps    map[string]*appState
	subs    map[chan domain.Event]struct{}
}

func New(cfg Config) *Controller {
	cfg.Log = cmp.Or(cfg.Log, logging.Discard)
	if cfg.NormalizeURL == nil {
		cfg.NormalizeURL = strings.ToLower
	}
	return &Controller{cfg: cfg, apps: map[string]*appState{}, subs: map[chan domain.Event]struct{}{}}
}

// Run reconciles every Application each PollInterval (never when 0: webhooks, Refresh and restarts only)
// and drives retries until ctx ends.
func (c *Controller) Run(ctx context.Context) {
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

// reconcile recomputes the status of one Application, publishes it, and triggers automatic Syncs.
func (c *Controller) reconcile(ctx context.Context, a domain.Application) domain.AppView {
	return c.reconcileWith(ctx, a, true)
}

// reconcileWith skips the Git fetch when fetch is false and a target is already known,
// so live container changes can be reflected immediately and cheaply.
func (c *Controller) reconcileWith(ctx context.Context, a domain.Application, fetch bool) domain.AppView {
	ctx = logging.With(ctx, "app", a.Name)
	v := domain.AppView{Application: a, SyncStatus: domain.Unknown, Health: domain.Missing, ReconciledAt: time.Now()}
	viaAgent := a.Kind == domain.KindCompose
	var targetTree string
	var drift, agentDrift bool

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
			c.cfg.Log.WarnContext(ctx, "list child applications", "err", err)
		}
		c.mu.Lock()
		v.Health = c.worstHealth(children)
		c.mu.Unlock()
		v.Host, v.Hosts = "", childHosts(children) // stored as "local" before it had no Host
	}

	sha, targetTree, err := c.target(ctx, a, fetch)
	if err == nil {
		v.TargetSHA = sha
	}
	switch {
	case err != nil:
		v.SyncMessage = err.Error()
		if errors.Is(err, domain.ErrUnavailable) && deployed { // Q124
			v.SyncMessage += "; the deployed Revision " + short(last.Revision) + " keeps running"
		}
	case !deployed:
		v.SyncStatus, v.SyncMessage = domain.OutOfSync, "never synced"
	case targetTree != last.TreeHash:
		v.SyncStatus, v.SyncMessage = domain.OutOfSync, "new revision "+short(sha)
		if a.Kind == domain.KindApps {
			// the commit may not touch the definitions at all: say what a Sync would change, if anything
			if pending := c.pendingChildren(ctx, a, sha, targetTree); pending != "" {
				v.SyncMessage = pending
			}
		}
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
	case viaAgent:
		// the deployed tree is the target's; what runs may still have drifted, as its Agent reported (M4-02)
		v.SyncStatus = domain.Synced
		if st, err := c.cfg.Store.AgentStatus(ctx, a.Name); err == nil && st.Drifted && st.Revision == last.Revision {
			v.SyncStatus, v.SyncMessage = domain.OutOfSync, "live state drifted: "+describe(st.Drift)
			agentDrift = true
		}
	}
	if viaAgent {
		v.Health = domain.HealthUnknown // until its Agent first reports
		if st, err := c.cfg.Store.AgentStatus(ctx, a.Name); err == nil {
			v.Health = st.Health
		}
		if !c.cfg.Agents.Online(a.Host) {
			v.SyncMessage = strings.TrimPrefix(v.SyncMessage+"; "+c.offline(ctx, a), "; ")
		}
	}

	c.mu.Lock()
	s := c.state(a.Name)
	s.targetTree, s.drift, s.agentDrift = targetTree, drift, agentDrift
	if v.SyncStatus == domain.Synced {
		s.retries, s.nextRetry = 0, time.Time{}
	}
	if s.syncing {
		// keep showing the running op instead of a half-applied status
		v.LastOperation = s.view.LastOperation
	}
	s.view = v
	trigger, why := c.autoTrigger(a, s, v)
	c.mu.Unlock()
	c.cfg.Log.DebugContext(ctx, "reconciled", "status", v.SyncStatus, "message", v.SyncMessage, "health", v.Health,
		"target", short(sha), "deployed", short(v.DeployedSHA), "drift", drift, "autoSync", cmp.Or(trigger, "no"), "why", why)
	c.emit(domain.Event{Type: "app", App: a.Name, Data: v})
	if a.Parent != "" {
		c.refreshParent(ctx, a.Parent)
	}

	if trigger != "" {
		if _, err := c.startSync(a, sha, targetTree, trigger); err != nil && !errors.Is(err, domain.ErrBusy) {
			c.cfg.Log.ErrorContext(ctx, "auto sync", "err", err)
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
	if sha, err = c.cfg.Source.Resolve(ctx, a.RepoURL, a.TargetRevision); err != nil {
		return "", "", err
	}
	r, err := c.render(ctx, a, sha)
	return sha, r.Checksum, err
}

// render is a's Desired State at sha.
func (c *Controller) render(ctx context.Context, a domain.Application, sha string) (domain.Rendered, error) {
	return c.cfg.Source.Render(ctx, domain.RenderSpec{RepoURL: a.RepoURL, Revision: sha, Path: a.Path,
		ComposeFiles: a.ComposeFiles, Profiles: a.Profiles, Apps: a.Kind == domain.KindApps})
}

// renderAt is a's Desired State at sha: the render kept for tree when there is one (Q120, Q122), else the Source
// Server's, kept for the next time. An App of Apps always renders: its children's definitions come from Git.
func (c *Controller) renderAt(ctx context.Context, a domain.Application, sha, tree string) (domain.Rendered, error) {
	if tree != "" && a.Kind == domain.KindCompose {
		files, err := c.cfg.Store.Render(ctx, tree)
		if err == nil {
			c.cfg.Log.DebugContext(ctx, "the kept render", "app", a.Name, "tree", tree, "files", len(files))
			return domain.Rendered{Files: files, Checksum: tree}, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Rendered{}, err
		}
	}
	r, err := c.render(ctx, a, sha)
	if err == nil && r.Invalid == nil && a.Kind == domain.KindCompose {
		if err := c.cfg.Store.SaveRender(ctx, r.Checksum, r.Files); err != nil { // the Source Server renders it again
			c.cfg.Log.WarnContext(ctx, "keep the render", "app", a.Name, "tree", r.Checksum, "err", err)
		}
	}
	return r, err
}

// Blob is a delivered file's content: from the Control Plane's own store (Q121), else the Source Server's.
func (c *Controller) Blob(ctx context.Context, hash string) ([]byte, error) {
	if b, err := c.cfg.Blobs.Get(hash); err == nil {
		return b, nil
	}
	return c.cfg.Source.Blob(ctx, hash)
}

// autoTrigger is the trigger of the Sync policy wants now ("" for none), and why (for the debug log). Caller holds c.mu.
func (c *Controller) autoTrigger(a domain.Application, s *appState, v domain.AppView) (trigger, why string) {
	switch {
	case v.SyncStatus != domain.OutOfSync:
		return "", "not OutOfSync"
	case !a.AutoSync:
		return "", "Auto-Sync off"
	case a.RolledBack:
		return "", "rolled back"
	case s.syncing || s.deleting:
		return "", "busy"
	case a.Deleting != "":
		return "", "deleting, waiting for its Host"
	case s.agentDrift:
		return "", "drifted on its Host: its Agent heals what it should (Self-Heal)"
	case v.LastOperation != nil && v.LastOperation.TreeHash == s.targetTree &&
		(v.LastOperation.Phase == domain.OpWaiting || v.LastOperation.Phase == domain.OpRunning):
		return "", "a Sync to this tree waits for its Host" // or for its disconnected Agent's report (Q81, Q93)
	case s.targetTree == s.failedTree:
		return "", "this tree used up its retries"
	case s.retries > 0 && time.Now().Before(s.nextRetry):
		return "", "retry " + strconv.Itoa(s.retries) + " waits until " + s.nextRetry.Format(time.TimeOnly)
	case s.drift:
		// an App of Apps only drifts by children kept without Prune; syncing again would not change that
		if a.SelfHeal && a.Kind != domain.KindApps {
			return "self-heal", "drifted"
		}
		return "", "drifted, Self-Heal off"
	}
	return "auto", "new revision"
}

// startSync records a Sync Operation to sha and runs it in the background; tree, when known, finds its kept render.
func (c *Controller) startSync(a domain.Application, sha, tree, trigger string) (int64, error) {
	ctx := context.Background()
	r, err := c.renderAt(ctx, a, sha, tree)
	if err != nil {
		return 0, err
	}
	tree = r.Checksum
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
	if err := c.cfg.Store.SupersedeWaitingOps(ctx, a.Name, id); err != nil {
		c.cfg.Log.Error("supersede waiting syncs", "app", a.Name, "err", err)
	}
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
	ctx = logging.With(ctx, "app", a.Name, "op", op.ID, "revision", short(op.Revision), "host", a.Host)
	c.cfg.Log.InfoContext(ctx, "sync started", "trigger", op.Trigger)
	err := c.apply(ctx, a, op)
	if waiting := errors.Is(err, agents.ErrOffline) || errors.Is(err, agents.ErrNotApproved); waiting || errors.Is(err, agents.ErrDisconnected) {
		c.unfinished(ctx, a, op, waiting, err)
		return
	}

	phase, msg := domain.OpSucceeded, "synced to "+short(op.Revision)
	if op.Trigger == triggerRestore {
		msg = "deployed " + short(op.Revision) + " again: its Agent didn't run it (reinstalled?)"
	}
	if errors.Is(err, errTakenOver) {
		msg, err = err.Error(), nil
	}
	if err != nil {
		phase, msg = domain.OpFailed, err.Error()
	}
	if ferr := c.cfg.Store.FinishOp(ctx, op.ID, phase, msg); ferr != nil {
		c.cfg.Log.ErrorContext(ctx, "record sync operation", "err", ferr)
	}
	if c.cfg.ObserveSync != nil {
		c.cfg.ObserveSync(a.Name, phase, time.Since(op.StartedAt))
	}
	c.cfg.Log.InfoContext(ctx, "sync finished", "trigger", op.Trigger, "phase", phase, "msg", msg)

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
				c.cfg.Log.ErrorContext(ctx, "clear rolled-back flag", "err", uerr)
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

// unfinished keeps a Sync its Agent didn't answer: Waiting for an offline Host (Q81), or Running while a disconnected
// Agent finishes it (Q93). Its outcome arrives when the Agent connects again (M3-06).
func (c *Controller) unfinished(ctx context.Context, a domain.Application, op domain.SyncOperation, waiting bool, why error) {
	phase, msg := domain.OpRunning, "Host "+a.Host+" disconnected; its Agent reports the result when it is back"
	if waiting {
		phase, msg = domain.OpWaiting, "waiting for Host "+a.Host+": "+why.Error()
	}
	if err := c.cfg.Store.SetOpPhase(ctx, op.ID, phase, msg); err != nil {
		c.cfg.Log.ErrorContext(ctx, "record sync operation", "err", err)
	}
	c.cfg.Log.WarnContext(ctx, "sync not finished", "phase", phase, "why", why)
	c.setSyncing(a.Name, false)
	if cur, err := c.cfg.Store.GetOp(ctx, op.ID); err == nil {
		c.emit(domain.Event{Type: "op", App: a.Name, Data: cur})
	}
	if fresh, err := c.cfg.Store.GetApp(ctx, a.Name); err == nil {
		c.reconcile(ctx, fresh)
	}
}

const triggerParent = "app-of-apps"

// syncPendingChildren is what a manual Sync of an App of Apps means to a person: deploy what isn't
// deployed. Children that are OutOfSync, Unknown (say, their files are missing) or whose last Sync failed get a
// Sync with a fresh retry budget (e.g. after they gave up waiting for a dependency); Synced ones are left alone.
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
		if (v.SyncStatus == domain.Synced && !failed) || v.TargetSHA == "" {
			continue
		}
		c.mu.Lock()
		s := c.state(ch.Name)
		s.retries, s.nextRetry, s.failedTree = 0, time.Time{}, ""
		c.mu.Unlock()
		if _, err := c.startSync(ch, v.TargetSHA, "", triggerParent); err != nil && !errors.Is(err, domain.ErrBusy) {
			c.cfg.Log.Error("sync child application", "app", ch.Name, "parent", parent, "err", err)
		}
	}
}

// apply renders the Revision and sends it to the Application's Agent, or, for an App of Apps, syncs its children.
func (c *Controller) apply(ctx context.Context, a domain.Application, op domain.SyncOperation) error {
	r, err := c.renderAt(ctx, a, op.Revision, op.TreeHash)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	c.cfg.Log.DebugContext(ctx, "rendered", "tree", r.Checksum, "warnings", len(r.Warnings), "invalid", r.Invalid != nil)
	if r.Invalid != nil {
		return r.Invalid
	}
	if a.Kind == domain.KindApps {
		return c.syncChildren(ctx, a, r.Apps) // an App of Apps has no files: its definitions are only read
	}
	if c.takenOver(ctx, a, r) {
		return errTakenOver
	}
	return c.deploy(ctx, a, op, r)
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
	// its Agent checks for a hand-started project of the same name at the first Sync (Adopt in the Desired State)
	if a.Kind == domain.KindCompose {
		if err := c.checkHost(ctx, a.Host); err != nil {
			return err
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

// checkUpdate carries over what an update may not change. Adopt may: an Agent checks it only before its first deploy
// of the Application, so it is set where its first Sync was refused (Q102).
func checkUpdate(old domain.Application, a *domain.Application) error {
	if a.Kind == domain.KindCompose && a.Host != old.Host {
		return fmt.Errorf("%w: delete and recreate the Application %q to move it to another Host", domain.ErrInvalid, a.Name)
	}
	a.CreatedAt = old.CreatedAt
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
		var waiting []string
		for _, ch := range children {
			if cascade {
				err = c.deleteApp(ctx, ch, true, volumes)
				if errors.Is(err, domain.ErrWaitingForHost) { // the App of Apps goes once its last child is gone
					waiting, err = append(waiting, ch.Name), nil
				}
			} else {
				// its Agent moves its files out of the App of Apps' folder at its next Sync
				ch.Parent, ch.NotInGit = "", false
				if err = c.cfg.Store.UpdateApp(ctx, ch); err == nil {
					c.reconcileWith(ctx, ch, false)
				}
			}
			if err != nil {
				return fmt.Errorf("application %q: %w", ch.Name, err)
			}
		}
		if len(waiting) > 0 {
			if err := c.cfg.Store.SetAppDeleting(ctx, a.Name, deleteHow(cascade, volumes)); err != nil { // coverage: unreachable, the App of Apps was just read; only a delete racing this one
				return err
			}
			return fmt.Errorf("%w of %s", domain.ErrWaitingForHost, strings.Join(waiting, ", "))
		}
	}
	if a.Kind == domain.KindCompose {
		err := c.cfg.Agents.Remove(ctx, a.Host, a.Name, cascade && volumes, !cascade)
		if errors.Is(err, agents.ErrOffline) || errors.Is(err, agents.ErrNotApproved) || errors.Is(err, agents.ErrDisconnected) {
			if serr := c.cfg.Store.SetAppDeleting(ctx, a.Name, deleteHow(cascade, volumes)); serr != nil { // coverage: unreachable, the app was just read; only a delete racing this one
				return serr
			}
			c.cfg.Log.WarnContext(ctx, "delete waits for the host", "app", a.Name, "host", a.Host, "why", err)
			if fresh, gerr := c.cfg.Store.GetApp(ctx, a.Name); gerr == nil {
				c.emit(domain.Event{Type: "app", App: a.Name, Data: c.cachedView(ctx, fresh)})
			}
			return fmt.Errorf("%w %s: its Agent removes %s when it connects", domain.ErrWaitingForHost, a.Host, a.Name)
		}
		if err != nil {
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
	if a.Parent != "" {
		c.finishDeleting(ctx, a.Parent)
	}
	return nil
}

// deleteHow is how a delete is recorded while it waits for a Host (Q83).
func deleteHow(cascade, volumes bool) string {
	switch {
	case !cascade:
		return "keep"
	case volumes:
		return "volumes"
	}
	return "containers"
}

// finishDeleting deletes an Application recorded as being deleted, again: once its Host is back, or for an App of
// Apps once its children are gone.
func (c *Controller) finishDeleting(ctx context.Context, name string) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil || a.Deleting == "" {
		return
	}
	err = c.deleteApp(ctx, a, a.Deleting != "keep", a.Deleting == "volumes")
	if err != nil && !errors.Is(err, domain.ErrWaitingForHost) {
		c.cfg.Log.ErrorContext(ctx, "finish deleting", "app", name, "err", err)
	}
}

func (c *Controller) Sync(ctx context.Context, name string, req domain.SyncRequest) (int64, error) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return 0, err
	}
	if req.Prune != nil {
		a.Prune = *req.Prune // this Sync only; not saved
	}
	c.cfg.Source.Invalidate(a.RepoURL)
	sha, err := c.cfg.Source.Resolve(ctx, a.RepoURL, a.TargetRevision)
	if err != nil {
		return 0, err
	}
	return c.startSync(a, sha, "", "manual")
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
	return c.startSync(a, op.Revision, op.TreeHash, "rollback")
}

// Refresh re-reads Git now, past the fetch cooldown. An App of Apps refreshes its children first, so its own
// status is built from theirs; each Repository is fetched once however many children share it.
func (c *Controller) Refresh(ctx context.Context, name string) (domain.AppView, error) {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return domain.AppView{}, err
	}
	c.cfg.Source.Invalidate(a.RepoURL)
	if a.Kind == domain.KindApps {
		children, err := c.children(ctx, a.Name)
		if err != nil {
			return domain.AppView{}, err
		}
		for _, ch := range children {
			if ch.RepoURL != a.RepoURL {
				c.cfg.Source.Invalidate(ch.RepoURL)
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
	c.cfg.Source.Invalidate(a.RepoURL)
	to, err := c.cfg.Source.Resolve(ctx, a.RepoURL, a.TargetRevision)
	if err != nil {
		return domain.DiffView{}, err
	}
	dv := domain.DiffView{ToSHA: to}
	if last, err := c.cfg.Store.LastSucceededOp(ctx, name); err == nil {
		dv.FromSHA = last.Revision
	}
	if dv.GitDiff, err = c.cfg.Source.Diff(ctx, a.RepoURL, dv.FromSHA, to, a.Path); err != nil {
		return domain.DiffView{}, err
	}
	r, err := c.render(ctx, a, to)
	if err != nil {
		return domain.DiffView{}, err
	}
	if r.Invalid != nil {
		return domain.DiffView{}, r.Invalid
	}
	if a.Kind == domain.KindApps {
		dv.Services, err = c.childDiff(ctx, a.Name, r.Apps)
		return dv, err
	}
	dv.Warnings = r.Warnings // the Source Server's (plain-text secrets), then the Agent's
	return c.agentDiff(ctx, a, dv, r)
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
	return c.agentLive(ctx, a), nil
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
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return domain.ResourceDetail{}, err
	}
	return c.agentDetail(ctx, a, d)
}

func (c *Controller) Logs(ctx context.Context, name, containerID string, follow bool, tail int, w io.Writer) error {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return err
	}
	return c.cfg.Agents.Logs(ctx, a.Host, a.Name, containerID, follow, tail, w)
}

func (c *Controller) RepoChanged(ctx context.Context, repoURL string) {
	want := c.cfg.NormalizeURL(repoURL)
	c.cfg.Source.Invalidate(repoURL)
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
	a.Host = strings.TrimSpace(a.Host)
	// Prune volumes is a sub-option of Prune, and an App of Apps has no volumes of its own
	a.PruneVolumes = a.PruneVolumes && a.Prune && a.Kind == domain.KindCompose
	if a.Kind == domain.KindApps {
		// an App of Apps deploys nothing itself: no Host; its Child Applications each name their own
		a.ComposeFiles, a.Profiles, a.Host = nil, nil, ""
	} else if a.Host == "" {
		return fmt.Errorf("%w: host is required: name the Host whose Agent deploys it", domain.ErrInvalid)
	}
	for _, f := range a.ComposeFiles {
		if filepath.IsAbs(f) || strings.Contains(f, "..") {
			return fmt.Errorf("%w: compose file %q must be relative to the path", domain.ErrInvalid, f)
		}
	}
	// its own work dir (Q57): absolute; the Agent decides whether it may write there. An App of Apps has no folder
	if a.WorkDir = strings.TrimSpace(a.WorkDir); a.Kind == domain.KindApps {
		a.WorkDir = ""
	} else if a.WorkDir != "" {
		if !filepath.IsAbs(a.WorkDir) || strings.Contains(a.WorkDir, "..") || filepath.Clean(a.WorkDir) == "/" {
			return fmt.Errorf("%w: workDir %q must be an absolute path, not /", domain.ErrInvalid, a.WorkDir)
		}
		a.WorkDir = filepath.Clean(a.WorkDir)
	}
	return nil
}

func (c *Controller) checkHost(ctx context.Context, name string) error {
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

// childHosts is the sorted, distinct Hosts of an App of Apps' children.
func childHosts(children []domain.Application) []string {
	var hosts []string
	for _, ch := range children {
		hosts = append(hosts, ch.Host)
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

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

// syncChildren makes parent's Child Applications match defs.
// Every definition is validated before anything is changed.
func (c *Controller) syncChildren(ctx context.Context, parent domain.Application, defs []domain.Application) error {
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

// childDiff compares defs with parent's stored Child Applications.
func (c *Controller) childDiff(ctx context.Context, parent string, defs []domain.Application) ([]domain.ServiceDiff, error) {
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
			diffs = append(diffs, domain.ServiceDiff{Service: d.Name, Change: "added", Host: d.Host})
			continue
		}
		fields := changedFields(children[i], d)
		if children[i].NotInGit {
			fields = append(fields, "notInGit")
		}
		if len(fields) > 0 {
			diffs = append(diffs, domain.ServiceDiff{Service: d.Name, Change: "changed", Fields: fields, Host: d.Host})
		}
	}
	for _, ch := range children {
		if !declared[ch.Name] {
			diffs = append(diffs, domain.ServiceDiff{Service: ch.Name, Change: "removed", Host: ch.Host})
		}
	}
	return diffs, nil
}

// pendingChildren says what a Sync of the App of Apps a would do to its children at sha, e.g. "1 Child Application
// to create (alloy), 2 to update (web, api)", or "" for nothing (or definitions it can't read: the Sync says why).
// Reconcile runs often, so it is computed once per tree, from the commit target() already fetched.
func (c *Controller) pendingChildren(ctx context.Context, a domain.Application, sha, tree string) string {
	c.mu.Lock()
	s := c.state(a.Name)
	if s.pendingTree == tree {
		defer c.mu.Unlock()
		return s.pending
	}
	c.mu.Unlock()
	var msg string
	if r, err := c.render(ctx, a, sha); err == nil && r.Invalid == nil {
		diffs, _ := c.childDiff(ctx, a.Name, r.Apps)
		msg = describeChildren(diffs)
	}
	c.mu.Lock()
	s.pendingTree, s.pending = tree, msg
	c.mu.Unlock()
	return msg
}

// describeChildren sums up a childDiff: "1 Child Application to create (alloy), 2 to update (web, api), 1 not in Git (old)".
func describeChildren(diffs []domain.ServiceDiff) string {
	var parts []string
	for _, k := range [][2]string{{"added", "to create"}, {"changed", "to update"}, {"removed", "not in Git"}} {
		var names []string
		for _, d := range diffs {
			if d.Change == k[0] {
				names = append(names, d.Service)
			}
		}
		if len(names) == 0 {
			continue
		}
		n := fmt.Sprint(len(names))
		switch {
		case len(parts) > 0:
		case len(names) == 1:
			n += " Child Application"
		default:
			n += " Child Applications"
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", n, k[1], strings.Join(names, ", ")))
	}
	return strings.Join(parts, ", ")
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
		{"workDir", old.WorkDir != a.WorkDir},
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
	v := domain.HostView{Host: h, Status: domain.HostStatus{Name: h.Name}, Apps: apps}
	if h.Agent != nil { // as its Agent reported (M4-02); nothing before an Agent enrolled
		v.Status.Connected = c.cfg.Agents.Online(h.Name)
		v.Status.ServerVersion, v.Status.OS = h.Agent.EngineVersion, h.Agent.OS
		if !v.Status.Connected && !h.Agent.LeftAt.IsZero() {
			v.Status.Error = "offline since " + h.Agent.LeftAt.Local().Format(time.DateTime)
		}
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
		if a.Kind == domain.KindCompose { // an App of Apps runs on no Host
			m[a.Host] = append(m[a.Host], a.Name)
		}
	}
	return m, nil
}

func (c *Controller) InviteHost(ctx context.Context, name string) (string, time.Time, error) {
	name = strings.TrimSpace(name)
	if !nameRE.MatchString(name) {
		return "", time.Time{}, fmt.Errorf("%w: name must be lowercase letters, digits, '-' or '_'", domain.ErrInvalid)
	}
	expires := time.Now().Add(inviteValidity).Truncate(time.Second)
	invite, err := c.cfg.Store.CreateInvite(ctx, name, expires)
	if err == nil {
		c.cfg.Log.Info("host invited", "host", name, "expires", expires)
	}
	return invite, expires, err
}

// inviteValidity is how long an invite lasts (Q73): long enough to install an Agent, and a new one is a click away.
const inviteValidity = time.Hour

func (c *Controller) DeleteHost(ctx context.Context, name string) error {
	byHost, err := c.appsByHost(ctx)
	if err != nil {
		return err
	}
	if apps := byHost[name]; len(apps) > 0 {
		return fmt.Errorf("%w: host %q is used by Applications %s", domain.ErrInvalid, name, strings.Join(apps, ", "))
	}
	return c.cfg.Store.DeleteHost(ctx, name)
}

func (c *Controller) CheckRepo(ctx context.Context, repoURL string) (domain.RepoAccess, error) {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return domain.RepoAccess{}, fmt.Errorf("%w: repoURL is required", domain.ErrInvalid)
	}
	return c.cfg.Source.CheckAccess(ctx, repoURL)
}

// ---- Host Keys ----

// HostKeys is the public half of the Host's Host Key, which its Agent reported at enrollment (Q108).
func (c *Controller) HostKeys(ctx context.Context, host string) ([]domain.HostKey, error) {
	h, err := c.cfg.Store.GetHost(ctx, host)
	if err != nil || h.AgeRecipient == "" {
		return []domain.HostKey{}, err
	}
	return []domain.HostKey{{Recipient: h.AgeRecipient, Current: true}}, nil
}
