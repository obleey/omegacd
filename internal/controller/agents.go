package controller

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// agentHost reports whether the Host is reached through an enrolled Agent (Q80): it has no URL, and an Agent's key.
// The built-in Host and push Hosts (SSH/TLS) are not, until M4-04 and M4-05.
func (c *Controller) agentHost(ctx context.Context, host string) bool {
	if c.cfg.Agents == nil {
		return false
	}
	h, err := c.cfg.Store.GetHost(ctx, cmp.Or(host, domain.LocalHost)) // local: when the built-in Agent enrolled it (M4-04)
	return err == nil && h.URL == "" && h.Fingerprint != ""
}

// deploy sends a's Desired State at op's Revision to its Host's Agent, which fetches the files it lacks, decrypts and
// deploys them, and waits for its report. The files travel as rendered: secrets stay encrypted until the Agent.
func (c *Controller) deploy(ctx context.Context, a domain.Application, op domain.SyncOperation, r domain.Rendered) error {
	ds, err := desiredState(a, op.Revision, op.ID, r)
	if err != nil {
		return err
	}
	c.cfg.Log.DebugContext(ctx, "desired state to the agent", "files", len(ds.Files), "tree", r.Checksum)
	return c.cfg.Agents.Deploy(ctx, a.Host, ds)
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

// Resynced runs the Syncs that waited for the Host (Q81) now that its Agent is back, each as the same Sync Operation.
func (c *Controller) Resynced(ctx context.Context, host string) {
	apps, err := c.cfg.Store.ListApps(ctx)
	if err != nil {
		c.cfg.Log.ErrorContext(ctx, "list applications", "err", err)
		return
	}
	for _, a := range apps {
		if a.Host != host || a.Kind != domain.KindCompose {
			continue
		}
		if a.Deleting != "" {
			go c.finishDeleting(context.WithoutCancel(ctx), a.Name)
			continue
		}
		ops, err := c.cfg.Store.ListOps(ctx, a.Name)
		if err != nil || len(ops) == 0 || ops[0].Phase != domain.OpWaiting {
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
			c.setSyncing(a.Name, false)
			continue
		}
		op.Phase = domain.OpRunning
		c.cfg.Log.InfoContext(ctx, "host back: running the sync that waited for it", "app", a.Name, "op", op.ID)
		c.emit(domain.Event{Type: "op", App: a.Name, Data: op})
		go c.runSync(a, op)
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
		st, _ := c.cfg.Store.AgentStatus(ctx, a.Name)
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
