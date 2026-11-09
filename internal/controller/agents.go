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
