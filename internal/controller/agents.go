package controller

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// agentHost reports whether the Host is reached through an enrolled Agent (Q80): it has no URL, and an Agent's key.
// The built-in Host and push Hosts (SSH/TLS) are not, until M4-04 and M4-05.
func (c *Controller) agentHost(ctx context.Context, host string) bool {
	if c.cfg.Agents == nil || host == "" || host == domain.LocalHost {
		return false
	}
	h, err := c.cfg.Store.GetHost(ctx, host)
	return err == nil && h.URL == "" && h.Fingerprint != ""
}

// deploy sends a's Desired State at op's Revision to its Host's Agent, which fetches the files it lacks, decrypts and
// deploys them, and waits for its report. The files travel as rendered: secrets stay encrypted until the Agent.
func (c *Controller) deploy(ctx context.Context, a domain.Application, op domain.SyncOperation, r domain.Rendered) error {
	ds := &agentv1.DesiredState{App: a.Name, Revision: op.Revision, ComposeFiles: a.ComposeFiles, Profiles: a.Profiles,
		WorkDir: a.WorkDir, Op: uint64(op.ID), Prune: a.Prune, PruneVolumes: a.Prune && a.PruneVolumes,
		SelfHeal: a.SelfHeal, Parent: a.Parent, Checksum: r.Checksum, Adopt: a.Adopt}
	for _, f := range r.Files {
		sum, err := hex.DecodeString(f.SHA256)
		if err != nil {
			return fmt.Errorf("file %s: %w", f.Path, err)
		}
		ds.Files = append(ds.Files, &agentv1.File{Path: f.Path, Mode: uint32(f.Mode), Sha256: sum})
	}
	c.cfg.Log.DebugContext(ctx, "desired state to the agent", "files", len(ds.Files), "tree", r.Checksum)
	return c.cfg.Agents.Deploy(ctx, a.Host, ds)
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
