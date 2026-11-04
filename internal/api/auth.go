package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
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
	maxBackoff    = 5 * time.Minute
	maxLogins     = 10000 // tracked client IPs
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

// ParseTrustedProxies parses a comma-separated list of CIDRs or bare IPs.
func ParseTrustedProxies(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for f := range strings.SplitSeq(v, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, err
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

func (s *server) trusted(a netip.Addr) bool {
	for _, p := range s.TrustedProxies {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// clientIP is RemoteAddr, or behind a trusted proxy the rightmost untrusted X-Forwarded-For entry.
func (s *server) clientIP(r *http.Request) string {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	remote := ap.Addr().Unmap()
	if !s.trusted(remote) {
		return remote.String()
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // garbage from the client side of the chain; don't trust anything left of it
		}
		if !s.trusted(a) {
			return a.Unmap().String()
		}
	}
	return remote.String()
}

func sessionHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
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
		if err != nil || !s.touchSession(r.Context(), c.Value) {
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

// touchSession reports whether token is a live session and slides its expiry, writing at most hourly.
func (s *server) touchSession(ctx context.Context, token string) bool {
	h := sessionHash(token)
	exp, err := s.Store.GetSession(ctx, h)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			slog.Error("get session", "err", err)
		}
		return false
	}
	if time.Until(exp) < sessionTTL-time.Hour {
		if err := s.Store.ExtendSession(ctx, h, time.Now().Add(sessionTTL)); err != nil {
			slog.Error("extend session", "err", err)
		}
	}
	return true
}

// recordFailure bumps ip's backoff and drops entries idle longer than maxBackoff, so escalation survives
// between attempts while memory stays bounded.
func (s *server) recordFailure(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.lastSweep) > time.Minute || len(s.logins) >= maxLogins {
		for k, v := range s.logins {
			if now.After(v.until.Add(maxBackoff)) {
				delete(s.logins, k)
			}
		}
		s.lastSweep = now
	}
	st, ok := s.logins[ip]
	if !ok && len(s.logins) >= maxLogins {
		return
	}
	st.fails++
	st.until = now.Add(min(time.Second<<min(st.fails, 9), maxBackoff))
	s.logins[ip] = st
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	ip := s.clientIP(r)
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
		s.recordFailure(ip)
		writeStatus(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	if err := s.Store.CreateSession(r.Context(), sessionHash(token), time.Now().Add(sessionTTL)); err != nil {
		writeError(w, err)
		return
	}
	s.mu.Lock()
	delete(s.logins, ip)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	writeJSON(w, http.StatusOK, map[string]bool{"mustChangePassword": mustChange})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.Store.DeleteSession(r.Context(), sessionHash(c.Value)); err != nil {
			writeError(w, err)
			return
		}
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
	keep := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		keep = sessionHash(c.Value)
	}
	if err := s.Store.DeleteSessionsExcept(r.Context(), keep); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
