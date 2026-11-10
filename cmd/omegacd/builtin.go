package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/obleey/omegacd/internal/agent"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/engine"
	"github.com/obleey/omegacd/internal/store"
)

// builtinAgent reads OMEGACD_BUILTIN_AGENT (Q26, Q35): whether omegacd server runs an Agent for its own machine, the
// Host local, and why. auto: when a container engine is reachable, never in Kubernetes.
func builtinAgent(setting string, engineErr error, kubernetes bool) (bool, string, error) {
	switch strings.ToLower(strings.TrimSpace(setting)) {
	case "true":
		return true, "OMEGACD_BUILTIN_AGENT=true", nil
	case "false":
		return false, "OMEGACD_BUILTIN_AGENT=false", nil
	case "", "auto":
		if kubernetes {
			return false, "in Kubernetes (KUBERNETES_SERVICE_HOST is set): Hosts run their own Agents", nil
		}
		if engineErr != nil {
			return false, "no container engine reachable (DOCKER_HOST or the default socket): " + engineErr.Error(), nil
		}
		return true, "a container engine is reachable", nil
	}
	return false, "", fmt.Errorf("OMEGACD_BUILTIN_AGENT must be auto, true or false, not %q", setting)
}

// loopback is where the built-in Agent reaches the Control Plane listening on addr: its own address, the loopback
// one when it listens on all of them.
func loopback(addr net.Addr) string {
	tcp := addr.(*net.TCPAddr)
	if tcp.IP.IsUnspecified() {
		return net.JoinHostPort("127.0.0.1", fmt.Sprint(tcp.Port))
	}
	return tcp.String()
}

// startBuiltinAgent runs the Agent of the Host local in this process (M4-04, Q88): its keys, database and cache in
// <data>/agent, its work dir the Control Plane's. It is enrolled and approved before it connects, so there is no
// enrollment step; it connects like any Agent, over loopback. euid is this process's user. The returned func closes
// what it opened.
func startBuiltinAgent(ctx context.Context, log *slog.Logger, st *store.Store, dataDir, workDir, url string, detected agent.EngineInfo, euid int) (func(), error) {
	state := filepath.Join(dataDir, "agent")
	if err := os.MkdirAll(state, 0o700); err != nil { // keys live here: owner-only
		return nil, err
	}
	db, err := agent.OpenDB(filepath.Join(state, "agent.db"))
	if err != nil {
		return nil, err
	}
	closeAll := func() { db.Close() }
	cache, err := agent.OpenCache(db, filepath.Join(state, "cache"), 10)
	if err != nil {
		closeAll()
		return nil, err
	}
	hostKey, err := agent.HostKey(state)
	if err != nil {
		closeAll()
		return nil, err
	}
	signingKey, err := agent.SigningKey(state)
	if err != nil {
		closeAll()
		return nil, err
	}
	if err := st.EnrollLocal(ctx, workDir, signingKey.Public().(ed25519.PublicKey), hostKey.Recipient().String()); err != nil {
		closeAll()
		return nil, fmt.Errorf("enroll the built-in agent: %w", err)
	}
	eng, err := engine.New(engine.Config{Log: log.With("component", "engine")})
	if err != nil { // coverage: unreachable, DOCKER_HOST was checked at start (useDockerHost): the Docker CLI fails no other way here
		closeAll()
		return nil, err
	}
	closeAll = func() { eng.Close(); db.Close() }
	alog := log.With("component", "agent")
	if euid == 0 {
		alog.Warn("the built-in agent runs as root, as the Control Plane does: it holds the container engine's socket " +
			"either way; run the Control Plane as a user in the socket's group instead (Q89)")
	}
	ag := agent.New(db, cache, agent.NewWorkDir(workDir, db, cache, alog), agent.NewSecrets(-1, agent.SOPS{HostKey: hostKey}), eng, alog)
	go ag.Connect(ctx, agent.Connection{URL: url, Key: signingKey, KeyPath: filepath.Join(state, "signing.key"),
		Engine: detected, AgeRecipient: hostKey.Recipient().String(), HostKey: hostKey})
	go ag.Run(ctx)
	alog.Info("built-in agent", "host", domain.LocalHost, "state", state, "workdir", workDir, "controlPlane", url,
		"hostKey", hostKey.Recipient().String(), "fingerprint", domain.Fingerprint(signingKey.Public().(ed25519.PublicKey)))
	return closeAll, nil
}

// useDockerHost settles the container engine before anything talks to it (M4-04): DOCKER_HOST checked, or the engine's
// socket found and set, the choice logged.
func useDockerHost(log *slog.Logger, getenv func(string) string, isSocket func(string) bool) error {
	host, others, err := engine.DockerHost(getenv, isSocket)
	switch {
	case err != nil:
		return err
	case getenv("DOCKER_HOST") != "":
		log.Info("container engine", "dockerHost", host, "from", "DOCKER_HOST")
		return nil
	case host == "":
		log.Warn("no container engine socket found (rootful or rootless Docker, Podman): trying the default; set DOCKER_HOST if the engine listens elsewhere")
		return nil
	}
	if len(others) > 0 {
		log.Info("container engine socket found, others too: set DOCKER_HOST to use another", "dockerHost", host, "others", others)
	} else {
		log.Info("container engine socket found", "dockerHost", host)
	}
	return os.Setenv("DOCKER_HOST", host)
}

func isSocket(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&fs.ModeSocket != 0
}
