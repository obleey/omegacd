package engine

import (
	"fmt"
	"path/filepath"

	"github.com/docker/cli/opts"
)

// DockerHost is the container engine OmegaCD talks to (M4-04): DOCKER_HOST when set, checked first (the Docker client
// exits the process on an address it can't use); otherwise the first socket that exists of rootful Docker, rootless
// Docker, rootless Podman and rootful Podman, with any others found, so the choice is logged. One Agent, one engine.
// "" and no error: none found, the default socket is tried.
func DockerHost(getenv func(string) string, isSocket func(path string) bool) (host string, others []string, err error) {
	if v := getenv("DOCKER_HOST"); v != "" {
		if _, err := opts.ParseHost(false, v); err != nil {
			return "", nil, fmt.Errorf("DOCKER_HOST=%q isn't a container engine address (%v): want unix:///path/to/docker.sock, "+
				"tcp://host:port or ssh://user@host, or leave it unset to find the engine's socket", v, err)
		}
		return v, nil, nil
	}
	sockets := []string{"/var/run/docker.sock"}
	if run := getenv("XDG_RUNTIME_DIR"); run != "" {
		sockets = append(sockets, filepath.Join(run, "docker.sock"), filepath.Join(run, "podman", "podman.sock"))
	}
	for _, s := range append(sockets, "/run/podman/podman.sock") {
		if isSocket(s) {
			if host == "" {
				host = "unix://" + s
			} else {
				others = append(others, "unix://"+s)
			}
		}
	}
	return host, others, nil
}
