package engine

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// unchanged lists the services whose containers already run exactly what the project says, though Compose would
// recreate them: their com.docker.compose.image label holds the same image written another way. Compose v5 records a
// platform manifest digest where older Compose (or another image store) recorded the multi-arch index digest, so a
// stack started by Ubuntu's Compose 2.x would be recreated on adoption, and on every Sync until then (Q30). A service
// counts when every container has the config hash Compose expects, runs the very image the service's reference
// resolves to now, and sits on the project's networks and volumes. Services that share another container's namespace
// or volumes are left to Compose, which recreates them along with their parent.
func unchanged(ctx context.Context, c client.APIClient, p *types.Project) ([]string, error) {
	ctrs, err := containers(ctx, c, p.Name)
	if err != nil {
		return nil, err
	}
	byService := map[string][]container.Summary{}
	for _, ct := range ctrs {
		byService[ct.Labels[api.ServiceLabel]] = append(byService[ct.Labels[api.ServiceLabel]], ct)
	}
	var keep []string
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		svc := p.Services[name]
		live := byService[name]
		if len(live) == 0 || len(svc.VolumesFrom) > 0 || slices.ContainsFunc([]string{svc.NetworkMode, svc.Ipc, svc.Pid}, func(m string) bool {
			return strings.HasPrefix(m, "service:") || strings.HasPrefix(m, "container:")
		}) {
			continue
		}
		resolved, ok := resolveRefs(svc, byService)
		if !ok {
			continue
		}
		hash, err := serviceHash(resolved)
		if err != nil {
			return nil, err
		}
		img, err := c.ImageInspect(ctx, svc.Image)
		if err != nil {
			continue // not here yet: Compose pulls it, and recreates
		}
		if slices.ContainsFunc(live, func(ct container.Summary) bool {
			return ct.Labels[api.ConfigHashLabel] != hash || ct.ImageID != img.ID || !onProjectNetworks(p, svc, ct) || !onProjectVolumes(p, svc, ct)
		}) {
			continue
		}
		keep = append(keep, name)
	}
	return keep, nil
}

func onProjectNetworks(p *types.Project, svc types.ServiceConfig, ct container.Summary) bool {
	if svc.NetworkMode != "" || ct.NetworkSettings == nil {
		return svc.NetworkMode != "" // host/none: nothing to attach
	}
	for key := range svc.Networks {
		if _, ok := ct.NetworkSettings.Networks[p.Networks[key].Name]; !ok {
			return false
		}
	}
	return true
}

func onProjectVolumes(p *types.Project, svc types.ServiceConfig, ct container.Summary) bool {
	for _, v := range svc.Volumes {
		if v.Type != types.VolumeTypeVolume || v.Source == "" {
			continue
		}
		want := p.Volumes[v.Source].Name
		if !slices.ContainsFunc(ct.Mounts, func(m container.MountPoint) bool { return m.Type == "volume" && m.Name == want }) {
			return false
		}
	}
	return true
}
