package engine

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
)

func (e *Engine) ProjectFiles(ctx context.Context, app domain.Application, dir string) ([]domain.Resource, error) {
	p, err := e.load(ctx, app, dir)
	if err != nil {
		return nil, err
	}
	found := map[[2]string]*domain.Resource{}
	add := func(kind, path, target, service string) {
		if _, err := os.Stat(path); err != nil || !insideDir(p.WorkingDir, path) {
			return
		}
		rel, _ := filepath.Rel(p.WorkingDir, path)
		r := found[[2]string{kind, rel}]
		if r == nil {
			r = &domain.Resource{Kind: kind, Name: rel, Target: target, Secret: kind == "secret"}
			found[[2]string{kind, rel}] = r
		}
		if service != "" && !slices.Contains(r.Services, service) {
			r.Services = append(r.Services, service)
		}
	}
	add("env_file", filepath.Join(p.WorkingDir, ".env"), "", "") // interpolation only
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		s := p.Services[name]
		for _, f := range s.EnvFiles {
			add("env_file", f.Path, "", name)
		}
		for _, c := range s.Configs {
			if f := p.Configs[c.Source].File; f != "" {
				add("config", f, cmp.Or(c.Target, "/"+c.Source), name)
			}
		}
		for _, c := range s.Secrets {
			if f := p.Secrets[c.Source].File; f != "" {
				t := cmp.Or(c.Target, c.Source)
				if !strings.HasPrefix(t, "/") {
					t = "/run/secrets/" + t
				}
				add("secret", f, t, name)
			}
		}
		for _, v := range s.Volumes {
			if v.Type == types.VolumeTypeBind {
				add("bind", v.Source, v.Target, name)
			}
		}
	}
	out := make([]domain.Resource, 0, len(found))
	for _, r := range found {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b domain.Resource) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.Name, b.Name))
	})
	return out, nil
}

func (e *Engine) Inspect(ctx context.Context, app domain.Application, kind, name string) (map[string]string, error) {
	hc := e.hc
	c := hc.cli.Client()
	switch kind {
	case "volume":
		res, err := c.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect volume %s: %w", name, err)
		}
		ctrs, err := c.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("volume", name)})
		if err != nil {
			return nil, fmt.Errorf("list containers: %w", err)
		}
		var users []string
		for _, ct := range ctrs.Items {
			users = append(users, canonicalName(ct))
		}
		v := res.Volume
		return map[string]string{
			"driver":     v.Driver,
			"mountpoint": v.Mountpoint,
			"created":    v.CreatedAt,
			"scope":      v.Scope,
			"labels":     joinLabels(v.Labels),
			"containers": strings.Join(slices.Sorted(slices.Values(users)), ", "),
		}, nil
	case "network":
		res, err := c.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect network %s: %w", name, err)
		}
		n := res.Network
		var subnets, gateways, ctrs []string
		for _, cfg := range n.IPAM.Config {
			if cfg.Subnet.IsValid() {
				subnets = append(subnets, cfg.Subnet.String())
			}
			if cfg.Gateway.IsValid() {
				gateways = append(gateways, cfg.Gateway.String())
			}
		}
		for _, ep := range n.Containers {
			s := ep.Name
			if ep.IPv4Address.IsValid() {
				s += " (" + ep.IPv4Address.String() + ")"
			}
			ctrs = append(ctrs, s)
		}
		slices.Sort(ctrs)
		return map[string]string{
			"driver":     n.Driver,
			"scope":      n.Scope,
			"created":    n.Created.Format(time.RFC3339),
			"internal":   fmt.Sprint(n.Internal),
			"subnet":     strings.Join(subnets, ", "),
			"gateway":    strings.Join(gateways, ", "),
			"labels":     joinLabels(n.Labels),
			"containers": strings.Join(ctrs, ", "),
		}, nil
	}
	return nil, fmt.Errorf("%w: cannot inspect kind %q", domain.ErrInvalid, kind)
}

func joinLabels(l map[string]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(l)) {
		out = append(out, k+"="+l[k])
	}
	return strings.Join(out, ", ")
}
