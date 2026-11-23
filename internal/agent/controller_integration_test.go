//go:build integration

package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/controller"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	srcserver "github.com/obleey/omegacd/internal/source"
	"github.com/obleey/omegacd/internal/store"
)

// M7-01 (Q332): the Control Plane drives a real Agent through an Application's life on the engine at DOCKER_HOST (each
// of make test-engines'), wired as cmd/omegacd wires them: the controller renders from Git through the Source Server,
// the hub carries the Sync to the Agent, which fetches the files, decrypts and deploys. Create and Sync, drift healed
// by the Agent's Self-Heal, a new commit's Diff and Sync, a Rollback, a delete with volumes.
func TestAgentDrivenByControlPlane(t *testing.T) {
	ag, _, recipient := testAgent(t)
	hostKey := ag.secrets.sources[0].(SOPS).HostKey
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	name := appName(t)
	t.Cleanup(func() { ag.Remove(context.Background(), name, true) })
	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// the repository: the app with a file from Git, a secret only the Host's key opens, a named volume
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=it", "-c", "user.email=it@local"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "app", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(repo, "app"), 0o755)
	write("compose.yaml", strings.Replace(compose, "%s", "", 1))
	write("conf.txt", "conf v1")
	write(".sops.env", encrypted(t, "SECRET=from-the-control-plane\n", ".sops.env", recipient))
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-qm", "v1")

	// the Control Plane, as cmd/omegacd wires it
	data := t.TempDir()
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(data, "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src, err := srcserver.NewServer(data)
	if err != nil {
		t.Fatal(err)
	}
	payloads, err := blob.Open(filepath.Join(data, "payloads"))
	if err != nil {
		t.Fatal(err)
	}
	hub := agents.New(st, nil)
	ctl := controller.New(controller.Config{Agents: hub, Store: st, Source: src, Blobs: payloads, NormalizeURL: srcserver.NormalizeURL})
	hub.Blob = ctl.Blob
	hub.Reported, hub.Resynced, hub.Removed, hub.Progress, hub.Status, hub.HostChanged = ctl.AgentReported, ctl.Resynced,
		ctl.AgentRemoved, ctl.AgentProgress, ctl.AgentStatusChanged, ctl.HostChanged
	go ctl.Run(ctx)
	srv := httptest.NewServer(hub)
	defer srv.Close()

	// the Host: invited, its Agent enrolls and is approved, then keeps what it deploys (Self-Heal) as cmd/omegacd runs it
	invite, _, err := ctl.InviteHost(ctx, "edge")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := SigningKey(t.TempDir())
	go ag.Connect(ctx, Connection{URL: srv.URL, Invite: invite, Key: key, AgeRecipient: recipient, HostKey: hostKey})
	go ag.Run(ctx)
	eventually(t, "the Agent enrolled", func() bool { h, _ := st.GetHost(ctx, "edge"); return h.Fingerprint != "" })
	if err := hub.Approve(ctx, "edge", domain.Fingerprint(key.Public().(ed25519.PublicKey))); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Host online", func() bool { return hub.Online("edge") })

	// its containers' logs, "" while a Sync recreates them (a container gone mid-read is not yet, not a failure)
	logsNow := func() string {
		var out bytes.Buffer
		lctx, lcancel := context.WithTimeout(ctx, 30*time.Second)
		defer lcancel()
		if ag.Logs(lctx, name, "", false, 100, &out) != nil {
			return ""
		}
		return out.String()
	}
	view := func() domain.AppView {
		t.Helper()
		v, err := ctl.GetApp(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	synced := func(what string, op int64) {
		t.Helper()
		eventually(t, what, func() bool {
			v := view()
			return v.LastOperation != nil && v.LastOperation.ID == op && v.LastOperation.Phase == domain.OpSucceeded &&
				v.SyncStatus == domain.Synced && v.Health == domain.Healthy
		})
	}
	running := func() []string {
		t.Helper()
		r, err := c.ContainerList(ctx, client.ContainerListOptions{Filters: make(client.Filters).Add("label", "com.docker.compose.project="+name)})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, ctr := range r.Items {
			if ctr.State == "running" {
				ids = append(ids, ctr.ID)
			}
		}
		return ids
	}

	// create and Sync: the secret decrypted on the Agent, the file from Git mounted
	if _, err := ctl.CreateApp(ctx, domain.Application{Name: name, RepoURL: repo, Path: "app", Host: "edge", SelfHeal: true, Prune: true}); err != nil {
		t.Fatal(err)
	}
	first, err := ctl.Sync(ctx, name, domain.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	synced("the first Sync, Synced and Healthy", first)
	eventually(t, "its container reads the file and the secret", func() bool {
		l := logsNow()
		return strings.Contains(l, "conf v1") && strings.Contains(l, "secret=from-the-control-plane")
	})

	// drift: the container stopped by hand; the Agent's Self-Heal starts it again and the Control Plane sees it Healthy
	ids := running()
	if len(ids) != 1 {
		t.Fatalf("running containers %v, want 1", ids)
	}
	if _, err := c.ContainerStop(ctx, ids[0], client.ContainerStopOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "healed by the Agent", func() bool { return len(running()) == 1 && view().Health == domain.Healthy })

	// a new commit: OutOfSync, its Diff names the service, its Sync deploys it
	write("conf.txt", "conf v2")
	git("commit", "-qam", "v2")
	if v, err := ctl.Refresh(ctx, name); err != nil || v.SyncStatus != domain.OutOfSync {
		t.Fatalf("Refresh = %s, %v; want OutOfSync", v.SyncStatus, err)
	}
	d, err := ctl.Diff(ctx, name)
	if err != nil || len(d.Services) == 0 || d.Services[0].Service != "web" || !strings.Contains(d.GitDiff, "+conf v2") {
		t.Fatalf("Diff = %+v, %v", d, err)
	}
	second, err := ctl.Sync(ctx, name, domain.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	synced("the second Sync", second)
	eventually(t, "the new file deployed", func() bool { return strings.Contains(logsNow(), "conf v2") })

	// a Rollback to the first Sync's Revision
	back, err := ctl.Rollback(ctx, name, first)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "rolled back", func() bool {
		v := view()
		l := logsNow()
		return v.LastOperation != nil && v.LastOperation.ID == back && v.LastOperation.Phase == domain.OpSucceeded &&
			strings.LastIndex(l, "conf v1") > strings.LastIndex(l, "conf v2")
	})

	// delete with its volumes: the Agent removes the project, then the Control Plane forgets the Application
	if err := ctl.DeleteApp(ctx, name, true, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted with its volume", func() bool {
		_, err := ctl.GetApp(ctx, name)
		vols, verr := c.VolumeList(ctx, client.VolumeListOptions{Filters: make(client.Filters).Add("label", "com.docker.compose.project="+name)})
		return errors.Is(err, domain.ErrNotFound) && verr == nil && len(vols.Items) == 0 && len(running()) == 0
	})
}
