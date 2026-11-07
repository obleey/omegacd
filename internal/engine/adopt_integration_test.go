//go:build integration

package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
)

// A stack started by hand with the Compose installed on the Host (whatever version: Ubuntu packages 2.x) is adopted
// without recreating containers whose configuration didn't change: import and disaster recovery rely on it (Q30).
func TestAdoptKeepsContainersOfAnyCompose(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker CLI")
	}
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	app.Adopt = true
	dir := t.TempDir()
	write(t, dir, "compose.yaml", "services:\n  app:\n    image: busybox:1.36\n    command: [\"sleep\", \"3600\"]\n    volumes: [data:/data]\nvolumes:\n  data: {}\n")
	up := exec.Command("docker", "compose", "-p", app.Name, "up", "-d", "--quiet-pull")
	up.Dir = dir
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("docker compose up: %v\n%s", err, out)
	}
	version, _ := exec.Command("docker", "compose", "version", "--short").Output()
	c := e.local().cli.Client()
	ids := func() (ctrs, nets, vols string) {
		f := make(client.Filters).Add("label", "com.docker.compose.project="+app.Name)
		cl, _ := c.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f})
		for _, x := range cl.Items {
			ctrs += x.ID[:12] + " "
		}
		nl, _ := c.NetworkList(ctx, client.NetworkListOptions{Filters: f})
		for _, x := range nl.Items {
			nets += x.Name + "=" + x.ID[:12] + " "
		}
		vl, _ := c.VolumeList(ctx, client.VolumeListOptions{Filters: f})
		for _, x := range vl.Items {
			vols += x.Name + " "
		}
		return
	}
	c1, n1, v1 := ids()
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	c2, n2, v2 := ids()
	t.Logf("compose %s: containers %s→ %s networks %s→ %s volumes %s→ %s", strings.TrimSpace(string(version)), c1, c2, n1, n2, v1, v2)
	if c1 == "" || c1 != c2 {
		t.Errorf("adopting recreated the containers")
	}
	if n1 != n2 || v1 != v2 {
		t.Errorf("adopting replaced networks or volumes")
	}

	// kept only while nothing changed: another image is a new container
	write(t, dir, "compose.yaml", "services:\n  app:\n    image: busybox:1.37\n    command: [\"sleep\", \"3600\"]\n    volumes: [data:/data]\nvolumes:\n  data: {}\n")
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if c3, _, _ := ids(); c3 == c2 {
		t.Error("a new image did not recreate the container")
	}
}
