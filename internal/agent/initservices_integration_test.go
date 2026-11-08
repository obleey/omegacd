//go:build integration

package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// initCompose is the init pattern: a one-shot migrate that web waits for (service_completed_successfully), both on one
// volume: each run appends a line, web prints them all. FAIL=1 makes the migration fail.
const initCompose = `services:
  migrate:
    image: docker.io/library/busybox:1.36
    command: ["sh", "-c", "echo migrated-%[1]s >> /data/log; [ %[2]s != 1 ]"]
    volumes: ["data:/data"]
  web:
    image: docker.io/library/busybox:1.36
    command: ["sh", "-c", "echo web-%[1]s; cat /data/log; sleep 3600"]
    depends_on:
      migrate:
        condition: service_completed_successfully
    volumes: ["data:/data"]
volumes:
  data: {}
`

// M2-11: Compose's own init pattern through the Agent: the app starts after its migration, a completed one-shot is
// Healthy and Self-Heal leaves it alone, a failing migration fails the Sync with its reason and leaves the running app
// untouched, and a restore runs the migration again.
func TestAgentRunsInitServices(t *testing.T) {
	ag, src, _ := testAgent(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	name := appName(t)
	a := App{Name: name, SelfHeal: true, Prune: true} // Prune: a completed one-shot must not count as an orphan
	t.Cleanup(func() { ag.Remove(context.Background(), name, true) })
	rev := func(r, fail string) Revision {
		return src.revision(name, r, map[string]string{"compose.yaml": strings.ReplaceAll(strings.ReplaceAll(initCompose, "%[1]s", r), "%[2]s", fail)})
	}
	go ag.Run(ctx) // the service, Self-Heal on: it must leave the completed one-shot alone
	time.Sleep(2 * time.Second)

	if err := ag.Deploy(ctx, a, rev("r1", "0"), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "web after its migration", func() bool {
		l := logs(t, ag, name)
		return strings.Contains(l, "web-r1") && strings.Index(l, "migrated-r1") > strings.Index(l, "web-r1")
	})
	live, err := ag.Status(ctx, name)
	if err != nil || !running(live) || len(live.Containers) != 2 {
		t.Fatalf("after the first Sync: %+v %v", live, err)
	}
	if n := strings.Count(logs(t, ag, name), "migrated-r1"); n != 1 {
		t.Fatalf("the migration ran %d times in one Sync", n)
	}
	time.Sleep(selfHealDelay + 2*time.Second) // the one-shot's exit was an engine event
	if n := reports(t, ag, triggerSelfHeal); n != 0 {
		t.Fatalf("Self-Heal re-ran a completed one-shot %d times", n)
	}

	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	web := func() string {
		live, _ := ag.Status(ctx, name)
		for _, ct := range live.Containers {
			if ct.Service == "web" {
				return ct.ID
			}
		}
		return ""
	}
	before := web()
	err = ag.Deploy(ctx, a, rev("r2", "1"), src.fetch, nil)
	if err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("a failing migration: %v", err)
	}
	if web() != before {
		t.Fatal("a failing migration replaced the running app")
	}
	if x, _ := ag.db.GetApp(ctx, name); x.Revision != "r1" {
		t.Fatalf("deployed %q after a failed Sync", x.Revision)
	}
	if insp, err := c.ContainerInspect(ctx, before, client.ContainerInspectOptions{}); err != nil || !insp.Container.State.Running {
		t.Fatalf("the app isn't running after a failed migration: %v", err)
	}

	// a reboot with the work dir in RAM: files gone; the restore runs the migration again
	os.RemoveAll(ag.work.Dir(a))
	if err := ag.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the migration run again", func() bool { return strings.Count(logs(t, ag, name), "migrated-r1") >= 2 })
}
