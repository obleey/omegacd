package agent

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/wire"
)

// reportDelay gathers a burst of engine events into one report.
var reportDelay = 500 * time.Millisecond // tests wait less

// statusReport is an Application's state as this Agent sees it (M4-02, Q23): Health, the containers, how they differ from the
// deployed Revision, the networks, volumes and files it uses (decrypted ones marked secret) and the volumes' and
// networks' details. It never holds a secret's value.
func (ag *Agent) statusReport(ctx context.Context, name string) (*agentv1.StatusReport, error) {
	a, r, err := ag.deployed(ctx, name)
	if err != nil {
		return nil, err
	}
	app := application(a, r)
	live, err := ag.engine.Status(ctx, app)
	if err != nil {
		return nil, err
	}
	drift, err := ag.engine.Diff(ctx, app, a.Dir, "")
	if err != nil {
		return nil, err
	}
	files, err := ag.engine.ProjectFiles(ctx, app, a.Dir)
	if err != nil {
		return nil, err
	}
	details := map[string]map[string]string{}
	for _, res := range live.Resources {
		if res.Kind == "volume" || res.Kind == "network" {
			if info, err := ag.engine.Inspect(ctx, app, res.Kind, res.Name); err == nil {
				details[res.Kind+"/"+res.Name] = info
			}
		}
	}
	for _, f := range files {
		f.Secret = f.Secret || f.Kind == "secret" || decryptedFrom(f.Name, r.Files)
		live.Resources = append(live.Resources, f)
	}
	lj, _ := json.Marshal(live)
	dj, _ := json.Marshal(drift)
	xj, _ := json.Marshal(details)
	return &agentv1.StatusReport{App: name, Revision: r.Revision, Health: string(live.Health), Drifted: len(drift) > 0,
		LiveState: lj, Drift: dj, Details: xj, TimeUnix: time.Now().Unix()}, nil
}

// decryptedFrom reports whether file (relative to the app's folder) is what the Agent decrypted from a rendered
// *.sops.* file next to it ("x.sops.env" becomes "x.env", ".sops.env" becomes ".env").
func decryptedFrom(file string, rendered []domain.File) bool {
	dir, base := path.Split(file)
	stem, ext, _ := strings.Cut(base, ".")
	if base[0] == '.' { // ".env", ".sops.env"
		stem, ext = "", base[1:]
	}
	for _, f := range rendered {
		if f.Path == dir+stem+".sops."+ext {
			return true
		}
	}
	return false
}

// reporter sends Applications' reports on the live connection, each a moment after the last change asked for it.
type reporter struct {
	ag      *Agent
	mu      sync.Mutex
	conn    *wire.Conn // nil while not connected
	pending map[string]bool
}

func (rp *reporter) connected(conn *wire.Conn) {
	rp.mu.Lock()
	rp.conn = conn
	rp.mu.Unlock()
}

// soon reports app after reportDelay, once however often it is asked meanwhile; nothing while not connected (the
// report on the next connect says it all).
func (rp *reporter) soon(ctx context.Context, app string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.conn == nil || rp.pending[app] {
		return
	}
	rp.pending[app] = true
	time.AfterFunc(reportDelay, func() {
		rp.mu.Lock()
		delete(rp.pending, app)
		conn := rp.conn
		rp.mu.Unlock()
		if conn != nil {
			rp.ag.sendReport(ctx, conn, app)
		}
	})
}

// sendReport sends app's report, if it has one (a deployed Revision).
func (ag *Agent) sendReport(ctx context.Context, conn *wire.Conn, app string) {
	rep, err := ag.statusReport(ctx, app)
	if err != nil {
		ag.log.DebugContext(ctx, "no status report", "app", app, "err", err)
		return
	}
	conn.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_StatusReport{StatusReport: rep}})
}

// chunks writes logs to the Control Plane, as LogChunks answering request id.
type chunks struct {
	ctx  context.Context
	conn *wire.Conn
	id   uint64
}

func (c chunks) Write(p []byte) (int, error) {
	err := c.conn.Send(c.ctx, &agentv1.Envelope{ReplyTo: c.id, Body: &agentv1.Envelope_LogChunk{LogChunk: &agentv1.LogChunk{Data: append([]byte(nil), p...)}}})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// streamLogs sends an Application's logs as asked (a container or service, tail, follow) until they end, the request
// is stopped (ctx) or the connection ends. Logs that end, or fail, close with a chunk saying done (and the error);
// stopped ones send nothing more: the viewer left (LogsStop), or the Agent is leaving, which the viewer hears as the
// Host disconnecting.
func (ag *Agent) streamLogs(ctx context.Context, conn *wire.Conn, id uint64, req *agentv1.LogsRequest) {
	err := ag.Logs(ctx, req.App, req.Service, req.Follow, int(req.Tail), chunks{ctx, conn, id})
	if ctx.Err() != nil {
		ag.log.DebugContext(ctx, "logs stopped: the viewer left, or this Agent's connection ended", "app", req.App)
		return
	}
	end := &agentv1.LogChunk{Done: true}
	if err != nil {
		end.Error = err.Error()
	}
	conn.Send(ctx, &agentv1.Envelope{ReplyTo: id, Body: &agentv1.Envelope_LogChunk{LogChunk: end}})
}
