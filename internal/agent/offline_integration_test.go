//go:build integration

package agent

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
)

const restartCompose = `services:
  web:
    image: docker.io/library/busybox:1.36
    restart: unless-stopped
    command: ["sh", "-c", "cat /conf.txt; echo \"secret=$$SECRET\"; sleep 3600"]
    environment:
      SECRET: ${SECRET}
    volumes: ["./conf.txt:/conf.txt:ro"]
`

func reports(t *testing.T, ag *Agent, trigger string) (n int) {
	es, err := ag.db.Outbox(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if r := e.GetSyncOperationReport(); r.GetTrigger() == trigger && r.GetPhase() == string(domain.OpSucceeded) {
			n++
		}
	}
	return n
}

// reboot is what a Host's reboot does to a RAM work dir: the containers stop, the work dir is empty.
func reboot(t *testing.T, ag *Agent, c *client.Client, a App) []string {
	live, err := ag.Status(t.Context(), a.Name)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, ct := range live.Containers {
		ids = append(ids, ct.ID)
		if _, err := c.ContainerStop(t.Context(), ct.ID, client.ContainerStopOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(ag.work.Root); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestAgentRestoresAfterReboot(t *testing.T) {
	ag, src, recipient := testAgent(t)
	ctx := t.Context()
	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name := appName(t)
	a := App{Name: name}
	t.Cleanup(func() { ag.Remove(context.Background(), name, true) })
	files := map[string]string{"compose.yaml": restartCompose, "conf.txt": "conf from the cache",
		".sops.env": encrypted(t, "SECRET=restored-offline\n", ".sops.env", recipient)}
	if err := ag.Deploy(ctx, a, src.revision(name, "r1", files), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	running := func() bool {
		live, _ := ag.Status(ctx, name)
		l := logs(t, ag, name)
		return live.Health == domain.Healthy && strings.Contains(l, "conf from the cache") && strings.Contains(l, "secret=restored-offline")
	}
	eventually(t, "deployed", running)

	// Q59 as a native Agent does it: the files come back before the engine starts anything, then Compose
	reboot(t, ag, c, a)
	if err := ag.RestoreFiles(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ag.work.Dir(a) + "/.env"); !strings.Contains(string(b), "restored-offline") {
		t.Fatalf("the files restore didn't decrypt .env: %q", b)
	}
	if err := ag.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "restored after the files came back first", running)

	// the engine was first: it restarted the container and made what was missing; the Agent still brings it back
	ids := reboot(t, ag, c, a)
	if _, err := c.ContainerStart(ctx, ids[0], client.ContainerStartOptions{}); err != nil {
		t.Logf("the engine refused to restart without the files (%v); nothing to clear then", err)
	}
	if err := ag.Restore(ctx); err != nil {
		// a native Agent can't remove a rootful engine's placeholders: it says how, and this is that fix
		if !strings.Contains(err.Error(), "restore --files") {
			t.Fatalf("the error doesn't say how to fix it: %v", err)
		}
		if err := reclaim(ctx, ag.work.Root, ag.work.Dir(a), "docker.io/library/busybox:1.36"); err != nil {
			t.Fatal(err)
		}
		if err := ag.Restore(ctx); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "restored after the engine restarted it first", running)
	if b, err := os.ReadFile(ag.work.Dir(a) + "/conf.txt"); err != nil || string(b) != "conf from the cache" {
		t.Fatalf("conf.txt = %q, %v", b, err)
	}
	if n := reports(t, ag, triggerRestore); n < 2 {
		t.Fatalf("%d restores in the outbox, want 2", n)
	}
	if got, _ := ag.db.GetApp(ctx, name); got.Revision != "r1" {
		t.Fatalf("deployed Revision %q after restores, want r1", got.Revision)
	}
}

func TestAgentSelfHealsOffline(t *testing.T) {
	ag, src, recipient := testAgent(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name := appName(t)
	a := App{Name: name, SelfHeal: true}
	t.Cleanup(func() { ag.Remove(context.Background(), name, true) })
	files := map[string]string{"compose.yaml": strings.Replace(compose, "%s", "", 1), "conf.txt": "conf",
		".sops.env": encrypted(t, "SECRET=x\n", ".sops.env", recipient)}
	if err := ag.Deploy(ctx, a, src.revision(name, "r1", files), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	go ag.Run(ctx, slog.New(slog.NewTextHandler(os.Stderr, nil))) // no Control Plane anywhere
	live, _ := ag.Status(ctx, name)
	eventually(t, "running", func() bool { live, _ = ag.Status(ctx, name); return len(live.Containers) == 1 })
	time.Sleep(2 * time.Second) // Run's event stream is open

	if _, err := c.ContainerStop(ctx, live.Containers[0].ID, client.ContainerStopOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "repaired by Self-Heal", func() bool {
		live, _ := ag.Status(ctx, name)
		return live.Health == domain.Healthy && reports(t, ag, triggerSelfHeal) >= 1
	})
	apps, _ := ag.db.ListApps(ctx)
	if !slices.ContainsFunc(apps, func(x App) bool { return x.Name == name && x.Revision == "r1" }) {
		t.Fatal("Self-Heal changed what is deployed")
	}
}
