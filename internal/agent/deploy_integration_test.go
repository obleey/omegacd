//go:build integration

package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/engine"
	"github.com/obleey/omegacd/internal/secrets"
)

// The Agent's whole deploy path against the engine at DOCKER_HOST: rootful Docker by default, rootless Docker and
// rootless Podman in the matrix (make test-engines). OMEGACD_IT_ROOT is a folder the engine sees at the same path.

func testAgent(t *testing.T) (*Agent, *source, string) {
	t.Helper()
	root := os.Getenv("OMEGACD_IT_ROOT")
	if root == "" {
		root = t.TempDir()
	}
	base, err := os.MkdirTemp(root, "agent-it-")
	if err != nil {
		t.Fatal(err)
	}
	// in the matrix the rootless engine runs as another user than this test (a real rootless Agent is the engine's
	// own user): it must be able to traverse to the work dir
	if err := os.Chmod(base, 0o711); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	state := filepath.Join(base, "state")
	os.MkdirAll(state, 0o700)
	db := openDB(t, filepath.Join(state, "agent.db"))
	t.Cleanup(func() { db.Close() })
	cache, err := OpenCache(db, filepath.Join(state, "cache"), 10)
	if err != nil {
		t.Fatal(err)
	}
	key, err := HostKey(state)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	ag := New(db, cache, NewWorkDir(filepath.Join(base, "work"), db, cache, nil), NewSecrets(-1, SOPS{HostKey: key}), eng, nil)
	return ag, &source{}, key.Recipient().String()
}

func appName(t *testing.T) string {
	b := make([]byte, 3)
	rand.Read(b)
	return "agent-it-" + hex.EncodeToString(b)
}

func encrypted(t *testing.T, plain, name, recipient string) string {
	b, err := secrets.Encrypt([]byte(plain), name, []string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func logs(t *testing.T, ag *Agent, name string) string {
	t.Helper()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := ag.Logs(ctx, name, "", false, 100, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// eventually retries check for up to 30 s: containers start and log asynchronously.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !check(); time.Sleep(500 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
	}
}

const compose = `services:
  web:
    image: docker.io/library/busybox:1.36
    command: ["sh", "-c", "cat /conf.txt; echo \"secret=$$SECRET\"; sleep 3600"]
    environment:
      SECRET: ${SECRET}
    volumes: ["./conf.txt:/conf.txt:ro", "data:/data"]
%s
volumes:
  data: {}
`

func TestAgentDeploys(t *testing.T) {
	ag, src, recipient := testAgent(t)
	ctx := t.Context()
	name := appName(t)
	a := App{Name: name, Prune: true}
	t.Cleanup(func() { ag.Remove(context.Background(), name, true) })
	files := map[string]string{
		"compose.yaml": strings.Replace(compose, "%s", "", 1),
		"conf.txt":     "conf v1",
		".sops.env":    encrypted(t, "SECRET=decrypted-on-the-agent\n", ".sops.env", recipient),
	}

	// a Revision deploys: files from Git mounted, the .env decrypted and interpolated, the project up and healthy
	if err := ag.Deploy(ctx, a, src.revision(name, "r1", files), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first Revision's logs", func() bool {
		l := logs(t, ag, name)
		return strings.Contains(l, "conf v1") && strings.Contains(l, "secret=decrypted-on-the-agent")
	})
	live, err := ag.Status(ctx, name)
	if err != nil || live.Health != domain.Healthy || len(live.Containers) != 1 {
		t.Fatalf("Status = %+v, %v", live, err)
	}
	if d, err := ag.Drift(ctx, name); err != nil || len(d) != 0 {
		t.Fatalf("drift right after a deploy: %+v %v", d, err)
	}

	// a stopped container is seen: an engine event, then Degraded
	changed := make(chan string, 100)
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	go ag.Watch(wctx, func(app string) { changed <- app })
	time.Sleep(time.Second) // the event stream is open
	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ContainerStop(ctx, live.Containers[0].ID, client.ContainerStopOptions{}); err != nil {
		t.Fatal(err)
	}
	// other tests' projects on the same engine send events too: wait for this app's
	for deadline := time.After(30 * time.Second); ; {
		select {
		case got := <-changed:
			if got != name {
				continue
			}
		case <-deadline:
			t.Fatal("no event for the stopped container")
		}
		break
	}
	if live, _ := ag.Status(ctx, name); live.Health == domain.Healthy {
		t.Fatal("a stopped container still Healthy")
	}

	// the next Revision: changed file, an added service; the one after drops it again (Prune)
	files["conf.txt"] = "conf v2"
	files["compose.yaml"] = strings.Replace(compose, "%s", "  extra:\n    image: docker.io/library/busybox:1.36\n    command: [\"sleep\", \"3600\"]", 1)
	if err := ag.Deploy(ctx, a, src.revision(name, "r2", files), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second Revision running", func() bool {
		live, _ := ag.Status(ctx, name)
		return live.Health == domain.Healthy && len(live.Containers) == 2 && strings.Contains(logs(t, ag, name), "conf v2")
	})
	files["compose.yaml"] = strings.Replace(compose, "%s", "", 1)
	if err := ag.Deploy(ctx, a, src.revision(name, "r3", files), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	if live, _ := ag.Status(ctx, name); len(live.Containers) != 1 {
		t.Fatalf("Prune left %d containers", len(live.Containers))
	}

	// a Revision the engine can't load fails, and the deployed one stays r3
	bad := map[string]string{"compose.yaml": "services: [\n"}
	if err := ag.Deploy(ctx, a, src.revision(name, "broken", bad), src.fetch, nil); err == nil {
		t.Fatal("a broken compose file deployed")
	}
	if got, _ := ag.db.GetApp(ctx, name); got.Revision != "r3" {
		t.Fatalf("deployed Revision %q after a failed deploy, want r3", got.Revision)
	}

	// Remove keeps volumes unless asked
	volumes := func() int {
		r, err := c.VolumeList(ctx, client.VolumeListOptions{Filters: make(client.Filters).Add("label", "com.docker.compose.project="+name)})
		if err != nil {
			t.Fatal(err)
		}
		return len(r.Items)
	}
	if err := ag.Remove(ctx, name, false); err != nil {
		t.Fatal(err)
	}
	if n := volumes(); n != 1 {
		t.Fatalf("%d volumes after Remove without volumes, want 1", n)
	}
	if _, err := os.Stat(ag.work.Dir(a)); !os.IsNotExist(err) {
		t.Fatalf("its folder is left: %v", err)
	}
	if err := ag.Deploy(ctx, a, src.revision(name, "r4", files), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	if err := ag.Remove(ctx, name, true); err != nil {
		t.Fatal(err)
	}
	if n := volumes(); n != 0 {
		t.Fatalf("%d volumes after Remove with volumes", n)
	}
}

// Q57: an Application in a second allowed root deploys there: the engine resolves its bind mounts in that root, the
// secret is decrypted there, and moving back to the Host's work dir leaves nothing behind.
func TestAgentDeploysInItsOwnWorkDir(t *testing.T) {
	ag, src, recipient := testAgent(t)
	ctx := t.Context()
	other := filepath.Join(filepath.Dir(ag.work.Root), "other") // where the matrix's engines see it too
	ag.work.Allow = []string{other}
	name := appName(t)
	a := App{Name: name, WorkDir: filepath.Join(other, "apps")}
	t.Cleanup(func() { ag.Remove(context.Background(), name, true) })
	files := map[string]string{"compose.yaml": strings.Replace(compose, "%s", "", 1), "conf.txt": "in its own root",
		".sops.env": encrypted(t, "SECRET=there-too\n", ".sops.env", recipient)}
	if err := ag.Deploy(ctx, a, src.revision(name, "r1", files), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the logs", func() bool {
		l := logs(t, ag, name)
		return strings.Contains(l, "in its own root") && strings.Contains(l, "secret=there-too")
	})
	if _, err := os.Stat(filepath.Join(other, "apps", name, ".env")); err != nil {
		t.Fatalf("not decrypted in its own root: %v", err)
	}

	a.WorkDir = ""
	if err := ag.Deploy(ctx, a, src.revision(name, "r2", files), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "apps", name)); err == nil {
		t.Fatal("the folder in the other root was left behind")
	}
	live, err := ag.Status(ctx, name)
	if err != nil || !running(live) {
		t.Fatalf("after moving: %+v %v", live, err)
	}
}
