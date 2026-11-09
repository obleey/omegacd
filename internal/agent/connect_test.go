package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/store"
	"github.com/obleey/omegacd/internal/wire"
)

// testRecipient is a real Host Key's recipient: enrollment refuses anything else.
var testRecipient = func() string { id, _ := age.GenerateX25519Identity(); return id.Recipient().String() }()

func TestSigningKey(t *testing.T) {
	dir := t.TempDir()
	k1, err := SigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "signing.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("signing.key: %v %v, want owner-only", fi, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "signing.key"))
	if !strings.Contains(string(b), "# fingerprint: "+domain.Fingerprint(k1.Public().(ed25519.PublicKey))) {
		t.Fatalf("the file doesn't say its fingerprint:\n%s", b)
	}
	k2, err := SigningKey(dir)
	if err != nil || !k1.Equal(k2) {
		t.Fatalf("not kept across starts: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "signing.key"), []byte("# nothing\nnot base64\n"), 0o600)
	if _, err := SigningKey(dir); err == nil || !strings.Contains(err.Error(), "not an omegacd signing key") {
		t.Fatalf("a broken key file: %v", err)
	}
}

// The Agent enrolls with its invite, logs that it waits with the fingerprint the Control Plane shows, and logs its
// approval; later connections need no invite. A used invite is refused with the reason in the Agent's log.
func TestConnectEnrolls(t *testing.T) {
	ctx := t.Context()
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hub := agents.New(st, nil)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	invite, _ := st.CreateInvite(ctx, "web", time.Now().Add(time.Hour))

	key, _ := SigningKey(t.TempDir())
	fp := domain.Fingerprint(key.Public().(ed25519.PublicKey))
	var log logging.Buffer
	l, _ := logging.New(&log, "info", "text")
	go New(nil, nil, nil, nil, nil, l).Connect(ctx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: testRecipient})
	waitLog(t, &log, "waiting for an admin to approve this Agent")
	if !strings.Contains(log.String(), "fingerprint="+fp) {
		t.Fatalf("the Agent's log lacks its fingerprint %s:\n%s", fp, log.String())
	}
	if h, _ := st.GetHost(ctx, "web"); h.Fingerprint != fp || h.AgeRecipient != testRecipient {
		t.Fatalf("the Control Plane has %+v", h)
	}
	if err := hub.Approve(ctx, "web", fp); err != nil {
		t.Fatal(err)
	}
	waitLog(t, &log, "connected to the Control Plane, approved")

	var other logging.Buffer
	l, _ = logging.New(&other, "info", "text")
	other2, _ := SigningKey(t.TempDir())
	go New(nil, nil, nil, nil, nil, l).Connect(ctx, Connection{URL: srv.URL, Invite: invite, Key: other2, AgeRecipient: testRecipient})
	waitLog(t, &other, "the Control Plane refused this Agent: invalid: the invite is unknown, used or expired")
}

func waitLog(t *testing.T, log *logging.Buffer, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(log.String(), want); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never said %q:\n%s", want, log.String())
		}
	}
}

// The Agent rotates its key when asked, without a gap: it keeps its connection, writes the new key over the old one
// owner-only once the Control Plane has it, and connects with it next time.
func TestConnectRotates(t *testing.T) {
	ctx := t.Context()
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hub := agents.New(st, nil)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	invite, _ := st.CreateInvite(ctx, "web", time.Now().Add(time.Hour))
	dir := t.TempDir()
	key, _ := SigningKey(dir)
	var log logging.Buffer
	l, _ := logging.New(&log, "info", "text")
	go New(nil, nil, nil, nil, nil, l).Connect(ctx, Connection{URL: srv.URL, Invite: invite, Key: key,
		KeyPath: filepath.Join(dir, "signing.key"), AgeRecipient: testRecipient})
	waitLog(t, &log, "waiting for an admin")
	hub.Approve(ctx, "web", domain.Fingerprint(key.Public().(ed25519.PublicKey)))
	waitLog(t, &log, "approved")

	fp, err := hub.Rotate(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	waitLog(t, &log, "signing key rotated")
	now, err := SigningKey(dir)
	if err != nil || domain.Fingerprint(now.Public().(ed25519.PublicKey)) != fp {
		t.Fatalf("the key file holds another key than %s: %v", fp, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "signing.key.next")); err == nil {
		t.Fatal("signing.key.next left behind")
	}
	if strings.Count(log.String(), "connected to the Control Plane, approved") != 1 {
		t.Fatalf("the connection didn't stay up:\n%s", log.String())
	}
}

// A key readable by others is refused (Q77), and a rotation left unfinished by a crash is cleaned up at the start.
func TestSigningKeyOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if _, err := SigningKey(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "signing.key.next"), []byte("half a rotation"), 0o600)
	if _, err := SigningKey(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "signing.key.next")); err == nil {
		t.Fatal("an unfinished rotation's key is still there")
	}
	os.Chmod(filepath.Join(dir, "signing.key"), 0o644)
	if _, err := SigningKey(dir); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a key others can read: %v", err)
	}
}

// Q95: a Control Plane with a private certificate is trusted only through OMEGACD_AGENT_CA_FILE.
func TestPrivateCA(t *testing.T) {
	ctx := t.Context()
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewTLSServer(agents.New(st, nil))
	defer srv.Close()
	invite, _ := st.CreateInvite(ctx, "web", time.Now().Add(time.Hour))
	key, _ := SigningKey(t.TempDir())

	connect := func(client *http.Client) *logging.Buffer {
		var log logging.Buffer
		l, _ := logging.New(&log, "info", "text")
		cctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		go New(nil, nil, nil, nil, nil, l).Connect(cctx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: testRecipient, HTTPClient: client})
		return &log
	}
	waitLog(t, connect(nil), "certificate signed by unknown authority")

	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
	client, err := TrustCA(ca)
	if err != nil {
		t.Fatal(err)
	}
	waitLog(t, connect(client), "waiting for an admin to approve")

	os.WriteFile(ca, []byte("not a certificate"), 0o644)
	if _, err := TrustCA(ca); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("a file without certificates: %v", err)
	}
}

// Q83: a removal whose answer can't be sent (the connection dropped) waits in the outbox, to be reported on reconnect.
func TestRemovalToOutbox(t *testing.T) {
	ctx := t.Context()
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	defer db.Close()
	if err := db.SaveApp(ctx, App{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := wire.Accept(w, r); err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	conn, err := wire.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	ag := New(db, nil, nil, nil, nil, nil)
	ag.removeApp(ctx, conn, 1, &agentv1.RemoveApp{App: "web", KeepContainers: true})

	if _, err := db.GetApp(ctx, "web"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("not forgotten: %v", err)
	}
	entries, err := db.Outbox(ctx)
	if err != nil || len(entries) != 1 || entries[0].GetAppRemoved().GetApp() != "web" {
		t.Fatalf("outbox %v, %v", entries, err)
	}
	select {
	case <-ag.recorded:
	default:
		t.Fatal("a live connection wasn't told to send it")
	}
}
