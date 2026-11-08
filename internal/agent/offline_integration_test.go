//go:build integration

package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/engine"
	"github.com/obleey/omegacd/internal/logging"
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
	var log logging.Buffer
	ag.log, _ = logging.New(&log, "debug", "text")
	go ag.Run(ctx) // no Control Plane anywhere
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
	// Q62: it says why it did, and why the restore before did nothing
	for _, want := range []string{
		`msg="restore: nothing to do, files present and containers running" health=Healthy app=` + name + " revision=r1",
		`msg=self-healing why="containers `, " app=" + name + " revision=r1",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("no %q in the Agent's log:\n%s", want, log.String())
		}
	}
}

// Q25: a rollback run on the Host goes back to a cached Revision with no Control Plane anywhere, from its own process
// (omegacd agent rollback: another Agent on the same state dir) while the Agent service runs with Self-Heal on. The
// service doesn't undo it, and the outbox says it was a rollback.
func TestAgentRollsBackOffline(t *testing.T) {
	ag, src, recipient := testAgent(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	name := appName(t)
	a := App{Name: name, SelfHeal: true}
	t.Cleanup(func() { ag.Remove(context.Background(), name, true) })
	for _, rev := range []string{"r1-aaaa", "r2-bbbb"} {
		files := map[string]string{"compose.yaml": strings.Replace(compose, "%s", "", 1), "conf.txt": rev,
			".sops.env": encrypted(t, "SECRET=x\n", ".sops.env", recipient)}
		if err := ag.Deploy(ctx, a, src.revision(name, rev, files), src.fetch, nil); err != nil {
			t.Fatal(err)
		}
	}
	go ag.Run(ctx) // the service
	time.Sleep(2 * time.Second)

	state := filepath.Dir(ag.db.path)
	db := openDB(t, ag.db.path)
	defer db.Close()
	cache, err := OpenCache(db, filepath.Join(state, "cache"), 10)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := HostKey(state)
	eng, err := engine.New(engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	byHand := New(db, cache, NewWorkDir(ag.work.Root, db, cache, nil), NewSecrets(-1, SOPS{HostKey: key}), eng, nil)
	if err := byHand.Rollback(ctx, name, "r1"); err != nil { // a prefix will do
		t.Fatal(err)
	}

	conf := func() string { b, _ := os.ReadFile(filepath.Join(ag.work.Dir(a), "conf.txt")); return string(b) }
	deployed := func() string { x, _ := ag.db.GetApp(ctx, name); return x.Revision }
	if conf() != "r1-aaaa" || deployed() != "r1-aaaa" {
		t.Fatalf("after the rollback: files of %q, deployed %q", conf(), deployed())
	}
	time.Sleep(selfHealDelay + 3*time.Second) // the service saw the containers change: it must not heal back to r2
	live, _ := ag.Status(ctx, name)
	if conf() != "r1-aaaa" || deployed() != "r1-aaaa" || !running(live) {
		t.Fatalf("the service undid the rollback: files of %q, deployed %q, %s", conf(), deployed(), live.Health)
	}
	if reports(t, ag, triggerRollback) != 1 {
		t.Fatal("the rollback isn't in the outbox")
	}
}
