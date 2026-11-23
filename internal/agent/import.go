package agent

import (
	"context"
	"encoding/json"

	"github.com/obleey/omegacd/internal/importer"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/wire"
)

// answerImport answers an ImportRequest (M6-05, Q317): the importer's half that reads this Host's stacks, one
// ImportResult per stack, every secret encrypted before it leaves the Host (Q230), then one with done set.
func (ag *Agent) answerImport(ctx context.Context, conn *wire.Conn, id uint64, req *agentv1.ImportRequest) {
	reply := func(r *agentv1.ImportResult) error {
		return conn.Send(ctx, &agentv1.Envelope{ReplyTo: id, Body: &agentv1.Envelope_ImportResult{ImportResult: r}})
	}
	err := ag.importStacks(ctx, req, func(r importer.Result) error {
		files := r.Files
		r.Files = nil           // raw bytes beside the JSON, not base64 in it: a stack's answer stays one frame (Q319)
		b, _ := json.Marshal(r) // a Result is strings, lists and bytes: it always encodes
		return reply(&agentv1.ImportResult{Result: b, Files: files})
	})
	done := &agentv1.ImportResult{Done: true}
	if err != nil {
		ag.log.WarnContext(ctx, "import", "err", err)
		done.Error = err.Error()
	} else {
		ag.log.InfoContext(ctx, "import answered")
	}
	reply(done)
}

func (ag *Agent) importStacks(ctx context.Context, req *agentv1.ImportRequest, each func(importer.Result) error) error {
	var r importer.Request
	if err := json.Unmarshal(req.Request, &r); err != nil {
		return err
	}
	h, err := ag.importHost()
	if err != nil {
		return err
	}
	defer h.Close()
	return importer.Plan(ctx, h, r, each)
}
