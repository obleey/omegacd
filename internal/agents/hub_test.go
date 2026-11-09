package agents

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
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
