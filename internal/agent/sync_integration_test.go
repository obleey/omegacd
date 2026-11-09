//go:build integration

package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/store"
)

// desiredState is r as the Control Plane sends it, for op.
func desiredState(a App, r Revision, op uint64) *agentv1.DesiredState {
	ds := &agentv1.DesiredState{App: a.Name, Revision: r.Revision, ComposeFiles: r.ComposeFiles, Op: op, Prune: a.Prune,
		SelfHeal: a.SelfHeal, Checksum: r.Checksum}
	for _, f := range r.Files {
		sum, _ := hex.DecodeString(f.SHA256)
		ds.Files = append(ds.Files, &agentv1.File{Path: f.Path, Mode: uint32(f.Mode), Sha256: sum})
	}
	return ds
}

// M4-01 against a real engine (each of make test-engines'): a Sync through the hub reaches a connected Agent, which
// fetches the files over the WebSocket, decrypts and deploys them; a rollback to a cached Revision transfers nothing;
// with the Agent gone, a Sync is refused as offline, which the controller records as waiting (Q81).
func TestAgentSyncsThroughHub(t *testing.T) {
	ag, src, recipient := testAgent(t)
	ctx := t.Context()
	name := appName(t)
	t.Cleanup(func() { ag.Remove(context.Background(), name, true) })

	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hub := agents.New(st, nil)
	var served atomic.Int32
	hub.Blob = func(_ context.Context, hash string) ([]byte, error) {
		served.Add(1)
		return src.fetch(hash)
	}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	invite, _ := st.CreateInvite(ctx, "edge", time.Now().Add(time.Hour))
	key, _ := SigningKey(t.TempDir())
	actx, stop := context.WithCancel(ctx)
	go ag.Connect(actx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: recipient})
	eventually(t, "the Agent enrolled", func() bool { h, _ := st.GetHost(ctx, "edge"); return h.Fingerprint != "" })
	if err := hub.Approve(ctx, "edge", domain.Fingerprint(key.Public().(ed25519.PublicKey))); err != nil {
		t.Fatal(err)
	}

	a := App{Name: name, Prune: true}
	files := map[string]string{
		"compose.yaml": strings.Replace(compose, "%s", "", 1),
		"conf.txt":     "conf v1",
		".sops.env":    encrypted(t, "SECRET=decrypted-on-the-agent\n", ".sops.env", recipient),
	}
	r1 := src.revision(name, "r1", files)
	var deployErr error
	eventually(t, "the approved Agent took the Sync", func() bool {
		deployErr = hub.Deploy(ctx, "edge", desiredState(a, r1, 1))
		return !errors.Is(deployErr, agents.ErrNotApproved) && !errors.Is(deployErr, agents.ErrOffline)
	})
	if deployErr != nil {
		t.Fatal(deployErr)
	}
	eventually(t, "the first Revision runs, its secret decrypted on the Agent", func() bool {
		l := logs(t, ag, name)
		return strings.Contains(l, "conf v1") && strings.Contains(l, "secret=decrypted-on-the-agent")
	})
	if n := served.Load(); n != 3 {
		t.Fatalf("served %d blobs, want the 3 files", n)
	}

	files["conf.txt"] = "conf v2"
	if err := hub.Deploy(ctx, "edge", desiredState(a, src.revision(name, "r2", files), 2)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second Revision runs", func() bool { return strings.Contains(logs(t, ag, name), "conf v2") })
	if n := served.Load(); n != 4 {
		t.Fatalf("served %d blobs, want only the changed file more", n)
	}

	if err := hub.Deploy(ctx, "edge", desiredState(a, r1, 3)); err != nil { // a rollback
		t.Fatal(err)
	}
	eventually(t, "rolled back to the first Revision", func() bool {
		l := logs(t, ag, name)
		return strings.LastIndex(l, "conf v1") > strings.LastIndex(l, "conf v2")
	})
	if n := served.Load(); n != 4 {
		t.Fatalf("a rollback to a cached Revision transferred %d blobs", n-4)
	}

	bad := map[string]string{"compose.yaml": "services: [not, a, map]\n"}
	if err := hub.Deploy(ctx, "edge", desiredState(a, src.revision(name, "broken", bad), 4)); err == nil {
		t.Fatal("a broken Revision deployed")
	}

	stop() // the Agent goes away
	eventually(t, "the Host offline", func() bool {
		return errors.Is(hub.Deploy(ctx, "edge", desiredState(a, r1, 5)), agents.ErrOffline)
	})
}
