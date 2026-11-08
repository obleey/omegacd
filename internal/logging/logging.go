// Package logging sets up the one logger each persona passes down (Q62): level and format from OMEGACD_LOG_LEVEL and
// OMEGACD_LOG_FORMAT, and the attributes of what's being worked on (app, host, revision, op) carried in the context,
// so an entry logged deep in the engine still says which Sync it belongs to.
package logging

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/obleey/omegacd/internal/secretkey"
)

// New returns a logger writing to w at level (debug, info, warn, error; "" is info) in format (text, json; "" is text).
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(cmp.Or(level, "info"))); err != nil {
		return nil, fmt.Errorf("OMEGACD_LOG_LEVEL=%s: want debug, info, warn or error", level)
	}
	opts := &slog.HandlerOptions{Level: l, ReplaceAttr: redact}
	switch strings.ToLower(cmp.Or(format, "text")) {
	case "text":
		return slog.New(handler{slog.NewTextHandler(w, opts)}), nil
	case "json":
		return slog.New(handler{slog.NewJSONHandler(w, opts)}), nil
	}
	return nil, fmt.Errorf("OMEGACD_LOG_FORMAT=%s: want text or json", format)
}

// redact is the safety net under "never log a secret" (Q62): a value whose key names a secret (password, token, …) is
// replaced, at any depth, whatever the code logging it meant, and so is a value holding a private key (an age identity,
// a PEM key) under any name, in an error message too. Numbers and booleans are kept: counts, flags. Public keys (age1…)
// aren't secrets: they're in every SOPS file.
func redact(_ []string, a slog.Attr) slog.Attr {
	if k := a.Value.Kind(); k == slog.KindString || k == slog.KindAny {
		v := a.Value.String()
		if secretkey.Plain(a.Key, v) || strings.Contains(v, "AGE-SECRET-KEY-") || strings.Contains(v, "PRIVATE KEY-----") {
			return slog.String(a.Key, "***")
		}
	}
	return a
}

// Discard is the logger of code given none (tests).
var Discard = slog.New(slog.DiscardHandler)

type ctxKey struct{}

// With returns ctx carrying key-value pairs that every entry logged with it gets, e.g. With(ctx, "app", name). A key
// ctx carries already gets the new value: a reconcile inside a Sync says app once.
func With(ctx context.Context, args ...any) context.Context {
	prev, _ := ctx.Value(ctxKey{}).([]any)
	all := slices.Clone(prev)
	for i := 0; i+1 < len(args); i += 2 {
		if j := slices.Index(all, args[i]); j >= 0 && j%2 == 0 {
			all[j+1] = args[i+1]
		} else {
			all = append(all, args[i], args[i+1])
		}
	}
	return context.WithValue(ctx, ctxKey{}, all)
}

// handler adds the context's attributes to each entry logged with a *Context method.
type handler struct{ slog.Handler }

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	if args, _ := ctx.Value(ctxKey{}).([]any); len(args) > 0 {
		r = r.Clone()
		r.Add(args...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h handler) WithAttrs(a []slog.Attr) slog.Handler { return handler{h.Handler.WithAttrs(a)} }
func (h handler) WithGroup(n string) slog.Handler      { return handler{h.Handler.WithGroup(n)} }

// Buffer is a log that goroutines write while a test reads it.
type Buffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *Buffer) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *Buffer) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
