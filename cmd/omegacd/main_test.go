package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalHostDisabled(t *testing.T) {
	for v, want := range map[string]bool{"": false, "true": false, "TRUE": false, " false ": true, "false": true} {
		if got, err := localHostDisabled(v); err != nil || got != want {
			t.Errorf("%q: %v, %v", v, got, err)
		}
	}
	for _, v := range []string{"no", "0", "off", "yes"} {
		if _, err := localHostDisabled(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestPollInterval(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 3 * time.Minute, "30s": 30 * time.Second, " 1h ": time.Hour, "0": 0, "0s": 0} {
		if got, err := pollInterval(v); err != nil || got != want {
			t.Errorf("%q: %v, %v", v, got, err)
		}
	}
	for _, v := range []string{"-1m", "3", "often"} {
		if _, err := pollInterval(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

// M4-04: the engine is settled before anything talks to it: a DOCKER_HOST that isn't an address stops OmegaCD with its
// own message (the Docker client would exit on it), a found socket is set for everything after, and the choice is logged.
func TestUseDockerHost(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	none := func(string) bool { return false }
	if err := useDockerHost(log, env(map[string]string{"DOCKER_HOST": "http://x:2375"}), none); err == nil ||
		!strings.Contains(err.Error(), "isn't a container engine address") {
		t.Fatalf("a bad DOCKER_HOST: %v", err)
	}
	if err := useDockerHost(log, env(map[string]string{"DOCKER_HOST": "unix:///custom.sock"}), none); err != nil ||
		!strings.Contains(logs.String(), "from=DOCKER_HOST") {
		t.Fatalf("set: %v\n%s", err, logs.String())
	}
	if err := useDockerHost(log, env(nil), none); err != nil || !strings.Contains(logs.String(), "no container engine socket found") {
		t.Fatalf("none: %v\n%s", err, logs.String())
	}
	t.Setenv("DOCKER_HOST", "") // restored after
	both := func(p string) bool { return p == "/var/run/docker.sock" || p == "/run/podman/podman.sock" }
	if err := useDockerHost(log, env(nil), both); err != nil || os.Getenv("DOCKER_HOST") != "unix:///var/run/docker.sock" ||
		!strings.Contains(logs.String(), "others too") {
		t.Fatalf("found: %v %q\n%s", err, os.Getenv("DOCKER_HOST"), logs.String())
	}
	if err := useDockerHost(log, env(nil), func(p string) bool { return p == "/run/podman/podman.sock" }); err != nil ||
		!strings.Contains(logs.String(), `msg="container engine socket found" dockerHost=unix:///run/podman/podman.sock`) {
		t.Fatalf("one found: %v\n%s", err, logs.String())
	}
	dir := t.TempDir()
	if isSocket(dir) || isSocket(filepath.Join(dir, "none")) {
		t.Fatal("a folder or nothing taken for a socket")
	}
}
