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
	hostKey := ag.secrets.sources[0].(SOPS).HostKey
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
	var progress atomic.Int32
	hub.Progress = func(context.Context, string, domain.ProgressEvent) { progress.Add(1) }
	srv := httptest.NewServer(hub)
	defer srv.Close()
	invite, _ := st.CreateInvite(ctx, "edge", time.Now().Add(time.Hour))
	if err := st.CreateApp(ctx, domain.Application{Name: name, Host: "edge", AutoSync: true}); err != nil {
		t.Fatal(err)
	}
	key, _ := SigningKey(t.TempDir())
	actx, stop := context.WithCancel(ctx)
	go ag.Connect(actx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: recipient, HostKey: hostKey})
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
	if progress.Load() == 0 {
		t.Fatal("no live progress from the Agent's Sync (Q87)")
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

	// M3-06: offline, a rollback on the Host goes to the outbox; on reconnect it arrives, once
	if err := ag.Rollback(ctx, name, "r2"); err != nil {
		t.Fatal(err)
	}
	rollbacks := func() (n int) {
		ops, _ := st.ListOps(ctx, name)
		for _, op := range ops {
			if op.Trigger == "rollback" {
				n++
			}
		}
		return n
	}
	for range 2 { // and a second connection sends nothing again: it was acknowledged
		actx, stop = context.WithCancel(ctx)
		go ag.Connect(actx, Connection{URL: srv.URL, Key: key, AgeRecipient: recipient, HostKey: hostKey})
		eventually(t, "the rollback reported", func() bool { return rollbacks() == 1 })
		eventually(t, "the outbox emptied by the ack", func() bool { e, _ := ag.db.Outbox(ctx); return len(e) == 0 })
		stop()
	}
	if app, _ := st.GetApp(ctx, name); rollbacks() != 1 || !app.RolledBack || app.AutoSync {
		t.Fatalf("rollbacks %d, app rolled back %v, auto-sync %v (Q25)", rollbacks(), app.RolledBack, app.AutoSync)
	}

	// Q83: a delete through the hub removes its containers on the Host
	actx, stop = context.WithCancel(ctx)
	defer stop()
	go ag.Connect(actx, Connection{URL: srv.URL, Key: key, AgeRecipient: recipient, HostKey: hostKey})
	var removeErr error
	eventually(t, "the Agent back for the delete", func() bool {
		removeErr = hub.Remove(ctx, "edge", name, true, false)
		// the hub may still hold the connection just closed: the controller retries those on resync
		return !errors.Is(removeErr, agents.ErrOffline) && !errors.Is(removeErr, agents.ErrDisconnected)
	})
	if removeErr != nil {
		t.Fatal(removeErr)
	}
	if live, err := ag.engine.Status(ctx, domain.Application{Name: name}); err != nil || len(live.Containers) != 0 {
		t.Fatalf("containers after the delete: %+v %v", live.Containers, err)
	}
	if _, err := ag.db.GetApp(ctx, name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the Agent still has it: %v", err)
	}
}

// Q86: an Agent without its Host Key can't open registry credentials: the Sync fails saying so, before pulling.
func TestAgentSyncWithoutHostKey(t *testing.T) {
	ag, src, _ := testAgent(t)
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
	hub.Blob = func(_ context.Context, hash string) ([]byte, error) { return src.fetch(hash) }
	srv := httptest.NewServer(hub)
	defer srv.Close()
	invite, _ := st.CreateInvite(ctx, "edge", time.Now().Add(time.Hour))
	key, _ := SigningKey(t.TempDir())
	go ag.Connect(ctx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: id.Recipient().String()})
	eventually(t, "the Agent enrolled", func() bool { h, _ := st.GetHost(ctx, "edge"); return h.Fingerprint != "" })
	hub.Approve(ctx, "edge", domain.Fingerprint(key.Public().(ed25519.PublicKey)))
	r := src.revision(name, "r1", map[string]string{"compose.yaml": strings.Replace(compose, "%s", "", 1), "conf.txt": "x"})
	var err2 error
	eventually(t, "the Sync answered", func() bool {
		err2 = hub.Deploy(ctx, "edge", desiredState(App{Name: name}, r, 1))
		return !errors.Is(err2, agents.ErrNotApproved) && !errors.Is(err2, agents.ErrOffline)
	})
	if err2 == nil || !strings.Contains(err2.Error(), "registry credentials: no Host Key") {
		t.Fatalf("Deploy = %v", err2)
	}
}
