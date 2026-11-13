package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	var b bytes.Buffer
	log, err := New(&b, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	ctx := With(With(context.Background(), "app", "web"), "op", 7, "app", "api") // a key again: replaced
	log.With("component", "engine").DebugContext(ctx, "kept container", "service", "db")
	log.Debug("no context") // entries without one still work
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines:\n%s", b.String())
	}
	var e map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("not one JSON object per line: %v: %s", err, lines[0])
	}
	for k, v := range map[string]any{"level": "DEBUG", "component": "engine", "app": "api", "op": float64(7), "service": "db"} {
		if e[k] != v {
			t.Errorf("%s = %v, want %v (%s)", k, e[k], v, lines[0])
		}
	}

	b.Reset()
	log, _ = New(&b, "", "")
	log.DebugContext(ctx, "hidden")
	log.InfoContext(ctx, "shown")
	if s := b.String(); strings.Contains(s, "hidden") || !strings.HasSuffix(s, "level=INFO msg=shown app=api op=7\n") {
		t.Errorf("default is info, text: %q", s)
	}
	for _, bad := range [][2]string{{"loud", ""}, {"", "xml"}} {
		if _, err := New(&b, bad[0], bad[1]); err == nil {
			t.Errorf("New(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

// Q62: whatever the code logs under a secret's name is replaced, also in the context and in groups.
func TestSecretsAreRedacted(t *testing.T) {
	var b bytes.Buffer
	log, _ := New(&b, "debug", "text")
	ctx := With(context.Background(), "apiToken", "t0k3n-ctx")
	log.With("db_password", "hunter2").InfoContext(ctx, "oops", slog.Group("cred", "secret", "s3cr3t"), "token", "abc123",
		"tokens", 3, "passwordFile", "/run/secrets/pw", "hostKey", "age1public",
		"err", errors.New("parse AGE-SECRET-KEY-1QQQ: bad"), "file", "-----BEGIN PRIVATE KEY-----\nMIIE")
	s := b.String()
	for _, leak := range []string{"hunter2", "t0k3n-ctx", "s3cr3t", "abc123", "AGE-SECRET-KEY-1QQQ", "MIIE"} {
		if strings.Contains(s, leak) {
			t.Errorf("%s logged: %s", leak, s)
		}
	}
	for _, kept := range []string{"tokens=3", "hostKey=age1public"} {
		if !strings.Contains(s, kept) {
			t.Errorf("%s redacted too: %s", kept, s)
		}
	}
}

// A logger with a group logs through the same handler, its attributes under the group.
func TestGroupRedacts(t *testing.T) {
	var b bytes.Buffer
	log, err := New(&b, "debug", "text")
	if err != nil {
		t.Fatal(err)
	}
	log.WithGroup("req").Info("login", "user", "admin")
	if !strings.Contains(b.String(), "req.user=admin") {
		t.Fatalf("log: %s", b.String())
	}
}
