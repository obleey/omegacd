package agents

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/store"
	"github.com/obleey/omegacd/internal/wire"
)

func newHub(t *testing.T) (*Hub, *store.Store, string) {
	t.Helper()
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := New(st, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, st, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// agent is a test Agent: it connects with key, enrolling with invite if not empty, and returns the connection and
// the first answer to its challenge response.
func agent(t *testing.T, url string, key ed25519.PrivateKey, invite string, sign func(nonce []byte) []byte) (*wire.Conn, *agentv1.Envelope) {
	t.Helper()
	ctx := t.Context()
	c, err := wire.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	e, err := c.Recv(ctx)
	if err != nil || e.GetChallenge() == nil {
		t.Fatalf("want a challenge first, got %v, %v", e, err)
	}
	if sign == nil {
		sign = func(nonce []byte) []byte { return ed25519.Sign(key, append([]byte(ChallengePrefix), nonce...)) }
	}
	if invite != "" {
		c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Enroll{Enroll: &agentv1.Enroll{Invite: invite, AgeRecipient: "age1test"}}})
	}
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_ChallengeResponse{ChallengeResponse: &agentv1.ChallengeResponse{
		PublicKey: key.Public().(ed25519.PublicKey), Signature: sign(e.GetChallenge().Nonce)}}})
	e, err = c.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return c, e
}

func newKey() ed25519.PrivateKey { _, k, _ := ed25519.GenerateKey(nil); return k }

func refused(t *testing.T, e *agentv1.Envelope, want string) {
	t.Helper()
	if e.GetError() == nil || !strings.Contains(e.GetError().Message, want) {
		t.Fatalf("got %v, want refused with %q", e, want)
	}
}

// An Agent enrolls with an invite and waits, sent nothing, until an admin approves the fingerprint it printed.
func TestEnrollAndApprove(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	invite, err := st.CreateInvite(ctx, "web", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	key := newKey()
	fp := domain.Fingerprint(key.Public().(ed25519.PublicKey))
	c, e := agent(t, url, key, invite, nil)
	s := e.GetEnrollmentStatus()
	if s == nil || s.Host != "web" || s.Approved || s.Fingerprint != fp {
		t.Fatalf("want waiting for approval with the key's fingerprint %s, got %v", fp, e)
	}
	host, _ := st.GetHost(ctx, "web")
	if host.Fingerprint != fp || host.AgeRecipient != "age1test" {
		t.Fatalf("the Control Plane shows %+v, the Agent %s", host, fp)
	}

	err = h.Send(ctx, "web", &agentv1.Envelope{Body: &agentv1.Envelope_DesiredState{DesiredState: &agentv1.DesiredState{App: "a"}}})
	if !errors.Is(err, ErrNotApproved) {
		t.Fatalf("sent to an unapproved Agent: %v", err)
	}
	if err := h.Approve(ctx, "web", domain.Fingerprint([]byte("another key"))); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("approved another fingerprint: %v", err)
	}
	if err := h.Approve(ctx, "web", fp); err != nil {
		t.Fatal(err)
	}
	e, err = c.Recv(ctx) // the first thing it receives after waiting: its approval
	if err != nil || !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("want approved, got %v, %v", e, err)
	}
	if err := h.Send(ctx, "web", &agentv1.Envelope{Body: &agentv1.Envelope_DesiredState{DesiredState: &agentv1.DesiredState{App: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if e, _ = c.Recv(ctx); e.GetDesiredState().GetApp() != "a" {
		t.Fatalf("got %v", e)
	}

	c.Close() // reconnecting needs no invite, and an approved Agent is told so at once
	_, e = agent(t, url, key, "", nil)
	if !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("reconnected: %v", e)
	}
}

func TestRefused(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	invite, _ := st.CreateInvite(ctx, "web", time.Now().Add(time.Hour))
	agent(t, url, newKey(), invite, nil)

	_, e := agent(t, url, newKey(), invite, nil)
	refused(t, e, "the invite is unknown, used or expired")

	expired, _ := st.CreateInvite(ctx, "db", time.Now().Add(time.Hour))
	h.now = func() time.Time { return time.Now().Add(time.Hour) }
	_, e = agent(t, url, newKey(), expired, nil)
	refused(t, e, "the invite is unknown, used or expired")
	h.now = time.Now

	key := newKey()
	_, e = agent(t, url, key, "", nil)
	refused(t, e, "no Host has the key "+domain.Fingerprint(key.Public().(ed25519.PublicKey)))

	fresh, _ := st.CreateInvite(ctx, "cache", time.Now().Add(time.Hour))
	_, e = agent(t, url, newKey(), fresh, func(nonce []byte) []byte { return ed25519.Sign(newKey(), nonce) })
	refused(t, e, "signature doesn't verify")
	_, e = agent(t, url, key, fresh, func(nonce []byte) []byte { return ed25519.Sign(key, nonce) }) // the nonce alone
	refused(t, e, "signature doesn't verify")
	if host, _ := st.GetHost(ctx, "cache"); host.Fingerprint != "" {
		t.Fatalf("a refused signature enrolled: %+v", host)
	}

	if err := h.Send(ctx, "nowhere", &agentv1.Envelope{}); !errors.Is(err, ErrOffline) {
		t.Fatal(err)
	}
}

// An Agent that never answers the challenge is cut off.
func TestAuthTimeout(t *testing.T) {
	authTimeout = 300 * time.Millisecond
	t.Cleanup(func() { authTimeout = 30 * time.Second })
	_, _, url := newHub(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := wire.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for {
		e, err := c.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("still connected after the auth timeout")
			}
			return
		}
		if e.GetError() != nil {
			return
		}
	}
}
