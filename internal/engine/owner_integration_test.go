//go:build integration

package engine

import (
	"context"
	"testing"

	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/enginetest"
)

// Q335, Q336: what a Sync creates carries its owner, containers, volumes and networks alike, and Project reports it;
// another owner's volume in the project is left alone by Prune volumes and by a Remove with volumes, which takes the
// owner's own.
func TestOwner(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	app := testApp(t, e)
	app.Owner = "mine"
	dir := enginetest.TempDir(t)
	write(t, dir, "compose.yaml", "services:\n  web:\n    image: busybox\n    command: [\"sleep\", \"3600\"]\n    volumes: [data:/data]\nvolumes:\n  data: {}\n")
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if st, err := e.Project(ctx, app.Name); err != nil || len(st.Owners) != 1 || st.Owners[0] != "mine" {
		t.Fatalf("Project = %+v, %v", st, err)
	}
	c := e.local().cli.Client()
	vols, _ := c.VolumeList(ctx, client.VolumeListOptions{Filters: projectFilter(app.Name)})
	nets, _ := c.NetworkList(ctx, client.NetworkListOptions{Filters: projectFilter(app.Name)})
	if len(vols.Items) != 1 || vols.Items[0].Labels[domain.OwnerLabel] != "mine" || len(nets.Items) != 1 || nets.Items[0].Labels[domain.OwnerLabel] != "mine" {
		t.Fatalf("volumes %+v, networks %+v", vols.Items, nets.Items)
	}

	// another OmegaCD's volume in the project, undeclared here: Prune volumes keeps it
	theirs := app.Name + "_theirs"
	if _, err := c.VolumeCreate(ctx, client.VolumeCreateOptions{Name: theirs, Labels: map[string]string{
		"com.docker.compose.project": app.Name, "com.docker.compose.volume": "theirs", domain.OwnerLabel: "other"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.VolumeRemove(context.Background(), theirs, client.VolumeRemoveOptions{}) })
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{Prune: true, PruneVolumes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VolumeInspect(ctx, theirs, client.VolumeInspectOptions{}); err != nil {
		t.Fatalf("Prune volumes took another owner's volume: %v", err)
	}
	if diffs, _ := e.Diff(ctx, app, dir, ""); len(diffs) != 0 {
		t.Fatalf("another owner's volume shown as ours to prune: %+v", diffs)
	}

	// Remove with volumes: its own volume goes, the other owner's stays; failing to list or remove them says so
	t.Run("its volumes can't be listed", func(t *testing.T) {
		if f, _ := faulty(t, nil, fail("GET", "/volumes")); f.Remove(ctx, app, true) == nil {
			t.Fatal("removed")
		}
	})
	t.Run("its own volume can't be removed", func(t *testing.T) {
		if f, _ := faulty(t, nil, fail("DELETE", "/volumes/*")); f.Remove(ctx, app, true) == nil {
			t.Fatal("removed")
		}
	})
	if err := e.Remove(ctx, app, true); err != nil {
		t.Fatal(err)
	}
	vols, _ = c.VolumeList(ctx, client.VolumeListOptions{Filters: projectFilter(app.Name)})
	if len(vols.Items) != 1 || vols.Items[0].Name != theirs {
		t.Fatalf("after Remove with volumes: %+v", vols.Items)
	}
}
