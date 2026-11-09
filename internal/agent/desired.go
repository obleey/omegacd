package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/wire"
)

// blobs fetches files over the connection for a Deploy: one BlobRequest per missing file, answered by its chunks, or
// by an Error to the request. ponytail: one file at a time; ask for all missing files at once if Syncs of many new
// files get slow.
type blobs struct {
	conn *wire.Conn
	asm  wire.Assembler

	mu      sync.Mutex
	waiting map[string]chan blobResult // by sha256 hex
	asked   map[uint64]string          // request id → sha256 hex
}

type blobResult struct {
	data []byte
	err  error
}

func newBlobs(conn *wire.Conn) *blobs {
	return &blobs{conn: conn, waiting: map[string]chan blobResult{}, asked: map[uint64]string{}}
}

// fetch is a file's content by its sha256 (hex), from the Control Plane.
func (b *blobs) fetch(ctx context.Context, hash string) ([]byte, error) {
	sum, err := hex.DecodeString(hash)
	if err != nil {
		return nil, fmt.Errorf("blob %q: %w", hash, err)
	}
	got := make(chan blobResult, 1)
	req := &agentv1.Envelope{Body: &agentv1.Envelope_BlobRequest{BlobRequest: &agentv1.BlobRequest{
		Sha256: [][]byte{sum}, Offset: []uint64{b.asm.Offset(sum)}}}}
	// under the lock: an answer is read (refused, chunk) only once the request is known by its id
	b.mu.Lock()
	b.waiting[hash] = got
	err = b.conn.Send(ctx, req)
	b.asked[req.Id] = hash
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case r := <-got:
		return r.data, r.err
	case <-b.conn.Done():
		return nil, fmt.Errorf("blob %s: the connection to the Control Plane dropped", hash)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// chunk takes a BlobChunk; a completed blob goes to its fetch.
func (b *blobs) chunk(c *agentv1.BlobChunk) {
	data, done, err := b.asm.Add(c)
	if err == nil && !done {
		return
	}
	b.deliver(hex.EncodeToString(c.Sha256), blobResult{data, err})
}

// refused takes an Error answering a request; it reports whether it answered one of b's.
func (b *blobs) refused(replyTo uint64, msg string) bool {
	b.mu.Lock()
	hash, ok := b.asked[replyTo]
	delete(b.asked, replyTo)
	b.mu.Unlock()
	if ok {
		b.deliver(hash, blobResult{err: errors.New(msg)})
	}
	return ok
}

func (b *blobs) deliver(hash string, r blobResult) {
	b.mu.Lock()
	got := b.waiting[hash]
	delete(b.waiting, hash)
	for id, h := range b.asked {
		if h == hash {
			delete(b.asked, id)
		}
	}
	b.mu.Unlock()
	if got != nil {
		got <- r
	}
}

// adoptable refuses a first deploy over a Compose project of the same name that OmegaCD didn't start, unless the
// Application adopts it: a hand-started stack is never taken over by accident.
func (ag *Agent) adoptable(ctx context.Context, ds *agentv1.DesiredState) error {
	if _, err := ag.db.GetApp(ctx, ds.App); err == nil || ds.Adopt {
		return err
	}
	exists, managed, err := ag.engine.ProjectExists(ctx, domain.LocalHost, ds.App)
	if err != nil {
		return err
	}
	if exists && !managed {
		return fmt.Errorf("%w: a Compose project %s runs on this Host that OmegaCD didn't start: set adopt: true to take it over",
			domain.ErrUnmanaged, ds.App)
	}
	return nil
}

// desired deploys a Desired State the Control Plane sent and reports the result, in the Sync Operation's log context
// (Q62). A report that can't be sent goes to the outbox (Q93).
func (ag *Agent) desired(ctx context.Context, conn *wire.Conn, b *blobs, ds *agentv1.DesiredState) {
	ctx = logging.With(ctx, "op", ds.Op)
	if _, busy := ag.deploying.LoadOrStore(ds.Op, true); busy && ds.Op != 0 {
		ag.log.DebugContext(ctx, "desired state again while deploying it: ignored", "app", ds.App)
		return
	}
	defer ag.deploying.Delete(ds.Op)
	a := App{Name: ds.App, Parent: ds.Parent, WorkDir: ds.WorkDir, SelfHeal: ds.SelfHeal, Prune: ds.Prune,
		PruneVolumes: ds.PruneVolumes}
	r := Revision{App: ds.App, Revision: ds.Revision, Checksum: ds.Checksum, ComposeFiles: ds.ComposeFiles, Profiles: ds.Profiles}
	for _, f := range ds.Files {
		r.Files = append(r.Files, domain.File{Path: f.Path, Mode: fs.FileMode(f.Mode), SHA256: hex.EncodeToString(f.Sha256)})
	}
	err := ag.adoptable(ctx, ds)
	if err == nil {
		err = ag.Deploy(ctx, a, r, func(hash string) ([]byte, error) { return b.fetch(ctx, hash) }, nil)
	}
	rep := &agentv1.SyncOperationReport{App: ds.App, Revision: ds.Revision, Op: ds.Op, Checksum: ds.Checksum,
		Phase: string(domain.OpSucceeded), Message: "synced to " + ds.Revision}
	if err != nil {
		ag.log.ErrorContext(ctx, "deploy failed", "app", ds.App, "err", err)
		rep.Phase, rep.Message = string(domain.OpFailed), err.Error()
	}
	if err := conn.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_SyncOperationReport{SyncOperationReport: rep}}); err != nil {
		ag.log.WarnContext(ctx, "the Control Plane is gone: the Sync's result waits in the outbox", "app", ds.App, "err", err)
		if err := ag.record(context.WithoutCancel(ctx), rep); err != nil {
			ag.log.ErrorContext(ctx, "record the Sync's result", "err", err)
		}
	}
}

// resyncInterval is how often a connected Agent sends its checksums, besides on every connect (Q78).
var resyncInterval = 5 * time.Minute // tests wait less

// keepInSync runs while a connection is approved: it sends the outbox entries the Control Plane hasn't acknowledged,
// oldest first, then the checksums of what is deployed (Q78); again whenever an entry is recorded and every
// resyncInterval, until the connection ends. Each entry goes once per connection; one not acknowledged goes again on
// the next.
func (ag *Agent) keepInSync(ctx context.Context, conn *wire.Conn) {
	var sent uint64
	tick := time.NewTicker(resyncInterval)
	defer tick.Stop()
	for {
		entries, err := ag.db.Outbox(ctx)
		if err != nil {
			ag.log.ErrorContext(ctx, "read the outbox", "err", err)
		}
		for _, e := range entries {
			if e.Seq <= sent {
				continue
			}
			if err := conn.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_OutboxEntry{OutboxEntry: e}}); err != nil {
				return
			}
			sent = e.Seq
		}
		if err := conn.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Resync{Resync: ag.checksums(ctx)}}); err != nil {
			return
		}
		select {
		case <-conn.Done():
			return
		case <-ctx.Done():
			return
		case <-ag.recorded:
		case <-tick.C:
		}
	}
}

// checksums are the deployed Revisions' checksums, by Application.
func (ag *Agent) checksums(ctx context.Context) *agentv1.Resync {
	r := &agentv1.Resync{Checksums: map[string][]byte{}}
	apps, err := ag.db.ListApps(ctx)
	if err != nil {
		ag.log.ErrorContext(ctx, "resync: list applications", "err", err)
	}
	for _, a := range apps {
		if a.Revision == "" {
			continue
		}
		rev, err := ag.cache.Revision(ctx, a.Name, a.Revision)
		if sum, herr := hex.DecodeString(rev.Checksum); err == nil && herr == nil {
			r.Checksums[a.Name] = sum
		}
	}
	return r
}
