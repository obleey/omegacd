package agents

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// a session outlives srv.Close (its connection is hijacked) and records the Agent leaving: the store and its
	// folder go only after every session ended
	var sessions sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessions.Add(1)
		defer sessions.Done()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { srv.Close(); sessions.Wait() })
	return h, st, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// agent is a test Agent: it connects with key, enrolling with invite if not empty, and returns the connection and
// the first answer to its challenge response.
func agent(t *testing.T, url string, key ed25519.PrivateKey, invite string, sign func(nonce []byte) []byte) (*wire.Conn, *agentv1.Envelope) {
	t.Helper()
	return agentV(t, url, key, invite, sign, &agentv1.Hello{AgentVersion: "(devel)", ProtocolVersion: 1})
}

// agentV is agent saying hello (nil: none).
func agentV(t *testing.T, url string, key ed25519.PrivateKey, invite string, sign func(nonce []byte) []byte, hello *agentv1.Hello) (*wire.Conn, *agentv1.Envelope) {
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
	if hello != nil {
		c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Hello{Hello: hello}})
	}
	if invite != "" {
		c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Enroll{Enroll: &agentv1.Enroll{Invite: invite, AgeRecipient: testRecipient}}})
	}
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_ChallengeResponse{ChallengeResponse: &agentv1.ChallengeResponse{
		PublicKey: key.Public().(ed25519.PublicKey), Signature: sign(e.GetChallenge().Nonce)}}})
	e, err = c.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return c, e
}

// testRecipient is a real Host Key's recipient: enrollment refuses anything else.
var testRecipient = func() string { id, _ := age.GenerateX25519Identity(); return id.Recipient().String() }()

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
	if s == nil || s.Host != "web" || s.Approved || s.Fingerprint != fp || !s.EnrolledNow {
		t.Fatalf("want just enrolled, waiting for approval with the key's fingerprint %s, got %v", fp, e)
	}
	host, _ := st.GetHost(ctx, "web")
	if host.Fingerprint != fp || host.AgeRecipient != testRecipient {
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
	if !e.GetEnrollmentStatus().GetApproved() || e.GetEnrollmentStatus().GetEnrolledNow() { // Q117: not enrolled again
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

// approvedAgent enrolls a key for host, approved, and returns it.
func approvedAgent(t *testing.T, h *Hub, st *store.Store, url, host string) ed25519.PrivateKey {
	t.Helper()
	invite, err := st.CreateInvite(t.Context(), host, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	key := newKey()
	c, _ := agent(t, url, key, invite, nil)
	if err := h.Approve(t.Context(), host, domain.Fingerprint(key.Public().(ed25519.PublicKey))); err != nil {
		t.Fatal(err)
	}
	c.Close()
	for deadline := time.Now().Add(10 * time.Second); h.session(host) != nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) { // the hub hears the close a moment later: tests that want it gone wait
			t.Fatal("the hub still has the connection")
		}
	}
	return key
}

// reading keeps reading c, as an Agent does (which answers pings), and hands on what arrives.
func reading(c *wire.Conn) <-chan *agentv1.Envelope {
	ch := make(chan *agentv1.Envelope, 16)
	go func() {
		defer close(ch)
		for {
			e, err := c.Recv(context.Background())
			if err != nil {
				return
			}
			ch <- e
		}
	}()
	return ch
}

func next(t *testing.T, ch <-chan *agentv1.Envelope) *agentv1.Envelope {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived")
		return nil
	}
}

// A signature captured from one connection is worth nothing on the next: every challenge is fresh.
func TestReplayedSignatureRefused(t *testing.T) {
	h, st, url := newHub(t)
	key := approvedAgent(t, h, st, url, "web")
	var captured []byte
	c, e := agent(t, url, key, "", func(nonce []byte) []byte {
		captured = ed25519.Sign(key, append([]byte(ChallengePrefix), nonce...))
		return captured
	})
	if !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("got %v", e)
	}
	c.Close()
	_, e = agent(t, url, key, "", func([]byte) []byte { return captured })
	refused(t, e, "signature doesn't verify")
}

// Two live connections with one key lock the Host (Q28, Q75): both are closed with the reason, the Agent is refused
// until an admin unlocks it, and the lock names both connections.
func TestDuplicateConnectionLocks(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	key := approvedAgent(t, h, st, url, "web")
	first, _ := agent(t, url, key, "", nil)
	got := reading(first)

	_, e := agent(t, url, key, "", nil) // a copy of the key, connected while the first still answers
	refused(t, e, "Host web is locked: two connections used its key at once (127.0.0.1 since ")
	if e := next(t, got); e.GetError() == nil || !strings.Contains(e.GetError().Message, "locked") {
		t.Fatalf("the first connection got %v, want the lock", e)
	}
	host, _ := st.GetHost(ctx, "web")
	if len(host.Locked) != 2 || host.Locked[0].Address != "127.0.0.1" || host.Locked[0].Since.After(host.Locked[1].Since) {
		t.Fatalf("locked with %+v", host.Locked)
	}
	_, e = agent(t, url, key, "", nil)
	refused(t, e, "Host web is locked")

	if err := h.Unlock(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, e = agent(t, url, key, "", nil); !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("after unlock: %v", e)
	}
}

// A connection that no longer answers (the Agent's network changed, it restarted) is replaced, not a reason to lock.
func TestDeadConnectionReplaced(t *testing.T) {
	aliveTimeout = 300 * time.Millisecond
	t.Cleanup(func() { aliveTimeout = 5 * time.Second })
	h, st, url := newHub(t)
	key := approvedAgent(t, h, st, url, "web")
	agent(t, url, key, "", nil) // never reads again: never answers a ping
	if _, e := agent(t, url, key, "", nil); !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("got %v", e)
	}
	if host, _ := st.GetHost(t.Context(), "web"); host.Locked != nil {
		t.Fatalf("locked: %+v", host.Locked)
	}
}

// rotateWith answers the Control Plane's RotateKey on c with next, signed by sign, and returns the answer to it.
func rotateWith(t *testing.T, c *wire.Conn, got <-chan *agentv1.Envelope, nextKey, sign ed25519.PrivateKey) *agentv1.Envelope {
	t.Helper()
	if e := next(t, got); e.GetRotateKey() == nil {
		t.Fatalf("want RotateKey, got %v", e)
	}
	pub := nextKey.Public().(ed25519.PublicKey)
	r := &agentv1.Envelope{Body: &agentv1.Envelope_KeyRotation{KeyRotation: &agentv1.KeyRotation{PublicKey: pub,
		Signature: ed25519.Sign(sign, append([]byte(RotatePrefix), pub...))}}}
	c.Send(t.Context(), r)
	e := next(t, got)
	if e.ReplyTo != r.Id {
		t.Fatalf("want the answer to %d, got %v", r.Id, e)
	}
	return e
}

// Rotation without a gap (Q76): the connection stays up, the next one uses the new key; until it does, the old key
// still works, and using it undoes the rotation.
func TestRotation(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	old := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, old, "", nil)
	got := reading(c)

	nextKey := newKey()
	want := domain.Fingerprint(nextKey.Public().(ed25519.PublicKey))
	done := make(chan error, 1)
	var fp string
	go func() { var err error; fp, err = h.Rotate(ctx, "web"); done <- err }()
	if e := rotateWith(t, c, got, nextKey, old); e.GetEnrollmentStatus().GetFingerprint() != want {
		t.Fatalf("answered %v", e)
	}
	if err := <-done; err != nil || fp != want {
		t.Fatalf("Rotate: %s %v", fp, err)
	}
	if err := h.Send(ctx, "web", &agentv1.Envelope{Body: &agentv1.Envelope_DesiredState{DesiredState: &agentv1.DesiredState{App: "a"}}}); err != nil {
		t.Fatalf("the connection is gone after the rotation: %v", err)
	}
	if host, _ := st.GetHost(ctx, "web"); host.Fingerprint != want || !host.Approved {
		t.Fatalf("after the rotation: %+v", host)
	}
	c.Close()
	if _, e := agent(t, url, nextKey, "", nil); !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("the new key: %v", e)
	}
	_, e := agent(t, url, old, "", nil)
	refused(t, e, "no Host has the key")

	// a rotation the Agent never finished (it crashed before switching): it comes back with the old key
	c, _ = agent(t, url, nextKey, "", nil)
	got = reading(c)
	go func() { _, err := h.Rotate(ctx, "web"); done <- err }()
	rotateWith(t, c, got, newKey(), nextKey)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, e := agent(t, url, nextKey, "", nil); !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("the old key after an unfinished rotation: %v", e)
	}
	if host, _ := st.GetHost(ctx, "web"); host.Fingerprint != want {
		t.Fatalf("shows %s, want the key the Agent still has", host.Fingerprint)
	}
}

func TestRotationRefused(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	key := approvedAgent(t, h, st, url, "web")
	if _, err := h.Rotate(ctx, "web"); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline: %v", err)
	}
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	done := make(chan error, 1)
	go func() { _, err := h.Rotate(ctx, "web"); done <- err }()
	if e := rotateWith(t, c, got, newKey(), newKey()); e.GetError() == nil { // signed by a key the Host doesn't have
		t.Fatalf("a new key signed by another was taken: %v", e)
	}
	if err := <-done; err == nil {
		t.Fatal("Rotate succeeded")
	}
	if host, _ := st.GetHost(ctx, "web"); host.Fingerprint != domain.Fingerprint(key.Public().(ed25519.PublicKey)) {
		t.Fatalf("the key changed: %+v", host)
	}

	invite, _ := st.CreateInvite(ctx, "db", time.Now().Add(time.Hour))
	agent(t, url, newKey(), invite, nil)
	if _, err := h.Rotate(ctx, "db"); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("unapproved: %v", err)
	}
}

// Revocation is deleting the Host (Q22): its live connection closes, and its key is refused.
func TestRevocation(t *testing.T) {
	h, st, url := newHub(t)
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	if err := st.DeleteHost(t.Context(), "web"); err != nil {
		t.Fatal(err)
	}
	h.Disconnect("web")
	select {
	case _, ok := <-got:
		if ok {
			t.Fatal("got a message instead of the close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked Agent is still connected")
	}
	_, e := agent(t, url, key, "", nil)
	refused(t, e, "no Host has the key")
}

// Q41: Agents of the Control Plane's minor version and one behind are served; two behind and newer ones are refused
// with what to upgrade. Every Agent's Hello is recorded, a refused one with the reason.
func TestVersionHandshake(t *testing.T) {
	h, st, url := newHub(t)
	h.version = "v0.3.1"
	key := approvedAgent(t, h, st, url, "web")
	for _, tc := range []struct{ version, refused string }{
		{"v0.3.0", ""},
		{"v0.2.7", ""},
		{"v0.1.9", "Agent v0.1.9 is too old for this Control Plane (v0.3.1), which serves Agents from v0.2 on: upgrade the Agent"},
		{"v0.4.0", "Agent v0.4.0 is newer than this Control Plane (v0.3.1): upgrade the Control Plane to v0.4 or later first"},
	} {
		hello := &agentv1.Hello{AgentVersion: tc.version, ProtocolVersion: 1, Hostname: "web-1", Os: "linux", Arch: "arm64",
			Engine: "podman", EngineVersion: "5.4.2", Rootless: true}
		c, e := agentV(t, url, key, "", nil, hello)
		if tc.refused == "" && !e.GetEnrollmentStatus().GetApproved() {
			t.Fatalf("%s: got %v", tc.version, e)
		}
		if tc.refused != "" {
			refused(t, e, tc.refused)
		}
		c.Close()
		host, _ := st.GetHost(t.Context(), "web")
		a := host.Agent
		if a == nil || a.Version != tc.version || a.Hostname != "web-1" || a.OS != "linux" || a.Arch != "arm64" ||
			a.Engine != "podman" || a.EngineVersion != "5.4.2" || !a.Rootless || a.Protocol != 1 || a.SeenAt.IsZero() ||
			!strings.Contains(a.Refused, tc.refused) || (tc.refused == "") != (a.Refused == "") {
			t.Fatalf("%s: recorded %+v", tc.version, a)
		}
	}
	_, e := agentV(t, url, key, "", nil, nil)
	refused(t, e, "no Hello")
}

// Q28: an Agent outside its Host's allowlist is refused even with a valid key; a narrowed list cuts a live connection.
func TestAllowlist(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	key := approvedAgent(t, h, st, url, "web")

	if _, err := h.SetAllowlist(ctx, "web", []string{"10.9.0.0/16", "not an address"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a bad entry: %v", err)
	}
	list, err := h.SetAllowlist(ctx, "web", []string{" 10.9.3.4/16 ", "2001:db8::1"})
	if err != nil || strings.Join(list, ",") != "10.9.0.0/16,2001:db8::1/128" {
		t.Fatalf("normalised: %v %v", list, err)
	}
	_, e := agent(t, url, key, "", nil)
	refused(t, e, "Host web's Agent may connect only from 10.9.0.0/16, 2001:db8::1/128, not from 127.0.0.1")

	if _, err := h.SetAllowlist(ctx, "web", []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	c, e := agent(t, url, key, "", nil)
	if !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("inside the list: %v", e)
	}
	got := reading(c)
	if _, err := h.SetAllowlist(ctx, "web", []string{"10.9.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	if e := next(t, got); !strings.Contains(e.GetError().GetMessage(), "may connect only from 10.9.0.0/16") {
		t.Fatalf("the live connection outside the new list got %v", e)
	}
	if list, err := h.SetAllowlist(ctx, "web", nil); err != nil || len(list) != 0 {
		t.Fatalf("cleared: %v %v", list, err)
	}
	if _, e := agent(t, url, key, "", nil); !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("anywhere again: %v", e)
	}
	if host, _ := st.GetHost(ctx, "web"); len(host.AllowIPs) != 0 {
		t.Fatalf("stored %v", host.AllowIPs)
	}
}

// Deploy sends the Desired State, serves the blobs it lists (and no other), and returns the Agent's report.
func TestDeploy(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	files := map[string][]byte{"compose.yaml": []byte("services: {}\n"), "secret.sops.env": []byte("ENC[...]")}
	sums := map[string][]byte{}
	for name, b := range files {
		sum := sha256.Sum256(b)
		sums[name] = sum[:]
	}
	h.Blob = func(_ context.Context, hash string) ([]byte, error) {
		for name, sum := range sums {
			if hex.EncodeToString(sum) == hash {
				return files[name], nil
			}
		}
		return nil, domain.ErrNotFound
	}
	if err := h.Deploy(ctx, "web", &agentv1.DesiredState{App: "a"}); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline: %v", err)
	}
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)

	ds := &agentv1.DesiredState{App: "a", Revision: "r1", Op: 7, Files: []*agentv1.File{
		{Path: "compose.yaml", Sha256: sums["compose.yaml"]}, {Path: "secret.sops.env", Sha256: sums["secret.sops.env"]}}}
	done := make(chan error, 1)
	go func() { done <- h.Deploy(ctx, "web", ds) }()
	e := next(t, got)
	if e.GetDesiredState().GetOp() != 7 || len(e.GetDesiredState().GetFiles()) != 2 {
		t.Fatalf("want the Desired State, got %v", e)
	}
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_BlobRequest{BlobRequest: &agentv1.BlobRequest{Sha256: [][]byte{sums["secret.sops.env"]}}}})
	if e := next(t, got); !bytes.Equal(e.GetBlobChunk().GetData(), files["secret.sops.env"]) {
		t.Fatalf("want the blob as rendered (still encrypted), got %v", e)
	}
	other := sha256.Sum256([]byte("another Host's file"))
	req := &agentv1.Envelope{Body: &agentv1.Envelope_BlobRequest{BlobRequest: &agentv1.BlobRequest{Sha256: [][]byte{other[:]}}}}
	c.Send(ctx, req)
	if e := next(t, got); e.ReplyTo != req.Id || !strings.Contains(e.GetError().GetMessage(), "not a file of a Desired State sent to this Agent") {
		t.Fatalf("a blob outside its Desired States: %v", e)
	}
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_SyncOperationReport{SyncOperationReport: &agentv1.SyncOperationReport{
		App: "a", Op: 7, Phase: "Failed", Message: "image not found"}}})
	if err := <-done; err == nil || err.Error() != "image not found" {
		t.Fatalf("Deploy = %v, want the Agent's failure", err)
	}

	go func() { done <- h.Deploy(ctx, "web", &agentv1.DesiredState{App: "a", Op: 8}) }()
	next(t, got)
	c.Close() // the connection drops before the report
	if err := <-done; !errors.Is(err, ErrDisconnected) {
		t.Fatalf("dropped mid-Sync: %v", err)
	}
}

// M3-06: an outbox entry is applied once however often it arrives, each arrival acknowledged.
func TestOutboxExactlyOnce(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	var reported atomic.Int32
	h.Reported = func(context.Context, string) { reported.Add(1) }
	key := approvedAgent(t, h, st, url, "web")
	if err := st.CreateApp(ctx, domain.Application{Name: "app", Host: "web"}); err != nil {
		t.Fatal(err)
	}
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	entry := &agentv1.OutboxEntry{Seq: 1, TimeUnix: time.Now().Unix(), Event: &agentv1.OutboxEntry_SyncOperationReport{
		SyncOperationReport: &agentv1.SyncOperationReport{App: "app", Revision: "r1", Trigger: "self-heal", Phase: "Succeeded"}}}
	for range 3 { // a lost ack: the Agent sends it again on its next connection
		c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_OutboxEntry{OutboxEntry: entry}})
		if e := next(t, got); e.GetOutboxAck().GetSeq() != 1 {
			t.Fatalf("want the ack, got %v", e)
		}
	}
	if ops, _ := st.ListOps(ctx, "app"); len(ops) != 1 || ops[0].Trigger != "self-heal" || reported.Load() != 1 {
		t.Fatalf("applied %d times, reported %d", len(ops), reported.Load())
	}
}

// M3-06: a Desired State lost on the way is sent again at the Agent's next resync, and the controller hears of it.
func TestResyncHealsALostSignal(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	resynced := make(chan string, 4)
	h.Resynced = func(_ context.Context, host string, apps map[string]string, active []uint64) {
		slices.Sort(active)
		resynced <- fmt.Sprintf("%s %v %v", host, slices.Sorted(maps.Keys(apps)), active)
	}
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	sum := sha256.Sum256([]byte("v2"))
	ds := &agentv1.DesiredState{App: "app", Op: 9, Checksum: hex.EncodeToString(sum[:])}
	done := make(chan error, 1)
	go func() { done <- h.Deploy(ctx, "web", ds) }()
	next(t, got) // the Agent never sees this one: lost on the way

	old := sha256.Sum256([]byte("v1"))
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Resync{Resync: &agentv1.Resync{
		Checksums: map[string][]byte{"app": old[:], "other": old[:]}, Deploying: []uint64{4}}}})
	if e := next(t, got); e.GetDesiredState().GetOp() != 9 {
		t.Fatalf("want the lost Desired State again, got %v", e)
	}
	// Q116, Q118: its Applications, and the Sync Operations still going: deploying on it, or sent and not reported
	if r := <-resynced; r != "web [app other] [4 9]" {
		t.Fatal(r)
	}
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_SyncOperationReport{SyncOperationReport: &agentv1.SyncOperationReport{
		App: "app", Op: 9, Phase: "Succeeded"}}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Resync{Resync: &agentv1.Resync{Checksums: map[string][]byte{"app": sum[:]}}}})
	<-resynced
	select {
	case e := <-got:
		t.Fatalf("an Agent that has it got it again: %v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

// Q83: Remove asks the Agent and waits for its answer; offline there's no one to ask.
func TestRemove(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	if err := h.Remove(ctx, "web", "app", true, false); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline: %v", err)
	}
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	done := make(chan error, 1)
	go func() { done <- h.Remove(ctx, "web", "app", true, false) }()
	e := next(t, got)
	if r := e.GetRemoveApp(); r.GetApp() != "app" || !r.Volumes || r.KeepContainers {
		t.Fatalf("got %v", e)
	}
	c.Send(ctx, &agentv1.Envelope{ReplyTo: e.Id, Body: &agentv1.Envelope_AppRemoved{AppRemoved: &agentv1.AppRemoved{App: "app", Error: "volume in use"}}})
	if err := <-done; err == nil || err.Error() != "volume in use" {
		t.Fatalf("Remove = %v, want the Agent's error", err)
	}
}

// Q85/Q86: an Agent gets the credentials of the registries it asks for, sealed to its Host Key: only that key opens
// them, and a Host with no Application gets none.
func TestRegistryAuth(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	hostKey, _ := age.GenerateX25519Identity()
	invite, _ := st.CreateInvite(ctx, "web", time.Now().Add(time.Hour))
	key := newKey()
	// enroll with this test's Host Key, so the test can open what is sealed to it
	cc, err := wire.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := cc.Recv(ctx)
	cc.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Hello{Hello: &agentv1.Hello{AgentVersion: "(devel)"}}})
	cc.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Enroll{Enroll: &agentv1.Enroll{Invite: invite, AgeRecipient: hostKey.Recipient().String()}}})
	cc.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_ChallengeResponse{ChallengeResponse: &agentv1.ChallengeResponse{
		PublicKey: key.Public().(ed25519.PublicKey), Signature: ed25519.Sign(key, append([]byte(ChallengePrefix), ch.GetChallenge().Nonce...))}}})
	cc.Recv(ctx)
	cc.Close()
	h.Approve(ctx, "web", domain.Fingerprint(key.Public().(ed25519.PublicKey)))
	// made up per run: no password, real or not, is ever written into the repository
	pull, hub := rand.Text(), rand.Text()
	for _, r := range []domain.RegistryCredential{
		{Registry: "registry.example.com:5000", Username: "deploy", Password: pull},
		{Registry: "https://index.docker.io/v1/", Username: "hub", Password: hub},
		{Registry: "ghcr.io", Username: "other", Password: rand.Text()},
	} {
		if _, err := st.SaveRegistryCredential(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	ask := func() *agentv1.Envelope {
		c, _ := agent(t, url, key, "", nil)
		got := reading(c)
		req := &agentv1.Envelope{Body: &agentv1.Envelope_RegistryAuthRequest{RegistryAuthRequest: &agentv1.RegistryAuthRequest{
			Registries: []string{"docker.io", "registry.example.com:5000"}}}}
		c.Send(ctx, req)
		e := next(t, got)
		if e.ReplyTo != req.Id {
			t.Fatalf("want the answer, got %v", e)
		}
		c.Close() // gone before the next ask, or two live connections with one key lock the Host
		for h.session("web") != nil {
			time.Sleep(10 * time.Millisecond)
		}
		return e
	}
	if e := ask(); len(e.GetRegistryAuth().GetCredentials()) != 0 {
		t.Fatalf("a Host without Applications got credentials: %v", e)
	}
	st.CreateApp(ctx, domain.Application{Name: "app", Host: "web"})
	creds := ask().GetRegistryAuth().GetCredentials()
	if len(creds) != 2 {
		t.Fatalf("want the two asked for, got %v", creds)
	}
	other, _ := age.GenerateX25519Identity()
	for _, sc := range creds {
		if bytes.Contains(sc.Sealed, []byte(pull)) || bytes.Contains(sc.Sealed, []byte(hub)) {
			t.Fatal("a password in plaintext on the wire")
		}
		if _, err := OpenCredential(other, sc.Registry, sc.Sealed); err == nil {
			t.Fatal("another key opened it")
		}
		cred, err := OpenCredential(hostKey, sc.Registry, sc.Sealed)
		if err != nil || (cred.Password != pull && cred.Password != hub) {
			t.Fatalf("opened %+v, %v", cred, err)
		}
	}
}

// Q87 and Q83 through the outbox: progress goes to the controller; a removal reported later applies once.
func TestProgressAndRemovalFromOutbox(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	progress := make(chan domain.ProgressEvent, 1)
	h.Progress = func(_ context.Context, app string, p domain.ProgressEvent) { progress <- p }
	var removed atomic.Int32
	h.Removed = func(context.Context, string, string) { removed.Add(1) }
	key := approvedAgent(t, h, st, url, "web")
	st.CreateApp(ctx, domain.Application{Name: "app", Host: "web"})
	st.SetAppDeleting(ctx, "app", "volumes")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_SyncProgress{SyncProgress: &agentv1.SyncProgress{App: "app", Op: 4, Resource: "Image busybox", Status: "Pulling"}}})
	if p := <-progress; p.OpID != 4 || p.Status != "Pulling" {
		t.Fatalf("progress %+v", p)
	}
	entry := &agentv1.OutboxEntry{Seq: 1, Event: &agentv1.OutboxEntry_AppRemoved{AppRemoved: &agentv1.AppRemoved{App: "app"}}}
	for range 2 {
		c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_OutboxEntry{OutboxEntry: entry}})
		if e := next(t, got); e.GetOutboxAck().GetSeq() != 1 {
			t.Fatalf("want the ack, got %v", e)
		}
	}
	if _, err := st.GetApp(ctx, "app"); !errors.Is(err, domain.ErrNotFound) || removed.Load() != 1 {
		t.Fatalf("removal applied: %v, told %d times", err, removed.Load())
	}
}

// Enrollment refuses a Host Key that isn't an age recipient: everything later encrypted to it would fail.
func TestEnrollRefusesABadHostKey(t *testing.T) {
	_, st, url := newHub(t)
	ctx := t.Context()
	invite, _ := st.CreateInvite(ctx, "web", time.Now().Add(time.Hour))
	c, err := wire.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	key := newKey()
	ch, _ := c.Recv(ctx)
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Hello{Hello: &agentv1.Hello{AgentVersion: "(devel)"}}})
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Enroll{Enroll: &agentv1.Enroll{Invite: invite, AgeRecipient: "age1-not-a-key"}}})
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_ChallengeResponse{ChallengeResponse: &agentv1.ChallengeResponse{
		PublicKey: key.Public().(ed25519.PublicKey), Signature: ed25519.Sign(key, append([]byte(ChallengePrefix), ch.GetChallenge().Nonce...))}}})
	e, _ := c.Recv(ctx)
	refused(t, e, `the Host Key "age1-not-a-key" isn't an age recipient`)
	if h, _ := st.GetHost(ctx, "web"); h.Fingerprint != "" {
		t.Fatalf("enrolled with it: %+v", h)
	}
}

func TestCredentialFormat(t *testing.T) {
	if _, err := SealCredential("not-a-recipient", domain.RegistryCredential{}); err == nil {
		t.Fatal("sealed to no key")
	}
	id, _ := age.GenerateX25519Identity()
	var buf bytes.Buffer
	w, _ := age.Encrypt(&buf, id.Recipient())
	w.Write([]byte("not json"))
	w.Close()
	if _, err := OpenCredential(id, "r.example.com", buf.Bytes()); err == nil || !strings.Contains(err.Error(), "registry r.example.com") {
		t.Fatalf("opened garbage: %v", err)
	}
}

// Remove on a connection that drops, or a context that ends, before the Agent answers.
func TestRemoveUnanswered(t *testing.T) {
	h, st, url := newHub(t)
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := h.Remove(ctx, "web", "app", false, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no answer: %v", err)
	}
	next(t, got)
	done := make(chan error, 1)
	go func() { done <- h.Remove(t.Context(), "web", "app", false, false) }()
	next(t, got)
	c.Close()
	if err := <-done; !errors.Is(err, ErrDisconnected) {
		t.Fatalf("dropped: %v", err)
	}
}

// A removal from the outbox of a Host deleted meanwhile isn't acknowledged: nothing applied it.
func TestOutboxRemovalOfAGoneHost(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	st.DeleteHost(ctx, "web")
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_OutboxEntry{OutboxEntry: &agentv1.OutboxEntry{Seq: 1,
		Event: &agentv1.OutboxEntry_AppRemoved{AppRemoved: &agentv1.AppRemoved{App: "app"}}}}})
	c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_OutboxEntry{OutboxEntry: &agentv1.OutboxEntry{Seq: 2,
		Event: &agentv1.OutboxEntry_SyncOperationReport{SyncOperationReport: &agentv1.SyncOperationReport{App: "app"}}}}})
	select {
	case e := <-got:
		t.Fatalf("acknowledged: %v", e)
	case <-time.After(300 * time.Millisecond):
	}
}

// Remove on a connection that closed but is still registered (the hub hears the close a moment later): the send
// fails, and the delete reads as disconnected, which the controller retries on resync.
func TestRemoveOnAClosingConnection(t *testing.T) {
	h, _, url := newHub(t)
	c, err := wire.Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	approved := make(chan struct{})
	close(approved)
	h.mu.Lock()
	h.conns["web"] = &session{conn: c, approved: approved, removes: map[uint64]chan *agentv1.AppRemoved{},
		logs: map[uint64]chan *agentv1.LogChunk{}, diffs: map[uint64]chan *agentv1.DiffResult{}, files: map[string]bool{}}
	h.mu.Unlock()
	if err := h.Remove(t.Context(), "web", "app", false, false); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Remove = %v", err)
	}
	if err := h.Logs(t.Context(), "web", "app", "", false, 0, io.Discard); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Logs = %v", err)
	}
	if _, err := h.Diff(t.Context(), "web", &agentv1.DesiredState{}); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Diff = %v", err)
	}
}

// M4-02: an Agent's status reports are kept for its own Host's apps and the controller is told; when it leaves, the
// Host records the time.
func TestStatusReports(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	told := make(chan string, 4)
	h.Status = func(_ context.Context, app string) { told <- app }
	key := approvedAgent(t, h, st, url, "web")
	if h.Online("web") {
		t.Fatal("online with no connection")
	}
	st.CreateApp(ctx, domain.Application{Name: "app", Host: "web"})
	st.CreateApp(ctx, domain.Application{Name: "elsewhere", Host: "other"})
	c, _ := agent(t, url, key, "", nil)
	if !h.Online("web") {
		t.Fatal("not online while connected")
	}
	live, _ := json.Marshal(domain.LiveState{Health: domain.Healthy, Containers: []domain.Container{{Name: "app-1"}}})
	send := func(r *agentv1.StatusReport) {
		c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_StatusReport{StatusReport: r}})
	}
	send(&agentv1.StatusReport{App: "elsewhere", Health: "Healthy", LiveState: live})          // not its Host's
	send(&agentv1.StatusReport{App: "app", Health: "Healthy", LiveState: []byte("{not json")}) // broken
	send(&agentv1.StatusReport{App: "app", Revision: "r1", Health: "Healthy", Drifted: true, LiveState: live,
		Drift: []byte(`[{"service":"app","change":"changed"}]`), Details: []byte(`{"volume/data":{"driver":"local"}}`), TimeUnix: 1794200000})
	if app := <-told; app != "app" {
		t.Fatalf("told about %s", app)
	}
	got, err := st.AgentStatus(ctx, "app")
	if err != nil || got.Revision != "r1" || got.Health != domain.Healthy || !got.Drifted || got.Live.Containers[0].Name != "app-1" ||
		got.Drift[0].Service != "app" || got.Details["volume/data"]["driver"] != "local" || got.At.Unix() != 1794200000 {
		t.Fatalf("kept %+v %v", got, err)
	}
	if _, err := st.AgentStatus(ctx, "elsewhere"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("kept another Host's app's report")
	}
	select {
	case app := <-told:
		t.Fatalf("told about %s too", app)
	default:
	}

	c.Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if hst, _ := st.GetHost(ctx, "web"); hst.Agent != nil && !hst.Agent.LeftAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no time recorded for the Agent leaving")
		}
	}
	if h.Online("web") {
		t.Fatal("online after leaving")
	}
}

// failWriter is a viewer that went away.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("viewer gone") }

// M4-02: logs stream from the Agent: chunks until done, its error; a viewer that leaves or a context that ends stops
// the Agent (LogsStop); a connection that drops ends them.
func TestLogs(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	var out bytes.Buffer
	if err := h.Logs(ctx, "web", "app", "", false, 10, &out); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline: %v", err)
	}
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	chunk := func(id uint64, ch *agentv1.LogChunk) {
		c.Send(ctx, &agentv1.Envelope{ReplyTo: id, Body: &agentv1.Envelope_LogChunk{LogChunk: ch}})
	}
	done := make(chan error, 1)
	go func() { done <- h.Logs(ctx, "web", "app", "app-1", false, 10, &out) }()
	e := next(t, got)
	if r := e.GetLogsRequest(); r.GetApp() != "app" || r.Service != "app-1" || r.Tail != 10 || r.Follow {
		t.Fatalf("request %v", e)
	}
	chunk(e.Id, &agentv1.LogChunk{Data: []byte("one\n")})
	chunk(e.Id, &agentv1.LogChunk{Data: []byte("two\n")})
	chunk(e.Id, &agentv1.LogChunk{Done: true})
	if err := <-done; err != nil || out.String() != "one\ntwo\n" {
		t.Fatalf("logs %q %v", out.String(), err)
	}
	chunk(999, &agentv1.LogChunk{Data: []byte("for no one")}) // a request that ended: dropped

	go func() { done <- h.Logs(ctx, "web", "app", "nope", false, 0, &out) }()
	e = next(t, got)
	chunk(e.Id, &agentv1.LogChunk{Done: true, Error: "no container for nope"})
	if err := <-done; err == nil || err.Error() != "no container for nope" {
		t.Fatalf("the Agent's error: %v", err)
	}

	go func() { done <- h.Logs(ctx, "web", "app", "", true, 0, failWriter{}) }()
	e = next(t, got)
	chunk(e.Id, &agentv1.LogChunk{Data: []byte("x")})
	if stop := next(t, got); stop.GetLogsStop() == nil || stop.ReplyTo != e.Id {
		t.Fatalf("want LogsStop for %d, got %v", e.Id, stop)
	}
	if err := <-done; err == nil {
		t.Fatal("the viewer left: no error")
	}

	lctx, cancel := context.WithCancel(ctx)
	go func() { done <- h.Logs(lctx, "web", "app", "", true, 0, &out) }()
	e = next(t, got)
	cancel()
	if stop := next(t, got); stop.GetLogsStop() == nil || stop.ReplyTo != e.Id {
		t.Fatalf("want LogsStop, got %v", stop)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	go func() { done <- h.Logs(ctx, "web", "app", "", true, 0, &out) }()
	next(t, got)
	c.Close()
	if err := <-done; !errors.Is(err, ErrDisconnected) {
		t.Fatalf("dropped: %v", err)
	}
}

// M4-11: Diff asks the Agent with the Desired State (its files then fetchable) and returns its answer, its error, or
// why there is none.
func TestDiff(t *testing.T) {
	h, st, url := newHub(t)
	ctx := t.Context()
	ds := &agentv1.DesiredState{App: "app", Revision: "r2", Files: []*agentv1.File{{Path: "compose.yaml", Sha256: bytes.Repeat([]byte{7}, 32)}}}
	if _, err := h.Diff(ctx, "web", ds); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline: %v", err)
	}
	h.Blob = func(context.Context, string) ([]byte, error) { return []byte("services: {}\n"), nil }
	key := approvedAgent(t, h, st, url, "web")
	c, _ := agent(t, url, key, "", nil)
	got := reading(c)
	answer := func(res *agentv1.DiffResult) ([]domain.ServiceDiff, error) {
		done := make(chan struct{})
		var out []domain.ServiceDiff
		var err error
		go func() { out, err = h.Diff(ctx, "web", ds); close(done) }()
		e := next(t, got)
		if e.GetDiffRequest().GetDesiredState().GetRevision() != "r2" {
			t.Fatalf("want the DiffRequest, got %v", e)
		}
		// its files are this Agent's to fetch now
		req := &agentv1.Envelope{Body: &agentv1.Envelope_BlobRequest{BlobRequest: &agentv1.BlobRequest{Sha256: [][]byte{ds.Files[0].Sha256}}}}
		c.Send(ctx, req)
		if b := next(t, got); b.GetBlobChunk() == nil {
			t.Fatalf("its file wasn't served: %v", b)
		}
		c.Send(ctx, &agentv1.Envelope{ReplyTo: e.Id, Body: &agentv1.Envelope_DiffResult{DiffResult: res}})
		<-done
		return out, err
	}
	if out, err := answer(&agentv1.DiffResult{Services: []byte(`[{"service":"web","change":"changed"}]`)}); err != nil || len(out) != 1 || out[0].Service != "web" {
		t.Fatalf("answer %+v %v", out, err)
	}
	if _, err := answer(&agentv1.DiffResult{Error: "compose file not found"}); err == nil || err.Error() != "compose file not found" {
		t.Fatalf("the Agent's error: %v", err)
	}
	if _, err := answer(&agentv1.DiffResult{Services: []byte("{not json")}); err == nil || !strings.Contains(err.Error(), "the Agent's diff") {
		t.Fatalf("a broken answer: %v", err)
	}
	c.Send(ctx, &agentv1.Envelope{ReplyTo: 999, Body: &agentv1.Envelope_DiffResult{DiffResult: &agentv1.DiffResult{}}}) // asked by no one: dropped

	dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := h.Diff(dctx, "web", ds); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no answer: %v", err)
	}
	next(t, got)
	done := make(chan error, 1)
	go func() { _, err := h.Diff(ctx, "web", ds); done <- err }()
	next(t, got)
	c.Close()
	if err := <-done; !errors.Is(err, ErrDisconnected) {
		t.Fatalf("dropped: %v", err)
	}
}
