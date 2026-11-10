package engine

import (
	"slices"
	"strings"
	"testing"
)

func TestDockerHost(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	sockets := func(s ...string) func(string) bool { return func(p string) bool { return slices.Contains(s, p) } }

	host, others, err := DockerHost(env(map[string]string{"DOCKER_HOST": "unix:///custom.sock"}), sockets("/var/run/docker.sock"))
	if err != nil || host != "unix:///custom.sock" || others != nil {
		t.Errorf("set: %q %v %v", host, others, err)
	}
	for _, bad := range []string{"bogus://nowhere", "http://x:2375", "tcp://host:port"} {
		if _, _, err := DockerHost(env(map[string]string{"DOCKER_HOST": bad}), sockets()); err == nil || !strings.Contains(err.Error(), "isn't a container engine address") {
			t.Errorf("%q: %v", bad, err)
		}
	}

	run := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}
	host, others, err = DockerHost(env(run), sockets("/run/user/1000/podman/podman.sock", "/var/run/docker.sock", "/run/podman/podman.sock"))
	if err != nil || host != "unix:///var/run/docker.sock" || !slices.Equal(others, []string{"unix:///run/user/1000/podman/podman.sock", "unix:///run/podman/podman.sock"}) {
		t.Errorf("rootful first, the others named: %q %v %v", host, others, err)
	}
	if host, _, _ = DockerHost(env(run), sockets("/run/user/1000/docker.sock")); host != "unix:///run/user/1000/docker.sock" {
		t.Errorf("rootless Docker: %q", host)
	}
	if host, others, err = DockerHost(env(nil), sockets("/run/user/1000/docker.sock")); host != "" || others != nil || err != nil {
		t.Errorf("none (no runtime dir to look in): %q %v %v", host, others, err)
	}
}
