// Package api serves the omegacd HTTP API, webhooks, metrics and the embedded UI.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/obleey/omegacd/internal/version"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/obleey/omegacd/internal/domain"
)

type Config struct {
	Controller    domain.Controller
	Store         domain.Store
	WebhookSecret string
	WebhookParse  func(provider string, h http.Header, body []byte, secret string) ([]string, error)
	UI            fs.FS
	// TrustedProxies may set X-Forwarded-For; see ParseTrustedProxies.
	TrustedProxies []netip.Prefix
}

type server struct {
	Config
	mu        sync.Mutex
	logins    map[string]loginState // client IP → failed login backoff
	lastSweep time.Time
}

func New(cfg Config) http.Handler {
	s := &server{Config: cfg, logins: map[string]loginState{}}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("GET /api/auth/me", s.me)
	mux.HandleFunc("POST /api/auth/password", s.changePassword)

	mux.HandleFunc("GET /api/apps", s.listApps)
	mux.HandleFunc("POST /api/apps", s.createApp)
	mux.HandleFunc("GET /api/apps/{name}", s.getApp)
	mux.HandleFunc("PUT /api/apps/{name}", s.updateApp)
	mux.HandleFunc("DELETE /api/apps/{name}", s.deleteApp)
	mux.HandleFunc("POST /api/apps/{name}/sync", s.syncApp)
	mux.HandleFunc("POST /api/apps/{name}/rollback", s.rollbackApp)
	mux.HandleFunc("POST /api/apps/{name}/refresh", s.refreshApp)
	mux.HandleFunc("GET /api/apps/{name}/history", s.history)
	mux.HandleFunc("GET /api/apps/{name}/diff", s.diff)
	mux.HandleFunc("GET /api/apps/{name}/resources", s.resources)
	mux.HandleFunc("GET /api/apps/{name}/resource", s.resource)
	mux.HandleFunc("GET /api/apps/{name}/export", s.export)
	mux.HandleFunc("GET /api/apps/{name}/logs/{container}", s.logs)
	mux.HandleFunc("GET /api/events", s.events)

	mux.HandleFunc("POST /api/repos/check", s.checkRepo)

	mux.HandleFunc("GET /api/settings/repo-credentials", s.listRepoCreds)
	mux.HandleFunc("POST /api/settings/repo-credentials", s.saveRepoCred)
	mux.HandleFunc("PUT /api/settings/repo-credentials/{id}", s.saveRepoCred)
	mux.HandleFunc("DELETE /api/settings/repo-credentials/{id}", s.deleteRepoCred)
	mux.HandleFunc("GET /api/settings/registry-credentials", s.listRegistryCreds)
	mux.HandleFunc("POST /api/settings/registry-credentials", s.saveRegistryCred)
	mux.HandleFunc("PUT /api/settings/registry-credentials/{id}", s.saveRegistryCred)
	mux.HandleFunc("DELETE /api/settings/registry-credentials/{id}", s.deleteRegistryCred)
	mux.HandleFunc("GET /api/settings/hosts", s.listHosts)
	mux.HandleFunc("POST /api/settings/hosts", s.saveHost)
	mux.HandleFunc("PUT /api/settings/hosts/{name}", s.saveHost)
	mux.HandleFunc("DELETE /api/settings/hosts/{name}", s.deleteHost)
	mux.HandleFunc("POST /api/settings/hosts/{name}/test", s.testHost)
	mux.HandleFunc("GET /api/settings/hosts/{name}/keys", s.hostKeys)
	mux.HandleFunc("POST /api/settings/hosts/{name}/keys/generate", s.generateHostKey)
	mux.HandleFunc("POST /api/settings/hosts/{name}/keys/import", s.importHostKey)
	mux.HandleFunc("DELETE /api/settings/hosts/{name}/keys/{recipient}", s.removeHostKey)
	mux.HandleFunc("GET /api/settings/tokens", s.listTokens)
	mux.HandleFunc("POST /api/settings/tokens", s.createToken)
	mux.HandleFunc("DELETE /api/settings/tokens/{id}", s.deleteToken)

	mux.HandleFunc("POST /api/webhooks/{provider}", s.webhook)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, fmt.Errorf("%s %s: %w", r.Method, r.URL.Path, domain.ErrNotFound))
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	// public, like /healthz: the version is printed on the login page and by `omegacd version`
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": version.String()})
	})
	mux.Handle("GET /metrics", metricsHandler(cfg.Controller))
	mux.Handle("/", uiHandler(cfg.UI))

	return logRequests(s.auth(mux))
}

func isPublic(p string) bool {
	return !strings.HasPrefix(p, "/api/") || p == "/api/auth/login" || p == "/api/version" || strings.HasPrefix(p, "/api/webhooks/")
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, domain.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, domain.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, domain.ErrAlreadyExists), errors.Is(err, domain.ErrUnmanaged), errors.Is(err, domain.ErrBusy):
		status = http.StatusConflict
	default:
		slog.Error("api error", "err", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeStatus(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("%w: request body: %v", domain.ErrInvalid, err)
	}
	return nil
}

const placeholder = `<!doctype html><title>omegacd</title><p>omegacd is running. The UI is not embedded in this build; use the <a href="/api/apps">API</a>.</p>`

func uiHandler(ui fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeStatus(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if ui == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(placeholder))
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if st, err := fs.Stat(ui, name); name == "" || err != nil || st.IsDir() {
			name = "index.html"
		}
		if strings.HasPrefix(r.URL.Path, "/_app/immutable/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.ServeFileFS(w, r, ui, name)
	})
}
