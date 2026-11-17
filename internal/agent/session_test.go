package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/wire"
)

// pair is a connection between an Agent and a scripted Control Plane: the test speaks for the Control Plane on cp.
func pair(t *testing.T) (agent, cp *wire.Conn) {
	t.Helper()
	got := make(chan *wire.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := wire.Accept(w, r)
		if err != nil {
			return
		}
		got <- c
		<-c.Done()
	}))
	t.Cleanup(srv.Close)
	agent, err := wire.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cp = <-got
	t.Cleanup(func() { agent.Close(); cp.Close() })
	return agent, cp
}

func recv(t *testing.T, c *wire.Conn) *agentv1.Envelope {
	t.Helper()
	e, err := c.Recv(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A key rotation that can't happen leaves the old key, saying why (Q132): a leftover it can't replace, the Control
// Plane refusing the new key, the new key unable to replace the old one. A second request while one waits is ignored;
// an acknowledgement the database can't apply and an envelope the Agent doesn't know are logged.
func TestRotationFailures(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	key, err := SigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "signing.key")
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	db.Close() // acknowledgements fail
	logs := &logging.Buffer{}
	ag := New(db, nil, nil, nil, nil, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	a, cp := pair(t)
	go ag.serve(ctx, a, &Connection{Key: key, KeyPath: keyPath})
	rotate := &agentv1.Envelope{Body: &agentv1.Envelope_RotateKey{RotateKey: &agentv1.RotateKey{}}}

	os.MkdirAll(filepath.Join(keyPath+".next", "x"), 0o700)
	cp.Send(ctx, rotate)
	waitLog(t, logs, `msg="key rotation: no new key"`)
	os.RemoveAll(keyPath + ".next")

	cp.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_RotateKey{RotateKey: &agentv1.RotateKey{}}})
	kr := recv(t, cp)
	cp.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_RotateKey{RotateKey: &agentv1.RotateKey{}}})
	waitLog(t, logs, "key rotation asked again while one waits for its answer: ignored")
	cp.Send(ctx, &agentv1.Envelope{ReplyTo: kr.Id, Body: &agentv1.Envelope_Error{Error: &agentv1.Error{Message: "key changed meanwhile"}}})
	waitLog(t, logs, `msg="the Control Plane refused the new signing key; the old one stays" err="key changed meanwhile"`)

	os.Remove(keyPath)
	os.MkdirAll(filepath.Join(keyPath, "x"), 0o700) // the old key's place taken: the new one can't go there
	cp.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_RotateKey{RotateKey: &agentv1.RotateKey{}}})
	kr = recv(t, cp)
	cp.Send(ctx, &agentv1.Envelope{ReplyTo: kr.Id, Body: &agentv1.Envelope_EnrollmentStatus{EnrollmentStatus: &agentv1.EnrollmentStatus{Approved: true}}})
	waitLog(t, logs, "key rotation: the new key couldn't replace the old one")

	cp.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_OutboxAck{OutboxAck: &agentv1.OutboxAck{Seq: 1}}})
	waitLog(t, logs, `msg="outbox ack"`)
	cp.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Hello{Hello: &agentv1.Hello{}}})
	waitLog(t, logs, `msg="an envelope this Agent doesn't handle: ignored"`)
}

// Fetching a file: a hash that isn't one, the Control Plane refusing it, a file in two chunks, the request given up or
// its connection dropped while waiting, and a connection already gone.
func TestBlobFetch(t *testing.T) {
	ctx := t.Context()
	a, cp := pair(t)
	b := newBlobs(a)
	go func() { // the Agent's reads, as serve does them
		for {
			e, err := a.Recv(context.Background())
			if err != nil {
				return
			}
			if c := e.GetBlobChunk(); c != nil {
				b.chunk(c)
			} else if e.GetError() != nil {
				b.refused(e.ReplyTo, e.GetError().Message)
			}
		}
	}()
	if _, err := b.fetch(ctx, "zz"); err == nil {
		t.Error("a hash that isn't hex")
	}

	go func() {
		e := recv(t, cp)
		cp.Send(ctx, &agentv1.Envelope{ReplyTo: e.Id, Body: &agentv1.Envelope_Error{Error: &agentv1.Error{Message: "not a file of yours"}}})
	}()
	if _, err := b.fetch(ctx, blob.Hash([]byte("x"))); err == nil || err.Error() != "not a file of yours" {
		t.Errorf("refused: %v", err)
	}
	if b.refused(12345, "not asked") {
		t.Error("an answer to a request it never sent")
	}

	big := bytes.Repeat([]byte("a"), wire.ChunkSize+10)
	go func() {
		recv(t, cp)
		sum, _ := hex.DecodeString(blob.Hash(big))
		cp.SendBlob(ctx, sum, big, 0)
	}()
	if got, err := b.fetch(ctx, blob.Hash(big)); err != nil || !bytes.Equal(got, big) {
		t.Errorf("two chunks: %d bytes, %v", len(got), err)
	}

	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := b.fetch(cctx, blob.Hash([]byte("never answered"))); err != context.Canceled {
		t.Errorf("given up: %v", err)
	}
	time.AfterFunc(50*time.Millisecond, func() { cp.Close() })
	if _, err := b.fetch(ctx, blob.Hash([]byte("dropped"))); err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Errorf("dropped: %v", err)
	}
	<-a.Done()
	if _, err := b.fetch(ctx, blob.Hash([]byte("gone"))); err == nil {
		t.Error("asked over a connection gone")
	}
}

// keepInSync sends each outbox entry once per connection, the checksums every time, and stops when the connection
// can't take either.
func TestKeepInSyncSendsOnce(t *testing.T) {
	ctx := t.Context()
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	defer db.Close()
	ag := New(db, nil, nil, nil, nil, nil)
	db.Record(ctx, rollback("web", "r1"))
	a, cp := pair(t)
	done := make(chan struct{})
	go func() { ag.keepInSync(ctx, a); close(done) }()
	if recv(t, cp).GetOutboxEntry() == nil || recv(t, cp).GetResync() == nil {
		t.Fatal("not the entry, then the checksums")
	}
	ag.recorded <- struct{}{}
	if e := recv(t, cp); e.GetResync() == nil { // the entry again? not on this connection
		t.Fatalf("got %v", e)
	}
	a.Close()
	<-done

	ag.keepInSync(ctx, a) // the entry, on a connection gone: returns
	db.ClearOutbox(ctx)
	ag.keepInSync(ctx, a) // nothing to send but the checksums, on a connection gone: returns
}

// A Sync's result that can't be sent nor kept is logged (Q132).
func TestSyncResultLost(t *testing.T) {
	ctx := t.Context()
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	db.Close()
	logs := &logging.Buffer{}
	ag := New(db, nil, nil, nil, nil, slog.New(slog.NewTextHandler(logs, nil)))
	a, _ := pair(t)
	a.Close()
	ag.desired(ctx, a, nil, nil, nil, &agentv1.DesiredState{App: "../x", Op: 1})
	if !strings.Contains(logs.String(), `msg="record the Sync's result"`) {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

// A Diff the Agent can't compute is answered with why, and logged at Warn (Q132).
func TestDiffAnswerFails(t *testing.T) {
	logs := &logging.Buffer{}
	ag := New(nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(logs, nil)))
	a, cp := pair(t)
	ag.answerDiff(t.Context(), a, nil, 5, &agentv1.DiffRequest{DesiredState: &agentv1.DesiredState{App: "../x"}})
	if e := recv(t, cp); e.ReplyTo != 5 || !strings.Contains(e.GetDiffResult().GetError(), "name must be") {
		t.Fatalf("got %v", e)
	}
	if !strings.Contains(logs.String(), `level=WARN msg="diff of a revision not deployed"`) {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

// changingEngine reports one change to app, then its events end.
type changingEngine struct {
	domain.Engine
	app string
}

func (e changingEngine) Watch(_ context.Context, changed func(string)) error {
	changed(e.app)
	return nil
}

// A Self-Heal that fails is logged, to be tried again at the next change (Q132).
func TestSelfHealFailureLogged(t *testing.T) {
	selfHealDelay = 10 * time.Millisecond
	t.Cleanup(func() { selfHealDelay = 3 * time.Second })
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	defer db.Close()
	logs := &logging.Buffer{}
	ag := New(db, nil, nil, nil, changingEngine{app: "web"}, slog.New(slog.NewTextHandler(logs, nil)))
	db.path = filepath.Join(t.TempDir(), "gone", "agent.db") // no lock: the Self-Heal can't start
	ag.Run(t.Context())
	waitLog(t, logs, `level=ERROR msg="self-heal failed; the next change to its containers tries again" app=web`)
}
