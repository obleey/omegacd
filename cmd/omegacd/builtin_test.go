package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/agent"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/store"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestBuiltinAgentSetting(t *testing.T) {
	noEngine := errors.New("no socket")
	for _, tc := range []struct {
		setting    string
		engine     error
		kubernetes bool
		on         bool
		why        string
	}{
		{"", nil, false, true, "a container engine is reachable"},
		{" AUTO ", noEngine, false, false, "no container engine reachable"},
		{"auto", nil, true, false, "in Kubernetes"},
		{"true", noEngine, true, true, "OMEGACD_BUILTIN_AGENT=true"},
		{"false", nil, false, false, "OMEGACD_BUILTIN_AGENT=false"},
	} {
		on, why, err := builtinAgent(tc.setting, tc.engine, tc.kubernetes)
		if err != nil || on != tc.on || !strings.Contains(why, tc.why) {
			t.Errorf("%q: %v %q %v", tc.setting, on, why, err)
		}
	}
	if _, _, err := builtinAgent("yes", nil, false); err == nil {
		t.Error("yes accepted")
	}
}

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]string{"0.0.0.0:8080": "127.0.0.1:8080", "[::]:8080": "127.0.0.1:8080", "10.0.0.5:8080": "10.0.0.5:8080"} {
		a, _ := net.ResolveTCPAddr("tcp", addr)
		if got := loopback(a); got != want {
			t.Errorf("%s: %s", addr, got)
		}
	}
}

// M4-04: the built-in Agent enrolls the Host local and starts, warning when it runs as root; every way it can't is
// an error naming what failed, its keys and database left as they were.
func TestStartBuiltinAgent(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := &lockedBuffer{} // the Agent it starts keeps logging
	log := slog.New(slog.NewTextHandler(logs, nil))
	start := func(data string, euid int) error {
		done, err := startBuiltinAgent(ctx, log, st, data, t.TempDir(), "http://127.0.0.1:1", agent.EngineInfo{}, euid)
		if err == nil {
			done()
		}
		return err
	}

	data := t.TempDir()
	if err := start(data, 0); err != nil {
		t.Fatal(err)
	}
	h, err := st.GetHost(ctx, domain.LocalHost)
	if err != nil || !h.Approved || h.Fingerprint == "" || !strings.HasPrefix(h.AgeRecipient, "age1") {
		t.Fatalf("local %+v %v", h, err)
	}
	if !strings.Contains(logs.String(), "runs as root") || !strings.Contains(logs.String(), "msg=\"built-in agent\"") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	if err := start(data, 1000); err != nil { // a restart: the same keys
		t.Fatal(err)
	}
	if again, _ := st.GetHost(ctx, domain.LocalHost); again.Fingerprint != h.Fingerprint {
		t.Fatal("a restart changed the key")
	}

	broken := func(make func(state string)) string {
		data := t.TempDir()
		state := filepath.Join(data, "agent")
		os.MkdirAll(state, 0o700)
		make(state)
		return data
	}
	file := filepath.Join(t.TempDir(), "a-file")
	os.WriteFile(file, nil, 0o600)
	for name, data := range map[string]string{
		"data dir a file":     file,
		"database a folder":   broken(func(s string) { os.MkdirAll(filepath.Join(s, "agent.db", "x"), 0o700) }),
		"cache a file":        broken(func(s string) { os.WriteFile(filepath.Join(s, "cache"), nil, 0o600) }),
		"Host Key not a key":  broken(func(s string) { os.WriteFile(filepath.Join(s, "host.key"), []byte("nope"), 0o600) }),
		"signing key exposed": broken(func(s string) { os.WriteFile(filepath.Join(s, "signing.key"), []byte("x"), 0o644) }),
	} {
		if err := start(data, 1000); err == nil {
			t.Errorf("%s: started", name)
		}
	}

	st.Close()
	if err := start(t.TempDir(), 1000); err == nil || !strings.Contains(err.Error(), "enroll the built-in agent") {
		t.Errorf("enrolled without a database: %v", err)
	}
}
