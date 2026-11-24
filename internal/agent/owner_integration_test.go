//go:build integration

package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
)

// M7-09 (Q336, Q338): two OmegaCDs' Agents on one engine, an Application of the same name. The second is refused, and
// its Sync, removal and Self-Heal leave the first's container and volume as they were; adopting takes it over on
// purpose, its containers recreated as its own, after which the first touches it no more.
func TestTwoOmegaCDsOneEngine(t *testing.T) {
	ctx := t.Context()
	first, src, recipient := testAgent(t)
	second, _, _ := testAgent(t)
	if err := first.db.BindControlPlane(ctx, "omegacd-a"); err != nil {
		t.Fatal(err)
	}
	if err := second.db.BindControlPlane(ctx, "omegacd-b"); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name := appName(t)
	t.Cleanup(func() {
		first.Remove(context.Background(), name, true)
		second.Remove(context.Background(), name, true)
	})
	files := map[string]string{"compose.yaml": strings.Replace(compose, "%s", "", 1), "conf.txt": "conf a",
		".sops.env": encrypted(t, "SECRET=a\n", ".sops.env", recipient)}
	if err := first.Deploy(ctx, App{Name: name, SelfHeal: true}, src.revision(name, "a1", files), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	running := func() []string {
		r, err := c.ContainerList(ctx, client.ContainerListOptions{Filters: make(client.Filters).Add("label", "com.docker.compose.project="+name)})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, ctr := range r.Items {
			if ctr.State == "running" {
				ids = append(ids, ctr.ID+" "+ctr.Labels[domain.OwnerLabel])
			}
		}
		return ids
	}
	volumes := func() int {
		r, err := c.VolumeList(ctx, client.VolumeListOptions{Filters: make(client.Filters).Add("label", "com.docker.compose.project="+name)})
		if err != nil {
			t.Fatal(err)
		}
		return len(r.Items)
	}
	before := running()
	if len(before) != 1 || !strings.HasSuffix(before[0], " omegacd-a") || volumes() != 1 {
		t.Fatalf("the first OmegaCD's app: %v, %d volumes", before, volumes())
	}

	// the second OmegaCD's Sync of the same name: refused, naming the owner
	files2 := map[string]string{"compose.yaml": strings.Replace(compose, "%s", "", 1), "conf.txt": "conf b",
		".sops.env": encrypted(t, "SECRET=b\n", ".sops.env", second.secrets.sources[0].(SOPS).HostKey.Recipient().String())}
	r2 := src.revision(name, "b1", files2)
	err = second.Deploy(ctx, App{Name: name, SelfHeal: true}, r2, src.fetch, nil)
	if !errors.Is(err, domain.ErrOwned) || !strings.Contains(err.Error(), "belongs to another OmegaCD (omegacd-a)") {
		t.Fatalf("the second OmegaCD's Sync: %v", err)
	}
	// its removal (it never deployed it) and its Self-Heal (it has nothing to heal) touch nothing
	if err := second.Remove(ctx, name, true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the second OmegaCD's removal: %v", err)
	}
	if err := second.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if after := running(); !slices.Equal(after, before) || volumes() != 1 {
		t.Fatalf("the first OmegaCD's app changed: %v, %d volumes", after, volumes())
	}

	// adopting takes it over on purpose: recreated as the second's own; the first then refuses to touch it
	if err := second.Deploy(ctx, App{Name: name, Adopt: true}, r2, src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	if after := running(); len(after) != 1 || !strings.HasSuffix(after[0], " omegacd-b") || after[0] == before[0] {
		t.Fatalf("adopted: %v", after)
	}
	if err := first.Deploy(ctx, App{Name: name}, src.revision(name, "a2", files), src.fetch, nil); !errors.Is(err, domain.ErrOwned) {
		t.Fatalf("the first OmegaCD's Sync after the takeover: %v", err)
	}
	if err := first.Remove(ctx, name, true); !errors.Is(err, domain.ErrOwned) || volumes() != 1 {
		t.Fatalf("the first OmegaCD's removal after the takeover: %v, %d volumes", err, volumes())
	}
}
