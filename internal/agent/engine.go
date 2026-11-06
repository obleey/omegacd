package agent

import (
	"context"
	"slices"
	"strings"

	"github.com/moby/moby/client"
)

// EngineInfo is the Docker-compatible engine the Agent runs Compose against, as it reports it (Q34).
type EngineInfo struct {
	Name     string // "docker" or "podman"
	Version  string
	Rootless bool
	OS, Arch string
}

// DetectEngine asks the engine at DOCKER_HOST (or the default socket) what it is.
func DetectEngine(ctx context.Context) (EngineInfo, error) {
	c, err := client.New(client.FromEnv)
	if err != nil {
		return EngineInfo{}, err
	}
	defer c.Close()
	v, err := c.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return EngineInfo{}, err
	}
	info, err := c.Info(ctx, client.InfoOptions{})
	if err != nil {
		return EngineInfo{}, err
	}
	var components []component
	for _, c := range v.Components {
		components = append(components, component{c.Name, c.Version})
	}
	return engineInfo(v.Version, v.Os, v.Arch, components, info.Info.SecurityOptions), nil
}

type component struct{ name, version string }

// engineInfo tells Podman from Docker by its component, and rootless by the security option both report.
func engineInfo(version, os, arch string, components []component, securityOptions []string) EngineInfo {
	e := EngineInfo{Name: "docker", Version: version, OS: os, Arch: arch}
	if i := slices.IndexFunc(components, func(c component) bool { return strings.HasPrefix(c.name, "Podman") }); i >= 0 {
		e.Name, e.Version = "podman", components[i].version
	}
	e.Rootless = slices.ContainsFunc(securityOptions, func(o string) bool {
		return slices.Contains(strings.Split(o, ","), "name=rootless")
	})
	return e
}
