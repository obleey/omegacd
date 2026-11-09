//go:build integration

package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/moby/moby/client"

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

// M4-02 against a real engine: after a Sync the Control Plane holds the Agent's report (containers, Health, the
// decrypted file marked secret, the volume's details); logs stream from the Agent, following until stopped; a
// container stopped by hand shows as drift; the Agent leaving records the time.
func TestAgentReportsThroughHub(t *testing.T) {
	reportDelay = 100 * time.Millisecond
	t.Cleanup(func() { reportDelay = 500 * time.Millisecond })
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
	hub.Blob = func(_ context.Context, hash string) ([]byte, error) { return src.fetch(hash) }
	srv := httptest.NewServer(hub)
	defer srv.Close()
	st.CreateApp(ctx, domain.Application{Name: name, Host: "edge"})
	invite, _ := st.CreateInvite(ctx, "edge", time.Now().Add(time.Hour))
	key, _ := SigningKey(t.TempDir())
	actx, stop := context.WithCancel(ctx)
	defer stop()
	go ag.Run(actx) // its engine watch reports changes
	go ag.Connect(actx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: recipient, HostKey: hostKey})
	eventually(t, "the Agent enrolled", func() bool { h, _ := st.GetHost(ctx, "edge"); return h.Fingerprint != "" })
	hub.Approve(ctx, "edge", domain.Fingerprint(key.Public().(ed25519.PublicKey)))

	files := map[string]string{
		"compose.yaml": strings.Replace(compose, "%s", "", 1),
		"conf.txt":     "conf v1",
		".sops.env":    encrypted(t, "SECRET=decrypted-on-the-agent\n", ".sops.env", recipient),
	}
	r1 := src.revision(name, "r1", files)
	var err2 error
	eventually(t, "the Sync", func() bool {
		err2 = hub.Deploy(ctx, "edge", desiredState(App{Name: name}, r1, 1))
		return !errors.Is(err2, agents.ErrNotApproved) && !errors.Is(err2, agents.ErrOffline)
	})
	if err2 != nil {
		t.Fatal(err2)
	}
	var rep domain.AgentStatus
	eventually(t, "the report after the Sync", func() bool {
		rep, err = st.AgentStatus(ctx, name)
		return err == nil && rep.Revision == "r1" && len(rep.Live.Containers) == 1 && rep.Live.Containers[0].State == "running"
	})
	secret := map[string]bool{}
	for _, res := range rep.Live.Resources {
		secret[res.Kind+" "+res.Name] = res.Secret
	}
	if !secret["env_file .env"] || secret["bind conf.txt"] || rep.Drifted || rep.Details["volume/"+name+"_data"] == nil {
		t.Fatalf("report: secrets %v, drifted %v, details %v", secret, rep.Drifted, rep.Details)
	}
	if b, _ := json.Marshal(rep); strings.Contains(string(b), "decrypted-on-the-agent") {
		t.Fatal("a secret's value in the report")
	}

	var logs bytes.Buffer
	eventually(t, "logs from the Agent", func() bool {
		logs.Reset()
		return hub.Logs(ctx, "edge", name, "", false, 20, &logs) == nil && strings.Contains(logs.String(), "conf v1")
	})
	fctx, fstop := context.WithTimeout(ctx, 2*time.Second)
	defer fstop()
	if err := hub.Logs(fctx, "edge", name, "", true, 5, &logs); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("following logs end with their context: %v", err)
	}
	if err := hub.Logs(ctx, "edge", name, "no-such-service", false, 5, &logs); err == nil {
		t.Fatal("logs of a service the app doesn't have")
	}

	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ContainerStop(ctx, rep.Live.Containers[0].ID, client.ContainerStopOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the stopped container reported", func() bool {
		rep, err = st.AgentStatus(ctx, name)
		return err == nil && rep.Live.Containers[0].State != "running" && rep.Health != domain.Healthy
	})

	// the Agent's connection ends while logs follow (of a running container: a stopped one's end at once): the
	// viewer hears the Host disconnect
	if _, err := c.ContainerStart(ctx, rep.Live.Containers[0].ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	following := make(chan error, 1)
	// the connection cut by the Control Plane while logs follow, the Agent staying up: it stops its streams of that
	// connection itself (nothing else would end them) and reconnects
	go func() { following <- hub.Logs(ctx, "edge", name, "", true, 5, io.Discard) }()
	time.Sleep(time.Second)
	hub.Disconnect("edge")
	if err := <-following; !errors.Is(err, agents.ErrDisconnected) {
		t.Fatalf("following logs on a cut connection: %v", err)
	}
	eventually(t, "the Agent back", func() bool { return hub.Online("edge") })
	go func() { following <- hub.Logs(ctx, "edge", name, "", true, 5, io.Discard) }()
	time.Sleep(time.Second)
	stop()
	select {
	case err := <-following:
		if err == nil {
			t.Fatal("following logs ended without an error when the Agent left")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("following logs outlived the Agent's connection")
	}
	eventually(t, "the Agent's leaving recorded", func() bool {
		h, _ := st.GetHost(ctx, "edge")
		return h.Agent != nil && !h.Agent.LeftAt.IsZero()
	})
}

// M4-11 against a real engine: the Diff of a Revision not deployed, through the hub. A new secret value shows as a
// service change (the Agent decrypted the staged files), nothing is deployed or cached as a Revision, no staged file
// is left; a broken Revision is refused; an app never deployed shows its services as new.
func TestAgentDiffThroughHub(t *testing.T) {
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
	hub.Blob = func(_ context.Context, hash string) ([]byte, error) { return src.fetch(hash) }
	srv := httptest.NewServer(hub)
	defer srv.Close()
	invite, _ := st.CreateInvite(ctx, "edge", time.Now().Add(time.Hour))
	key, _ := SigningKey(t.TempDir())
	go ag.Connect(ctx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: recipient, HostKey: hostKey})
	eventually(t, "the Agent enrolled", func() bool { h, _ := st.GetHost(ctx, "edge"); return h.Fingerprint != "" })
	hub.Approve(ctx, "edge", domain.Fingerprint(key.Public().(ed25519.PublicKey)))

	a := App{Name: name}
	files := func(secret string) map[string]string {
		return map[string]string{
			"compose.yaml": strings.Replace(compose, "%s", "", 1),
			"conf.txt":     "conf v1",
			".sops.env":    encrypted(t, "SECRET="+secret+"\n", ".sops.env", recipient),
		}
	}
	never := src.revision(name, "r0", files("first"))
	var services []domain.ServiceDiff
	eventually(t, "the approved Agent's diff", func() bool {
		services, err = hub.Diff(ctx, "edge", desiredState(a, never, 0))
		return !errors.Is(err, agents.ErrNotApproved) && !errors.Is(err, agents.ErrOffline)
	})
	if err != nil || len(services) == 0 || services[0].Change != "added" {
		t.Fatalf("an app never deployed: %+v %v", services, err)
	}
	if fi, err := os.Stat(ag.work.Root); err != nil || fi.Mode().Perm() != 0o750 {
		t.Fatalf("the work root as the Diff left it: %v %v", fi.Mode(), err) // the engine, maybe another user, traverses it
	}
	if err := hub.Deploy(ctx, "edge", desiredState(a, src.revision(name, "r1", files("first")), 1)); err != nil {
		t.Fatal(err)
	}
	before := cached(t, ag.db, name)

	services, err = hub.Diff(ctx, "edge", desiredState(a, src.revision(name, "r2", files("second")), 0))
	if err != nil || len(services) != 1 || services[0].Change != "changed" {
		t.Fatalf("a new secret value: %+v %v", services, err)
	}
	if after := cached(t, ag.db, name); !slices.Equal(after, before) {
		t.Fatalf("the cache's Revisions changed: %v, were %v", after, before)
	}
	if app, _ := ag.db.GetApp(ctx, name); app.Revision != "r1" {
		t.Fatalf("deployed: %s", app.Revision)
	}
	if entries, _ := os.ReadDir(filepath.Join(ag.work.Root, stagingDir)); len(entries) != 0 {
		t.Fatalf("staged files left: %v", entries)
	}
	if !strings.Contains(logs(t, ag, name), "secret=first") {
		t.Fatal("the running app changed")
	}

	broken := map[string]string{"compose.yaml": "services: [not, a, map]\n"}
	if _, err := hub.Diff(ctx, "edge", desiredState(a, src.revision(name, "broken", broken), 0)); err == nil {
		t.Fatal("a broken Revision diffed")
	}
	missing := src.revision(name, "nofile", map[string]string{"other.yaml": "x: 1\n"})
	if _, err := hub.Diff(ctx, "edge", desiredState(a, missing, 0)); err == nil || !strings.Contains(err.Error(), "compose file") {
		t.Fatalf("no compose file: %v", err)
	}
}

// M4-11: a Revision the Agent can't stage fails its Diff, saying why, and leaves nothing staged: files the Control
// Plane can't send, a secret encrypted for another Host, a staging folder it can't make.
func TestDiffCannotStage(t *testing.T) {
	ag, src, _ := testAgent(t)
	ctx := t.Context()
	a := App{Name: appName(t)}
	compose := map[string]string{"compose.yaml": "services: {}\n"}
	lost := func(string) ([]byte, error) { return nil, errors.New("connection lost") }
	if _, err := ag.diff(ctx, desiredState(a, src.revision(a.Name, "r1", compose), 0), lost); err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("files not sent: %v", err)
	}
	other, _ := age.GenerateX25519Identity()
	foreign := map[string]string{"compose.yaml": "services: {}\n", ".sops.env": encrypted(t, "SECRET=x\n", ".sops.env", other.Recipient().String())}
	if _, err := ag.diff(ctx, desiredState(a, src.revision(a.Name, "r2", foreign), 0), src.fetch); err == nil {
		t.Fatal("a secret for another Host decrypted")
	}
	if entries, _ := os.ReadDir(filepath.Join(ag.work.Root, stagingDir)); len(entries) != 0 {
		t.Fatalf("staged files left: %v", entries)
	}
	os.RemoveAll(filepath.Join(ag.work.Root, stagingDir))
	os.WriteFile(filepath.Join(ag.work.Root, stagingDir), nil, 0o600)
	if _, err := ag.diff(ctx, desiredState(a, src.revision(a.Name, "r3", compose), 0), src.fetch); err == nil {
		t.Fatal("staged without a staging folder")
	}
}
