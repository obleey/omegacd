package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/obleey/omegacd/internal/domain"
)

const (
	adminUser     = "admin"
	sessionCookie = "omegacd_session"
	sessionTTL    = 24 * time.Hour
	maxBackoff    = 5 * time.Minute
	maxLogins     = 10000 // tracked client IPs
	maxPassword   = 72    // bcrypt reads no more (Q148)
)

// InitialPasswordFile is the file in the data dir that holds the admin's generated first password until it is
// changed (Q300).
const InitialPasswordFile = "initial-admin-password"

// EnsureAdmin makes the admin account at the first start: a generated password, written to file (mode 0600) and never
// logged, to be changed at the first login (Q298, Q300). While file is there, every start says where it is (Q312).
func EnsureAdmin(ctx context.Context, store domain.Store, file string, log *slog.Logger) error {
	_, _, err := store.AdminPasswordHash(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		err = newAdminPassword(ctx, store, file)
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err != nil {
		log.Debug("no initial admin password file: the admin's password was set", "path", file, "err", err)
		return nil
	}
	log.Warn("the initial admin password is in this file: log in as admin and change it", "username", adminUser, "path", file)
	return nil
}

// ResetAdminPassword writes a new generated password to file and requires it changed at the next login, ending every
// session; API tokens keep working (Q311).
func ResetAdminPassword(ctx context.Context, store domain.Store, file string) error {
	if err := newAdminPassword(ctx, store, file); err != nil {
		return err
	}
	return store.DeleteSessionsExcept(ctx, "")
}

func newAdminPassword(ctx context.Context, store domain.Store, file string) error {
	password := rand.Text()[:20]
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil { // coverage: unreachable, bcrypt refuses only a password over its 72 bytes, and this has 20
		return err
	}
	// the file first: a password in the database that nobody can read would lock the admin out
	if err := os.WriteFile(file, []byte(password+"\n"), 0o600); err != nil {
		return fmt.Errorf("write the initial admin password: %w", err)
	}
	if err := os.Chmod(file, 0o600); err != nil { // coverage: unreachable, the file was just written by this user
		return err
	}
	return store.SetAdminPassword(ctx, string(hash), true)
}

type loginState struct {
	fails int
	until time.Time
}

// ParseTrustedProxies parses a comma-separated list of CIDRs or bare IPs.
func ParseTrustedProxies(v string) ([]netip.Prefix, error) {
	return domain.ParsePrefixes(strings.Split(v, ","))
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
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Requested-With") != "omegacd" {
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
			s.Log.ErrorContext(ctx, "get session", "err", err)
		}
		return false
	}
	if time.Until(exp) < sessionTTL-time.Hour {
		if err := s.Store.ExtendSession(ctx, h, time.Now().Add(sessionTTL)); err != nil {
			s.Log.ErrorContext(ctx, "extend session", "err", err)
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
	if len(req.NewPassword) > maxPassword { // Q148
		writeError(w, fmt.Errorf("%w: new password must be at most %d bytes", domain.ErrInvalid, maxPassword))
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
	if err != nil { // coverage: unreachable, bcrypt refuses only a password over its 72 bytes, checked above
		writeError(w, err)
		return
	}
	if err := s.Store.SetAdminPassword(r.Context(), string(newHash), false); err != nil {
		writeError(w, err)
		return
	}
	if s.InitialPasswordFile != "" { // the first password is no use any more (Q300); the change counts anyway (Q313)
		if err := os.Remove(s.InitialPasswordFile); err == nil {
			s.Log.InfoContext(r.Context(), "admin password changed: the initial password file is deleted", "path", s.InitialPasswordFile)
		} else if !errors.Is(err, fs.ErrNotExist) {
			s.Log.ErrorContext(r.Context(), "admin password changed, but the initial password file is left: delete it", "path", s.InitialPasswordFile, "err", err)
		}
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
