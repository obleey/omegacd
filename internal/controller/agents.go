package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/workdir"
)

// errTakenOver: the Application's Agent already runs its target (Q119); nothing was sent.
var errTakenOver = errors.New("taken over: its Agent already runs this Revision")

// takenOver reports whether a, never deployed by this Control Plane, already runs on its Host at the tree r: its Agent's
// last resync had the same checksum for it (Q119).
func (c *Controller) takenOver(ctx context.Context, a domain.Application, r domain.Rendered) bool {
	if _, err := c.cfg.Store.LastSucceededOp(ctx, a.Name); !errors.Is(err, domain.ErrNotFound) {
		return false
	}
	h, err := c.cfg.Store.GetHost(ctx, a.Host)
	if err != nil || r.Checksum == "" || h.AgentApps[a.Name] != r.Checksum {
		c.cfg.Log.DebugContext(ctx, "not a takeover: its Agent doesn't run this tree", "app", a.Name, "err", err)
		return false
	}
	c.cfg.Log.InfoContext(ctx, "taken over: its agent already runs this revision", "app", a.Name, "host", a.Host)
	return true
}

// deploy sends a's Desired State at op's Revision to its Host's Agent, which fetches the files it lacks, decrypts and
// deploys them, and waits for its report. The files travel as rendered: secrets stay encrypted until the Agent.
func (c *Controller) deploy(ctx context.Context, a domain.Application, op domain.SyncOperation, r domain.Rendered) error {
	ds, err := desiredState(a, op.Revision, op.ID, r)
	if err != nil {
		return err
	}
	if err := c.keepFiles(ctx, r.Files); err != nil {
		return err
	}
	c.cfg.Log.DebugContext(ctx, "desired state to the agent", "files", len(ds.Files), "tree", r.Checksum)
	return c.cfg.Agents.Deploy(ctx, a.Host, ds)
}

// keepFiles copies the files the Control Plane's store lacks from the Source Server (Q121): every file an Agent may
// ask for is here before the Desired State goes.
func (c *Controller) keepFiles(ctx context.Context, files []domain.File) error {
	copied := 0
	for _, f := range files {
		if c.cfg.Blobs.Has(f.SHA256) {
			continue
		}
		b, err := c.cfg.Source.Blob(ctx, f.SHA256)
		if err == nil {
			_, err = c.cfg.Blobs.Put(b)
		}
		if err != nil {
			return fmt.Errorf("keep %s from the Source Server: %w", f.Path, err)
		}
		copied++
	}
	c.cfg.Log.DebugContext(ctx, "files kept for the agents", "copied", copied, "files", len(files))
	return nil
}

// desiredState is a's Desired State at revision, rendered as r, for the Sync Operation op (0: none, e.g. a Diff).
func desiredState(a domain.Application, revision string, op int64, r domain.Rendered) (*agentv1.DesiredState, error) {
	ds := &agentv1.DesiredState{App: a.Name, Revision: revision, ComposeFiles: a.ComposeFiles, Profiles: a.Profiles,
		WorkDir: a.WorkDir, Op: uint64(op), Prune: a.Prune, PruneVolumes: a.Prune && a.PruneVolumes,
		SelfHeal: a.SelfHeal, Parent: a.Parent, Checksum: r.Checksum, Adopt: a.Adopt}
	for _, f := range r.Files {
		sum, err := hex.DecodeString(f.SHA256)
		if err != nil {
			return nil, fmt.Errorf("file %s: %w", f.Path, err)
		}
		ds.Files = append(ds.Files, &agentv1.File{Path: f.Path, Mode: uint32(f.Mode), Sha256: sum})
	}
	return ds, nil
}

// agentDiff fills in the Diff view of an Application on an Agent's Host (M4-11): the service changes from its Agent,
// which stages the target Revision without deploying; offline, the last ones it computed, saying so. The compose text
// of both Revisions comes from what the Control Plane rendered (compose files are never secret; encrypted ones stay
// ciphertext here).
func (c *Controller) agentDiff(ctx context.Context, a domain.Application, dv domain.DiffView, r domain.Rendered) (domain.DiffView, error) {
	ds, err := desiredState(a, dv.ToSHA, 0, r)
	if err != nil {
		return dv, err
	}
	services, err := c.cfg.Agents.Diff(ctx, a.Host, ds)
	switch {
	case err == nil:
		dv.Services = services
		if serr := c.cfg.Store.SaveAgentDiff(ctx, a.Name, domain.AgentDiff{Revision: dv.ToSHA, Services: services, At: time.Now()}); serr != nil {
			c.cfg.Log.WarnContext(ctx, "keep the agent's diff", "app", a.Name, "err", serr)
		}
	case errors.Is(err, agents.ErrOffline) || errors.Is(err, agents.ErrNotApproved) || errors.Is(err, agents.ErrDisconnected):
		last, lerr := c.cfg.Store.AgentDiff(ctx, a.Name)
		if lerr != nil {
			return dv, fmt.Errorf("Host %s offline, and its Agent never computed a Diff of %s: %w", a.Host, a.Name, err)
		}
		dv.Services = last.Services
		note := "Host " + a.Host + " offline: the services' changes as its Agent computed them at " + last.At.Local().Format(time.DateTime)
		if last.Revision != dv.ToSHA {
			note += ", for Revision " + short(last.Revision) + " (not the target)"
		}
		dv.Warnings = append(dv.Warnings, note)
	default:
		return dv, err
	}
	if dv.Desired, err = c.renderedCompose(ctx, a, r); err != nil {
		return dv, err
	}
	if dv.FromSHA != "" { // the deployed Revision's: shown beside, the Diff stands without it
		dr, err := c.render(ctx, a, dv.FromSHA)
		if err == nil {
			dv.Live, err = c.renderedCompose(ctx, a, dr)
		}
		if err != nil {
			c.cfg.Log.WarnContext(ctx, "the deployed revision's compose files", "app", a.Name, "revision", short(dv.FromSHA), "err", err)
		}
	}
	return dv, nil
}

// renderedCompose is a's compose files' text as rendered in r, staged here as files (no decryption: none is needed).
func (c *Controller) renderedCompose(ctx context.Context, a domain.Application, r domain.Rendered) (string, error) {
	stage, done, err := c.stage(ctx, a, r)
	if err != nil {
		return "", err
	}
	defer done()
	return composeText(a, stage)
}

// stage writes r's files, as rendered, into a temporary folder; done removes it.
func (c *Controller) stage(ctx context.Context, a domain.Application, r domain.Rendered) (dir string, done func(), err error) {
	tmp, err := os.MkdirTemp("", "omegacd-stage-")
	if err != nil {
		return "", nil, err
	}
	done = func() { os.RemoveAll(tmp) }
	dir = filepath.Join(tmp, a.Name)
	if err := workdir.Write(dir, r.Files, func(hash string) ([]byte, error) { return c.Blob(ctx, hash) }); err != nil {
		done()
		return "", nil, err
	}
	return dir, done, nil
}

// composeText is the app's compose files in dir as written, one after another when there are several.
func composeText(a domain.Application, dir string) (string, error) {
	var b strings.Builder
	files := a.ComposeFilesIn(dir)
	for _, f := range files {
		content, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "", err
		}
		if len(files) > 1 {
			fmt.Fprintf(&b, "# ---- %s ----\n", f)
		}
		b.Write(content)
		if len(content) > 0 && content[len(content)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

// AgentReported shows what an Agent's report changed (M3-06): its Sync Operations, then its status.
func (c *Controller) AgentReported(ctx context.Context, app string) {
	a, err := c.cfg.Store.GetApp(ctx, app)
	if err != nil {
		return
	}
	if ops, err := c.cfg.Store.ListOps(ctx, app); err == nil && len(ops) > 0 {
		c.emit(domain.Event{Type: "op", App: app, Data: ops[0]})
	}
	c.reconcile(ctx, a)
}

// Resynced takes a Host's Agent's resync (Q78): apps it runs with their deployed trees' checksums, active the Sync
// Operations still going on it. They are kept for a takeover (Q119); the ones the Control Plane doesn't know are listed
// as on the Host, not managed (Q118); a Sync Operation still
// Running that isn't active ended where this Control Plane can't know how (a database restored from a backup) and is
// closed as Unknown (Q116); the Syncs that waited for the Host run, each as the same Sync Operation (Q81).
func (c *Controller) Resynced(ctx context.Context, host string, running map[string]string, active []uint64) {
	apps, err := c.cfg.Store.ListApps(ctx)
	if err != nil {
		c.cfg.Log.ErrorContext(ctx, "list applications", "err", err)
		return
	}
	known := map[string]bool{}
	for _, a := range apps {
		if a.Host != host || a.Kind != domain.KindCompose {
			continue
		}
		known[a.Name] = true
		if a.Deleting != "" {
			go c.finishDeleting(context.WithoutCancel(ctx), a.Name)
			continue
		}
		ops, err := c.cfg.Store.ListOps(ctx, a.Name)
		if err != nil {
			c.cfg.Log.ErrorContext(ctx, "list sync operations", "app", a.Name, "err", err)
			continue
		}
		if len(ops) == 0 {
			continue // never synced
		}
		c.closeLost(ctx, host, a.Name, ops, active)
		if _, runs := running[a.Name]; !runs && ops[0].Phase != domain.OpRunning && ops[0].Phase != domain.OpWaiting {
			c.restoreOnHost(ctx, a, ops)
		}
		if ops[0].Phase != domain.OpWaiting {
			continue
		}
		op := ops[0]
		c.mu.Lock()
		s := c.state(a.Name)
		busy := s.syncing || s.deleting
		if !busy {
			s.syncing = true
		}
		c.mu.Unlock()
		if busy {
			continue
		}
		if err := c.cfg.Store.SetOpPhase(ctx, op.ID, domain.OpRunning, "Host "+host+" is back"); err != nil {
			c.cfg.Log.ErrorContext(ctx, "run the sync that waited for the host", "app", a.Name, "op", op.ID, "err", err)
			c.setSyncing(a.Name, false)
			continue
		}
		op.Phase = domain.OpRunning
		c.cfg.Log.InfoContext(ctx, "host back: running the sync that waited for it", "app", a.Name, "op", op.ID)
		c.emit(domain.Event{Type: "op", App: a.Name, Data: op})
		go c.runSync(a, op)
	}
	var unmanaged []string
	for _, name := range slices.Sorted(maps.Keys(running)) {
		if !known[name] {
			unmanaged = append(unmanaged, name)
		}
	}
	if err := c.cfg.Store.SetAgentApps(ctx, host, running, unmanaged); err != nil {
		c.cfg.Log.ErrorContext(ctx, "record the applications on the host, not managed", "host", host, "err", err)
	} else if len(unmanaged) > 0 {
		c.cfg.Log.DebugContext(ctx, "on the host, not managed", "host", host, "apps", unmanaged)
	}
}

// triggerRestore marks a Sync that deploys the deployed Revision again on a Host whose Agent doesn't run it (Q123).
const triggerRestore = "restore"

// restoreOnHost deploys a's deployed Revision (the newest Succeeded of ops) again, from its kept render: its Agent
// doesn't run it, most likely reinstalled (Q123). Never deployed: nothing to restore.
func (c *Controller) restoreOnHost(ctx context.Context, a domain.Application, ops []domain.SyncOperation) {
	i := slices.IndexFunc(ops, func(op domain.SyncOperation) bool { return op.Phase == domain.OpSucceeded })
	if i < 0 {
		c.cfg.Log.DebugContext(ctx, "not on its host, never deployed: nothing to restore", "app", a.Name)
		return
	}
	c.cfg.Log.InfoContext(ctx, "its agent doesn't run it: deploying the deployed revision again", "app", a.Name,
		"host", a.Host, "revision", short(ops[i].Revision))
	if _, err := c.startSync(a, ops[i].Revision, ops[i].TreeHash, triggerRestore); err != nil && !errors.Is(err, domain.ErrBusy) {
		c.cfg.Log.ErrorContext(ctx, "restore on its host", "app", a.Name, "err", err)
	}
}

// closeLost closes app's Running Sync Operations that are neither active on its Host's Agent nor being run here as
// Unknown (Q116): after a restore from a backup their result went to the Control Plane that is gone; a Desired State
// lost with a dropped connection never reached the Agent. The Agent's outbox came first, so a result it has is in.
func (c *Controller) closeLost(ctx context.Context, host, app string, ops []domain.SyncOperation, active []uint64) {
	c.mu.Lock()
	syncing := c.state(app).syncing
	c.mu.Unlock()
	for _, op := range ops {
		if op.Phase != domain.OpRunning || syncing || slices.Contains(active, uint64(op.ID)) {
			continue
		}
		msg := "result unknown: Host " + host + "'s Agent neither runs it nor reported how it ended (the Control Plane's " +
			"database is older than the Host, restored from a backup, or the Sync was lost with a dropped connection); " +
			"the Application's state is what its Agent reports"
		if err := c.cfg.Store.FinishOp(ctx, op.ID, domain.OpUnknown, msg); err != nil {
			c.cfg.Log.ErrorContext(ctx, "close a lost sync operation", "app", app, "op", op.ID, "err", err)
			continue
		}
		op.Phase, op.Message = domain.OpUnknown, msg
		c.cfg.Log.WarnContext(ctx, "a sync operation's result is unknown: its host's agent neither runs it nor reported it", "app", app, "op", op.ID)
		c.emit(domain.Event{Type: "op", App: app, Data: op})
	}
}

// AgentRemoved finishes a delete an Agent reported from its outbox (Q83): the store already dropped the Application.
func (c *Controller) AgentRemoved(ctx context.Context, host, app string) {
	c.mu.Lock()
	s := c.apps[app]
	delete(c.apps, app)
	c.mu.Unlock()
	c.emit(domain.Event{Type: "app-deleted", App: app, Data: app})
	if s != nil && s.view.Parent != "" {
		c.finishDeleting(ctx, s.view.Parent)
	}
}

// AgentProgress shows a Sync's progress on an Agent live (Q87).
func (c *Controller) AgentProgress(_ context.Context, app string, p domain.ProgressEvent) {
	c.emit(domain.Event{Type: "progress", App: app, Data: p})
}

// ForgetApp removes an Application from the Control Plane only (Q92): its Host is gone for good, or its containers
// should stay as they are; nothing is asked of any Agent or engine.
func (c *Controller) ForgetApp(ctx context.Context, name string) error {
	a, err := c.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return err
	}
	if a.Kind == domain.KindApps {
		return fmt.Errorf("%w: an App of Apps is deleted, not forgotten: forget its children", domain.ErrInvalid)
	}
	c.mu.Lock()
	s := c.state(name)
	if s.syncing {
		c.mu.Unlock()
		return domain.ErrBusy
	}
	s.deleting = true
	c.mu.Unlock()
	if err := c.cfg.Store.DeleteApp(ctx, name); err != nil { // coverage: unreachable, the app was just read; only another forget racing this one
		c.mu.Lock()
		s.deleting = false
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	delete(c.apps, name)
	c.mu.Unlock()
	c.cfg.Log.InfoContext(ctx, "application forgotten: left as it is on its Host", "app", name, "host", a.Host)
	c.emit(domain.Event{Type: "app-deleted", App: name, Data: name})
	if a.Parent != "" {
		c.finishDeleting(ctx, a.Parent)
	}
	return nil
}

// AgentStatusChanged shows an Application's new report from its Agent (M4-02).
func (c *Controller) AgentStatusChanged(ctx context.Context, app string) {
	a, err := c.cfg.Store.GetApp(ctx, app)
	if err != nil {
		c.cfg.Log.DebugContext(ctx, "a report for an application not here (deleted meanwhile)", "app", app, "err", err)
		return
	}
	c.reconcileWith(ctx, a, false)
}

// offline says since when an Application's Host is offline and how old what is shown of it is (Q38).
func (c *Controller) offline(ctx context.Context, a domain.Application) string {
	msg := "Host " + a.Host + " offline"
	if h, err := c.cfg.Store.GetHost(ctx, a.Host); err == nil && h.Agent != nil && !h.Agent.LeftAt.IsZero() {
		msg += " since " + h.Agent.LeftAt.Local().Format(time.DateTime)
	}
	if st, err := c.cfg.Store.AgentStatus(ctx, a.Name); err == nil {
		msg += "; last known state from " + st.At.Local().Format(time.DateTime)
	}
	return msg
}

// agentLive is what runs for an Application on an Agent's Host, as its Agent last reported (M4-02): the same view,
// online or, last known, offline.
func (c *Controller) agentLive(ctx context.Context, a domain.Application) domain.LiveState {
	st, err := c.cfg.Store.AgentStatus(ctx, a.Name)
	if err != nil {
		return domain.LiveState{Services: []string{}, Containers: []domain.Container{}, Resources: []domain.Resource{}, Health: domain.HealthUnknown}
	}
	return st.Live
}

// agentDetail fills in a resource of an Application on an Agent's Host: a volume's or network's details as its Agent
// reported them; a file's content from the Revision the Control Plane rendered, as in Git. A secret's content is only
// on the Host: shown as such.
func (c *Controller) agentDetail(ctx context.Context, a domain.Application, d domain.ResourceDetail) (domain.ResourceDetail, error) {
	if d.Kind == "volume" || d.Kind == "network" {
		st, err := c.cfg.Store.AgentStatus(ctx, a.Name)
		if err != nil && !errors.Is(err, domain.ErrNotFound) { // none yet: no details
			return d, err
		}
		d.Info = st.Details[d.Kind+"/"+d.Name]
		return d, nil
	}
	if d.Secret {
		d.Info = map[string]string{"type": "file", "secret": "decrypted on the Host: its content never leaves it"}
		return d, nil
	}
	last, err := c.cfg.Store.LastSucceededOp(ctx, a.Name)
	if err != nil {
		return d, err
	}
	r, err := c.render(ctx, a, last.Revision)
	if err != nil {
		return d, err
	}
	i := slices.IndexFunc(r.Files, func(f domain.File) bool { return f.Path == filepath.ToSlash(d.Name) })
	if i < 0 {
		d.Info = map[string]string{"type": "on the Host only"} // made there, e.g. a bind mount's folder
		return d, nil
	}
	b, err := c.cfg.Source.Blob(ctx, r.Files[i].SHA256)
	if err != nil {
		return d, err
	}
	d.Info = map[string]string{"type": "file", "size": fmt.Sprintf("%d bytes", len(b))}
	if len(b) > maxResourceContent {
		b = b[:maxResourceContent]
		d.Info["truncated"] = "showing the first 256 KiB"
	}
	d.Content = string(b)
	return d, nil
}
