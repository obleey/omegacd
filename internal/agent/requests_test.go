package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/wire"
)

// peer is a Control Plane stand-in: answer gets each envelope the Agent sends and returns the reply, nil for none.
func peer(t *testing.T, answer func(*agentv1.Envelope) *agentv1.Envelope) (*wire.Conn, *requests) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := wire.Accept(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			e, err := c.Recv(r.Context())
			if err != nil {
				return
			}
			if a := answer(e); a != nil {
				c.Send(r.Context(), a)
			}
		}
	}))
	t.Cleanup(srv.Close)
	conn, err := wire.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	q := newRequests(conn)
	go func() {
		for {
			e, err := conn.Recv(context.Background())
			if err != nil {
				return
			}
			q.answer(e)
		}
	}()
	return conn, q
}

func ask(q *requests, ctx context.Context) error {
	_, err := q.ask(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_RegistryAuthRequest{RegistryAuthRequest: &agentv1.RegistryAuthRequest{}}})
	return err
}

// The Agent's requests get their answers by reply_to: an Error answers as an error; no answer waits until the
// connection drops or the context ends.
func TestRequests(t *testing.T) {
	_, q := peer(t, func(e *agentv1.Envelope) *agentv1.Envelope {
		return &agentv1.Envelope{ReplyTo: e.Id, Body: &agentv1.Envelope_Error{Error: &agentv1.Error{Message: "not for you"}}}
	})
	if err := ask(q, t.Context()); err == nil || err.Error() != "not for you" {
		t.Fatalf("an Error answer: %v", err)
	}
	if (&requests{}).answer(&agentv1.Envelope{ReplyTo: 9}) {
		t.Fatal("answered a request never asked")
	}

	conn, q := peer(t, func(*agentv1.Envelope) *agentv1.Envelope { return nil })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := ask(q, ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no answer, the context ends: %v", err)
	}
	go func() { time.Sleep(50 * time.Millisecond); conn.Close() }()
	if err := ask(q, t.Context()); err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Fatalf("no answer, the connection drops: %v", err)
	}
	if err := ask(q, t.Context()); err == nil {
		t.Fatal("a request on a closed connection")
	}
}

// Q86: credentials open only with this Host's key; without a key or an answer there are none.
func TestRegistryAuthOnTheAgent(t *testing.T) {
	ctx := t.Context()
	mine, _ := age.GenerateX25519Identity()
	other, _ := age.GenerateX25519Identity()
	sealedFor := func(id *age.X25519Identity) []byte {
		b, err := agents.SealCredential(id.Recipient().String(), domain.RegistryCredential{Username: "u", Password: "p"})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	answering := func(sealed []byte) *requests {
		_, q := peer(t, func(e *agentv1.Envelope) *agentv1.Envelope {
			return &agentv1.Envelope{ReplyTo: e.Id, Body: &agentv1.Envelope_RegistryAuth{RegistryAuth: &agentv1.RegistryAuth{
				Credentials: []*agentv1.SealedCredential{{Registry: "r.example.com", Sealed: sealed}}}}}
		})
		return q
	}
	if creds, err := registryAuth(answering(sealedFor(mine)), mine)(ctx, []string{"r.example.com"}); err != nil ||
		len(creds) != 1 || creds[0].Password != "p" || creds[0].Registry != "r.example.com" {
		t.Fatalf("%+v %v", creds, err)
	}
	if _, err := registryAuth(answering(sealedFor(other)), mine)(ctx, []string{"r.example.com"}); err == nil ||
		!strings.Contains(err.Error(), "don't decrypt with this Host's key") {
		t.Fatalf("sealed to another key: %v", err)
	}
	if creds, err := registryAuth(nil, mine)(ctx, nil); err != nil || creds != nil {
		t.Fatal("asked for no registries")
	}
	if _, err := registryAuth(nil, nil)(ctx, []string{"docker.io"}); err == nil || !strings.Contains(err.Error(), "no Host Key") {
		t.Fatalf("no Host Key: %v", err)
	}
	_, q := peer(t, func(e *agentv1.Envelope) *agentv1.Envelope {
		return &agentv1.Envelope{ReplyTo: e.Id, Body: &agentv1.Envelope_Error{Error: &agentv1.Error{Message: "no"}}}
	})
	if _, err := registryAuth(q, mine)(ctx, []string{"docker.io"}); err == nil {
		t.Fatal("an error answer gave credentials")
	}
}

type removeFails struct{ domain.Engine }

func (removeFails) Remove(context.Context, domain.Application, bool) error {
	return errors.New("volume in use")
}

// Q83 on the Agent: forgetting an app it doesn't have is ErrNotFound; a removal that fails is answered with why and
// not put in the outbox; one whose answer can't be recorded either is logged.
func TestRemovalFailures(t *testing.T) {
	ctx := t.Context()
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	defer db.Close()
	ag := New(db, nil, nil, nil, removeFails{}, nil)
	if err := ag.Forget(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("forget an unknown app: %v", err)
	}
	db.SaveApp(ctx, App{Name: "web"})
	got := make(chan *agentv1.Envelope, 1)
	conn, _ := peer(t, func(e *agentv1.Envelope) *agentv1.Envelope { got <- e; return nil })
	ag.removeApp(ctx, conn, 7, &agentv1.RemoveApp{App: "web", Volumes: true})
	if e := <-got; e.ReplyTo != 7 || e.GetAppRemoved().GetError() != "volume in use" {
		t.Fatalf("answer %v", e)
	}
	if entries, _ := db.Outbox(ctx); len(entries) != 0 {
		t.Fatalf("a failed removal in the outbox: %v", entries)
	}
	// a delete retried after its answer was lost: the app is gone already, which is removed
	ag.removeApp(ctx, conn, 9, &agentv1.RemoveApp{App: "never-here"})
	if e := <-got; e.ReplyTo != 9 || e.GetAppRemoved().GetError() != "" {
		t.Fatalf("removing an app it doesn't have: %v", e)
	}

	conn.Close()
	db.Close() // nowhere to record it either
	ag.removeApp(ctx, conn, 8, &agentv1.RemoveApp{App: "gone", KeepContainers: true})
	if err := ag.recordEntry(ctx, &agentv1.OutboxEntry{}); err == nil {
		t.Fatal("recorded into a closed database")
	}
}
