package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/obleey/dockercd/internal/domain"
)

const (
	adminUser     = "admin"
	sessionCookie = "dockercd_session"
	sessionTTL    = 24 * time.Hour
)

// EnsureAdmin initialises the admin account on first start.
func EnsureAdmin(ctx context.Context, store domain.Store, initialPassword string, log *slog.Logger) error {
	_, _, err := store.AdminPasswordHash(ctx)
	if err == nil || !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	generated := initialPassword == ""
	if generated {
		initialPassword = rand.Text()[:20]
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(initialPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := store.SetAdminPassword(ctx, string(hash), generated); err != nil {
		return err
	}
	if generated {
		log.Warn("initial admin password", "username", adminUser, "password", initialPassword)
	}
	return nil
}

type loginState struct {
	fails int
	until time.Time
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if isPublic(p) {
			next.ServeHTTP(w, r)
			return
		}
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if _, err := s.Store.VerifyToken(r.Context(), tok); err != nil {
				writeStatus(w, http.StatusUnauthorized, "invalid token")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.touchSession(c.Value) {
			writeStatus(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Requested-With") != "dockercd" {
			writeStatus(w, http.StatusForbidden, "missing X-Requested-With header")
			return
		}
		if !strings.HasPrefix(p, "/api/auth/") {
			_, mustChange, err := s.Store.AdminPasswordHash(r.Context())
			if err != nil {
				writeError(w, err)
				return
			}
			if mustChange {
				writeStatus(w, http.StatusForbidden, "password change required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// touchSession reports whether token is a live session and extends it.
func (s *server) touchSession(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok || time.Now().After(exp) {
		delete(s.sessions, token)
		return false
	}
	s.sessions[token] = time.Now().Add(sessionTTL)
	return true
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	ip := clientIP(r)
	// shortcut: in-memory per-IP backoff, never pruned and unaware of proxies; fine for one admin, upgrade if exposed publicly.
	s.mu.Lock()
	st := s.logins[ip]
	s.mu.Unlock()
	if time.Now().Before(st.until) {
		writeStatus(w, http.StatusTooManyRequests, "too many login attempts, retry later")
		return
	}
	hash, mustChange, err := s.Store.AdminPasswordHash(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(adminUser)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) == nil
	if !userOK || !passOK {
		st.fails++
		st.until = time.Now().Add(min(time.Second<<min(st.fails, 9), 5*time.Minute))
		s.mu.Lock()
		s.logins[ip] = st
		s.mu.Unlock()
		writeStatus(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	s.mu.Lock()
	delete(s.logins, ip)
	s.sessions[token] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	writeJSON(w, http.StatusOK, map[string]bool{"mustChangePassword": mustChange})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	_, mustChange, err := s.Store.AdminPasswordHash(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": adminUser, "mustChangePassword": mustChange})
}

func (s *server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req struct{ CurrentPassword, NewPassword string }
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if len(req.NewPassword) < 8 || req.NewPassword == req.CurrentPassword {
		writeError(w, fmt.Errorf("%w: new password must be at least 8 characters and differ from the current one", domain.ErrInvalid))
		return
	}
	hash, _, err := s.Store.AdminPasswordHash(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.CurrentPassword)) != nil {
		writeStatus(w, http.StatusBadRequest, "current password is wrong")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.Store.SetAdminPassword(r.Context(), string(newHash), false); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
