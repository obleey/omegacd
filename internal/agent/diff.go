package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/wire"
	"github.com/obleey/omegacd/internal/workdir"
)

// stagingDir holds Revisions staged for a Diff, under the work dir (RAM by default: their decrypted values never touch
// a disk, Q19), one owner-only folder each, removed when the Diff is done.
const stagingDir = ".omegacd-diff"

// diff is how a Revision not deployed yet would change what runs (M4-11): its files staged from the cache, or fetched
// from the Control Plane, decrypted and diffed as if they were in the Application's folder. Nothing is deployed and
// nothing is recorded: the cache and its Revisions stay as they are.
func (ag *Agent) diff(ctx context.Context, ds *agentv1.DesiredState, fetch func(hash string) ([]byte, error)) ([]domain.ServiceDiff, error) {
	a, r := fromDesired(ds)
	os.MkdirAll(ag.work.Root, 0o750) // as the WorkDir makes it, for an engine running as another user; a failure shows below
	if err := os.MkdirAll(filepath.Join(ag.work.Root, stagingDir), 0o700); err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	rand.Read(b)
	stage := filepath.Join(ag.work.Root, stagingDir, a.Name+"-"+hex.EncodeToString(b))
	defer os.RemoveAll(stage)
	blob := func(hash string) ([]byte, error) {
		if b, err := ag.cache.Blob(hash); err == nil {
			return b, nil
		}
		return fetch(hash)
	}
	if err := workdir.Write(stage, r.Files, blob); err != nil {
		return nil, err
	}
	if err := os.Chmod(stage, 0o700); err != nil { // coverage: unreachable, the folder was just made by this user
		return nil, err
	}
	if _, err := ag.secrets.Decrypt(stage); err != nil {
		return nil, err
	}
	app := application(a, r)
	for _, f := range app.ComposeFilesIn(stage) {
		if _, err := os.Stat(filepath.Join(stage, f)); errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: compose file %q not found in the Revision %s", domain.ErrInvalid, f, r.Revision)
		}
	}
	if err := ag.engine.Validate(ctx, app, stage); err != nil {
		return nil, err
	}
	return ag.engine.Diff(ctx, app, stage, ag.work.Dir(a))
}

// answerDiff answers a DiffRequest, in its log context.
func (ag *Agent) answerDiff(ctx context.Context, conn *wire.Conn, b *blobs, id uint64, req *agentv1.DiffRequest) {
	ds := req.GetDesiredState()
	ctx = logging.With(ctx, "app", ds.GetApp(), "revision", ds.GetRevision())
	services, err := ag.diff(ctx, ds, func(hash string) ([]byte, error) { return b.fetch(ctx, hash) })
	res := &agentv1.DiffResult{}
	if err != nil {
		ag.log.WarnContext(ctx, "diff of a revision not deployed", "err", err)
		res.Error = err.Error()
	} else {
		res.Services, _ = json.Marshal(services)
		ag.log.DebugContext(ctx, "diff of a revision not deployed", "services", len(services))
	}
	conn.Send(ctx, &agentv1.Envelope{ReplyTo: id, Body: &agentv1.Envelope_DiffResult{DiffResult: res}})
}
