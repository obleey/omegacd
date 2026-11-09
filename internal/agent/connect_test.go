package agent

import (
	"crypto/ed25519"
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
	"github.com/obleey/omegacd/internal/store"
)

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
	go New(nil, nil, nil, nil, nil, l).Connect(ctx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: "age1web"})
	waitLog(t, &log, "waiting for an admin to approve this Agent")
	if !strings.Contains(log.String(), "fingerprint="+fp) {
		t.Fatalf("the Agent's log lacks its fingerprint %s:\n%s", fp, log.String())
	}
	if h, _ := st.GetHost(ctx, "web"); h.Fingerprint != fp || h.AgeRecipient != "age1web" {
		t.Fatalf("the Control Plane has %+v", h)
	}
	if err := hub.Approve(ctx, "web", fp); err != nil {
		t.Fatal(err)
	}
	waitLog(t, &log, "connected to the Control Plane, approved")

	var other logging.Buffer
	l, _ = logging.New(&other, "info", "text")
	other2, _ := SigningKey(t.TempDir())
	go New(nil, nil, nil, nil, nil, l).Connect(ctx, Connection{URL: srv.URL, Invite: invite, Key: other2, AgeRecipient: "age1other"})
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
