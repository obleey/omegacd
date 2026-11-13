package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/obleey/omegacd/internal/version"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"filippo.io/age"
	"golang.org/x/crypto/bcrypt"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	"github.com/obleey/omegacd/internal/store"
	"github.com/obleey/omegacd/internal/wire"
)

type fakeStore struct {
	domain.Store // unimplemented methods panic
	mu           sync.Mutex
	backupErr    error // the backup fails with it
	hash         string
	mustChange   bool
	tokens       map[string]domain.APIToken
	sessions     map[string]time.Time
}

type fakeSource struct {
	domain.Source // unimplemented methods panic
	creds         []domain.RepoCredential
	err           error
}

func (s *fakeSource) ListCredentials(context.Context) ([]domain.RepoCredential, error) {
	return s.creds, s.err
}

func (s *fakeStore) CreateSession(_ context.Context, h string, exp time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[h] = exp
	return nil
}

func (s *fakeStore) GetSession(_ context.Context, h string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[h]
	if !ok || time.Now().After(exp) {
		return time.Time{}, domain.ErrNotFound
	}
	return exp, nil
}

func (s *fakeStore) ExtendSession(_ context.Context, h string, exp time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[h]; !ok {
		return domain.ErrNotFound
	}
	s.sessions[h] = exp
	return nil
}

func (s *fakeStore) DeleteSession(_ context.Context, h string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, h)
	return nil
}

func (s *fakeStore) DeleteSessionsExcept(_ context.Context, keep string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h := range s.sessions {
		if h != keep {
			delete(s.sessions, h)
		}
	}
	return nil
}

func (s *fakeStore) AdminPasswordHash(context.Context) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hash == "" {
		return "", false, domain.ErrNotFound
	}
	return s.hash, s.mustChange, nil
}

func (s *fakeStore) SetAdminPassword(_ context.Context, hash string, mustChange bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hash, s.mustChange = hash, mustChange
	return nil
}

func (s *fakeStore) CreateToken(_ context.Context, name string) (domain.APIToken, string, error) {
	t := domain.APIToken{ID: int64(len(s.tokens) + 1), Name: name}
	plain := "tok-" + name
	s.tokens[plain] = t
	return t, plain, nil
}

func (s *fakeStore) VerifyToken(_ context.Context, plain string) (domain.APIToken, error) {
	if t, ok := s.tokens[plain]; ok {
		return t, nil
	}
	return domain.APIToken{}, domain.ErrNotFound
}

type fakeController struct {
	domain.Controller
	mu         sync.Mutex
	apps       map[string]domain.AppView
	events     chan domain.Event
	changed    chan string
	hosts      map[string]domain.Host
	lastSync   domain.SyncRequest
	lastDelete [2]bool // cascade, volumes
}

func (c *fakeController) ListHosts(context.Context) ([]domain.HostView, error) {
	var out []domain.HostView
	for _, h := range c.hosts {
		out = append(out, domain.HostView{Host: h})
	}
	return out, nil
}

func (c *fakeController) DeleteHost(_ context.Context, name string) error {
	if name == "busy" {
		return fmt.Errorf("%w: host %q is used by Applications web", domain.ErrInvalid, name)
	}
	delete(c.hosts, name)
	return nil
}

func (c *fakeController) HostKeys(_ context.Context, host string) ([]domain.HostKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.hosts[host]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return []domain.HostKey{{Recipient: h.AgeRecipient, Current: true}}, nil
}

func (c *fakeController) ListApps(context.Context) ([]domain.AppView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []domain.AppView
	for _, a := range c.apps {
		out = append(out, a)
	}
	return out, nil
}

func (c *fakeController) GetApp(_ context.Context, name string) (domain.AppView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.apps[name]
	if !ok {
		return a, domain.ErrNotFound
	}
	return a, nil
}

func (c *fakeController) CreateApp(_ context.Context, a domain.Application) (domain.AppView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.apps[a.Name]; ok {
		return domain.AppView{}, domain.ErrAlreadyExists
	}
	v := domain.AppView{Application: a, SyncStatus: domain.OutOfSync}
	c.apps[a.Name] = v
	return v, nil
}

func (c *fakeController) ForgetApp(ctx context.Context, name string) error {
	if _, err := c.GetApp(ctx, name); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.apps, name)
	return nil
}

func (c *fakeController) DeleteApp(ctx context.Context, name string, cascade, volumes bool) error {
	if _, err := c.GetApp(ctx, name); err != nil {
		return err
	}
	if name == "offline" {
		return fmt.Errorf("%w edge: its Agent removes offline when it connects", domain.ErrWaitingForHost)
	}
	c.mu.Lock()
	c.lastDelete = [2]bool{cascade, volumes}
	c.mu.Unlock()
	delete(c.apps, name)
	return nil
}

func (c *fakeController) Sync(ctx context.Context, name string, req domain.SyncRequest) (int64, error) {
	if _, err := c.GetApp(ctx, name); err != nil {
		return 0, err
	}
	c.lastSync = req
	return 7, nil
}

func (c *fakeController) Rollback(_ context.Context, name string, opID int64) (int64, error) {
	return opID + 1, nil
}

func (c *fakeController) ResourceDetail(_ context.Context, name, kind, res string) (domain.ResourceDetail, error) {
	if name != "web" || kind != "config" || res != "nginx.conf" {
		return domain.ResourceDetail{}, domain.ErrNotFound
	}
	return domain.ResourceDetail{Resource: domain.Resource{Kind: kind, Name: res}, Content: "server {}"}, nil
}

func (c *fakeController) CheckRepo(_ context.Context, url string) (domain.RepoAccess, error) {
	if url == "" {
		return domain.RepoAccess{}, domain.ErrInvalid
	}
	return domain.RepoAccess{Access: "credential", CredentialPrefix: "https://github.com/obleey/", DefaultBranch: "master"}, nil
}

func (c *fakeController) RepoChanged(_ context.Context, url string) { c.changed <- url }

func (c *fakeController) Subscribe() (<-chan domain.Event, func()) { return c.events, func() {} }

type env struct {
	t     *testing.T
	srv   *httptest.Server
	store *fakeStore
	src   *fakeSource
	ctrl  *fakeController
	c     *http.Client
	log   *strings.Builder // ponytail: unlocked, read only after the request it logged returned
}

func newEnv(t *testing.T, mustChange bool) *env {
	store := &fakeStore{tokens: map[string]domain.APIToken{}, sessions: map[string]time.Time{}}
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.MinCost)
	store.hash, store.mustChange = string(hash), mustChange
	ctrl := &fakeController{apps: map[string]domain.AppView{}, events: make(chan domain.Event, 1), changed: make(chan string, 4), hosts: map[string]domain.Host{}}
	src := &fakeSource{}
	var log strings.Builder
	logger, _ := logging.New(&log, "debug", "text")
	h := New(Config{
		Log:        logger,
		Controller: ctrl, Store: store, Source: src, WebhookSecret: "s3", Agents: agents.New(store, logger),
		Backup: func(ctx context.Context, w io.Writer) error {
			if store.backupErr != nil {
				return store.backupErr
			}
			_, err := io.WriteString(w, "the tar")
			return err
		},
		WebhookParse: func(provider string, h http.Header, body []byte, secret string) ([]string, error) {
			if h.Get("X-Sig") != secret {
				return nil, errors.New("bad signature")
			}
			if string(body) == "not its format" {
				return nil, fmt.Errorf("%w: not its format", domain.ErrInvalid)
			}
			return []string{"https://example.com/r.git"}, nil
		},
		UI: fstest.MapFS{
			"index.html":            {Data: []byte("INDEX")},
			"_app/immutable/app.js": {Data: []byte("JS")},
			"_app/immutable/big.js": {Data: []byte(strings.Repeat("export const greeting = 'hello';\n", 500))},
			"favicon.svg":           {Data: []byte("<svg/>")},
			"logo.png":              {Data: []byte("\x89PNG")},
		},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &env{t: t, srv: srv, store: store, src: src, ctrl: ctrl, c: &http.Client{Jar: jar}, log: &log}
}

// do sends a request; hdr pairs are header key/values. Returns status and body.
func (e *env) do(method, path, body string, hdr ...string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "omegacd")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) expect(method, path, body string, want int, hdr ...string) string {
	e.t.Helper()
	got, b := e.do(method, path, body, hdr...)
	if got != want {
		e.t.Fatalf("%s %s: status %d, want %d (body %s)", method, path, got, want, b)
	}
	return b
}

func (e *env) login() {
	e.expect("POST", "/api/auth/login", `{"username":"admin","password":"secret123"}`, 200)
}

func TestLoginLogoutMe(t *testing.T) {
	e := newEnv(t, false)
	e.expect("GET", "/api/auth/me", "", 401)
	e.login()
	if b := e.expect("GET", "/api/auth/me", "", 200); !strings.Contains(b, `"username":"admin"`) {
		t.Fatal(b)
	}
	e.expect("POST", "/api/auth/logout", "", 204)
	e.expect("GET", "/api/auth/me", "", 401)

	e.expect("POST", "/api/auth/login", `{"username":"admin","password":"nope"}`, 401)
	e.expect("POST", "/api/auth/login", `{"username":"admin","password":"secret123"}`, 429)
}

func TestMustChangePassword(t *testing.T) {
	e := newEnv(t, true)
	if b := e.expect("POST", "/api/auth/login", `{"username":"admin","password":"secret123"}`, 200); !strings.Contains(b, `"mustChangePassword":true`) {
		t.Fatal(b)
	}
	if b := e.expect("GET", "/api/apps", "", 403); !strings.Contains(b, "password change required") {
		t.Fatal(b)
	}
	e.expect("GET", "/api/auth/me", "", 200)
	e.expect("POST", "/api/auth/password", `{"currentPassword":"wrong","newPassword":"newsecret1"}`, 400)
	e.expect("POST", "/api/auth/password", `{"currentPassword":"secret123","newPassword":"newsecret1"}`, 204)
	e.expect("GET", "/api/apps", "", 200)
}

func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	other := *e
	jar, _ := cookiejar.New(nil)
	other.c = &http.Client{Jar: jar}
	other.login()
	e.expect("POST", "/api/auth/password", `{"currentPassword":"secret123","newPassword":"newsecret1"}`, 204)
	e.expect("GET", "/api/auth/me", "", 200)
	other.expect("GET", "/api/auth/me", "", 401)
}

func TestSessionSlidingExpiry(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	var h string
	for k := range e.store.sessions {
		h = k
	}
	if len(h) != 64 {
		t.Fatalf("session key %q is not a sha256 hex", h)
	}
	fresh := e.store.sessions[h]
	e.expect("GET", "/api/auth/me", "", 200)
	if !e.store.sessions[h].Equal(fresh) {
		t.Fatal("fresh session was rewritten")
	}
	e.store.sessions[h] = time.Now().Add(time.Hour)
	e.expect("GET", "/api/auth/me", "", 200)
	if time.Until(e.store.sessions[h]) < 23*time.Hour {
		t.Fatal("stale session was not extended")
	}
}

func TestBearerToken(t *testing.T) {
	e := newEnv(t, false)
	e.store.CreateToken(context.Background(), "ci")
	e.expect("GET", "/api/apps", "", 401, "Authorization", "Bearer wrong")
	e.expect("GET", "/api/apps", "", 200, "Authorization", "Bearer tok-ci")
	// bearer requests are CSRF-exempt
	e.expect("POST", "/api/apps", `{"name":"a"}`, 201, "Authorization", "Bearer tok-ci", "X-Requested-With", "")
}

func TestCSRFHeader(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	e.expect("POST", "/api/apps", `{"name":"a"}`, 403, "X-Requested-With", "")
	e.expect("POST", "/api/apps", `{"name":"a"}`, 201)
}

func TestAppsCRUD(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	e.expect("POST", "/api/apps", `{"name":"web","repoURL":"https://x/r.git","adopt":true}`, 201)
	e.expect("POST", "/api/apps", `{"name":"web"}`, 409)
	e.expect("POST", "/api/apps", `{bad`, 400)
	if b := e.expect("GET", "/api/apps/web", "", 200); !strings.Contains(b, `"syncStatus":"OutOfSync"`) {
		t.Fatal(b)
	}
	if b := e.expect("GET", "/api/apps/web/export", "", 200); !strings.Contains(b, "repoURL: https://x/r.git") || strings.Contains(b, "adopt") {
		t.Fatal(b)
	}
	e.expect("GET", "/api/apps/nope", "", 404)
	if b := e.expect("POST", "/api/apps/web/sync", "", 202); !strings.Contains(b, `"opID":7`) || e.ctrl.lastSync.Prune != nil {
		t.Fatal(b)
	}
	e.expect("POST", "/api/apps/web/sync", `{"prune":true}`, 202)
	if p := e.ctrl.lastSync.Prune; p == nil || !*p {
		t.Fatalf("one-off prune not passed: %v", p)
	}
	e.expect("POST", "/api/apps/web/sync", `{bad`, 400)
	if b := e.expect("POST", "/api/apps/web/rollback", `{"opID":3}`, 202); !strings.Contains(b, `"opID":4`) {
		t.Fatal(b)
	}
	e.expect("POST", "/api/apps/nope/sync", "", 404)
	e.expect("DELETE", "/api/apps/web?cascade=true&volumes=true", "", 204)
	if e.ctrl.lastDelete != [2]bool{true, true} {
		t.Fatalf("cascade, volumes = %v", e.ctrl.lastDelete)
	}
	e.expect("DELETE", "/api/apps/web", "", 404)
	e.expect("GET", "/api/nothing", "", 404)
}

func TestResourceDetail(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	if b := e.expect("GET", "/api/apps/web/resource?kind=config&name=nginx.conf", "", 200); !strings.Contains(b, `"content":"server {}"`) {
		t.Fatal(b)
	}
	e.expect("GET", "/api/apps/web/resource?kind=config&name=..%2Fetc%2Fpasswd", "", 404)
}

func TestEventsSSE(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/events", nil)
	resp, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatal(ct)
	}
	e.ctrl.events <- domain.Event{Type: "app-deleted", App: "web", Data: "web"}
	sc := bufio.NewScanner(resp.Body)
	var got []string
	deadline := time.AfterFunc(5*time.Second, func() { resp.Body.Close() })
	defer deadline.Stop()
	for sc.Scan() {
		got = append(got, sc.Text())
		if strings.HasPrefix(sc.Text(), "data: ") {
			break
		}
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "event: app-deleted\ndata: ") {
		t.Fatal(joined)
	}
	var ev domain.Event
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got[len(got)-1], "data: ")), &ev); err != nil || ev.App != "web" {
		t.Fatal(ev, err)
	}
}

func TestWebhook(t *testing.T) {
	e := newEnv(t, false)
	e.expect("POST", "/api/webhooks/github", "{}", 401, "X-Sig", "wrong")
	e.expect("POST", "/api/webhooks/github", "{}", 202, "X-Sig", "s3")
	select {
	case u := <-e.ctrl.changed:
		if u != "https://example.com/r.git" {
			t.Fatal(u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RepoChanged not called")
	}
	e.expect("POST", "/api/webhooks/bitbucket", "{}", 404)
	e.expect("POST", "/api/webhooks/generic", "not its format", 400, "X-Sig", "s3")
	e.expect("POST", "/api/webhooks/generic", "{}", 202, "X-Sig", "s3")
}

func TestSPAFallback(t *testing.T) {
	e := newEnv(t, false)
	if b := e.expect("GET", "/apps/web", "", 200); b != "INDEX" {
		t.Fatal(b)
	}
	resp, err := http.Get(e.srv.URL + "/_app/immutable/app.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatal(cc)
	}
	e.expect("GET", "/healthz", "", 200)
	e.expect("GET", "/metrics", "", 200)
}

func TestUIGzip(t *testing.T) {
	e := newEnv(t, false)
	get := func(method, path, accept string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept) // set by hand, so the client does not decompress on its own
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	want := strings.Repeat("export const greeting = 'hello';\n", 500)

	resp, body := get("GET", "/_app/immutable/big.js", "br, gzip;q=0.8")
	if resp.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("headers: %v", resp.Header)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if string(plain) != want || len(body) >= len(want)/10 || resp.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("gzip body: %d bytes for %d, round trip ok=%v", len(body), len(want), string(plain) == want)
	}
	if _, again := get("GET", "/_app/immutable/big.js", "gzip"); !bytes.Equal(again, body) {
		t.Fatal("second response differs (cache)")
	}
	// the SPA fallback is compressed too
	if resp, _ := get("GET", "/apps/web", "gzip"); resp.Header.Get("Content-Encoding") != "gzip" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("index: %v", resp.Header)
	}
	// HEAD: headers only
	if resp, b := get("HEAD", "/_app/immutable/big.js", "gzip"); resp.Header.Get("Content-Encoding") != "gzip" || len(b) != 0 {
		t.Fatalf("HEAD: %v, %d bytes", resp.Header, len(b))
	}
	// plain when not accepted or refused, and never for images or the API
	for _, c := range []struct{ path, accept string }{
		{"/_app/immutable/big.js", ""}, {"/_app/immutable/big.js", "gzip;q=0"}, {"/_app/immutable/big.js", "br"},
		{"/logo.png", "gzip"}, {"/api/version", "gzip"},
	} {
		resp, b := get("GET", c.path, c.accept)
		if resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s with %q: compressed", c.path, c.accept)
		}
		if c.path == "/_app/immutable/big.js" && string(b) != want {
			t.Errorf("%s with %q: body differs", c.path, c.accept)
		}
	}
}

func TestAcceptsGzip(t *testing.T) {
	for h, want := range map[string]bool{
		"gzip": true, "gzip, deflate, br": true, "br;q=1.0, gzip;q=0.8": true, "*": true, "GZIP": true, "gzip;q=0.001": true,
		"": false, "br": false, "gzip;q=0": false, "gzip; q=0.0": false, "identity": false, "x-gzip": false,
	} {
		if got := acceptsGzip(h); got != want {
			t.Errorf("%q: %v, want %v", h, got, want)
		}
	}
}

func TestEnsureAdmin(t *testing.T) {
	s := &fakeStore{}
	if err := EnsureAdmin(context.Background(), s, "", slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if s.hash == "" || !s.mustChange {
		t.Fatal("expected generated password with mustChange")
	}
	old := s.hash
	EnsureAdmin(context.Background(), s, "other", slog.New(slog.DiscardHandler))
	if s.hash != old {
		t.Fatal("existing admin overwritten")
	}
}

func TestClientIP(t *testing.T) {
	trusted, err := ParseTrustedProxies(" 10.0.0.0/8, 192.168.1.1 ,::1")
	if err != nil {
		t.Fatal(err)
	}
	if len(trusted) != 3 || trusted[1].Bits() != 32 || trusted[2].Bits() != 128 {
		t.Fatal(trusted)
	}
	if _, err := ParseTrustedProxies("10.0.0.0/8,nope"); err == nil {
		t.Fatal("want error")
	}
	s := &server{Config: Config{TrustedProxies: trusted}}
	for _, tc := range []struct{ remote, xff, want string }{
		{"203.0.113.9:1234", "1.2.3.4", "203.0.113.9"},                // untrusted peer: XFF ignored
		{"10.1.2.3:1234", "1.2.3.4", "1.2.3.4"},                       // trusted proxy
		{"10.1.2.3:1234", "6.6.6.6, 1.2.3.4, 192.168.1.1", "1.2.3.4"}, // spoofed left-most entry ignored
		{"10.1.2.3:1234", "", "10.1.2.3"},
		{"10.1.2.3:1234", "10.9.9.9", "10.1.2.3"},
		{"[::1]:1234", "2001:db8::1", "2001:db8::1"},
	} {
		r := httptest.NewRequest("POST", "/api/auth/login", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := s.clientIP(r); got != tc.want {
			t.Errorf("%s %q: got %s, want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
}

func TestLoginBackoffPruned(t *testing.T) {
	s := &server{logins: map[string]loginState{
		"old":    {fails: 3, until: time.Now().Add(-maxBackoff - time.Second)},
		"recent": {fails: 3, until: time.Now().Add(-time.Second)},
	}}
	s.recordFailure("new")
	if _, ok := s.logins["old"]; ok {
		t.Fatal("idle entry not pruned")
	}
	if s.logins["recent"].fails != 3 || s.logins["new"].fails != 1 {
		t.Fatal(s.logins)
	}
	for i := range maxLogins + 10 {
		s.recordFailure(strconv.Itoa(i))
	}
	if len(s.logins) > maxLogins {
		t.Fatalf("%d entries, want <= %d", len(s.logins), maxLogins)
	}
}

func TestHostsAPI(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	e.ctrl.hosts["prod"] = domain.Host{Name: "prod", AgeRecipient: "age1prod"}
	if b := e.expect("GET", "/api/settings/hosts", "", 200); !strings.Contains(b, `"name":"prod"`) || !strings.Contains(b, `"ageRecipient":"age1prod"`) {
		t.Fatal(b)
	}
	// push Hosts are gone (Q107): a Host is added by inviting its Agent
	e.expect("POST", "/api/settings/hosts", `{"name":"x","url":"ssh://deploy@x"}`, 404)
	e.expect("POST", "/api/settings/hosts/prod/test", "", 404)
	if b := e.expect("DELETE", "/api/settings/hosts/busy", "", 400); !strings.Contains(b, "web") {
		t.Fatal(b)
	}
	e.expect("DELETE", "/api/settings/hosts/prod", "", 204)
}

func (c *fakeController) InviteHost(_ context.Context, name string) (string, time.Time, error) {
	if name == "Bad" {
		return "", time.Time{}, fmt.Errorf("%w: name must be lowercase letters, digits, '-' or '_'", domain.ErrInvalid)
	}
	return "omegacd-invite-abc", time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), nil
}

func (s *fakeStore) ApproveHost(_ context.Context, host, fingerprint string) error {
	if fingerprint != "SHA256:right" {
		return fmt.Errorf("%w: Host %s's Agent has another key", domain.ErrInvalid, host)
	}
	return nil
}

func TestEnrollmentAPI(t *testing.T) {
	e := newEnv(t, false)
	e.expect("POST", "/api/settings/hosts/web/invite", "", 401) // an admin's
	e.login()
	if b := e.expect("POST", "/api/settings/hosts/web/invite", "", 201); !strings.Contains(b, `"invite":"omegacd-invite-abc"`) ||
		!strings.Contains(b, `"expiresAt":"2026-11-09T12:00:00Z"`) {
		t.Fatal(b)
	}
	e.expect("POST", "/api/settings/hosts/Bad/invite", "", 400)
	e.expect("POST", "/api/settings/hosts/web/approve", `{"fingerprint":"SHA256:wrong"}`, 400)
	e.expect("POST", "/api/settings/hosts/web/approve", `{"fingerprint":"SHA256:right"}`, 204)
	if b := e.expect("POST", "/api/settings/hosts/web/rotate", "", 400); !strings.Contains(b, "not connected") {
		t.Fatal(b)
	}
	e.expect("POST", "/api/settings/hosts/web/unlock", "", 204)
}

func (s *fakeStore) UnlockHost(context.Context, string) error { return nil }

// The Agents' endpoint needs no session (an Agent signs a challenge instead) and upgrades through the API's
// middleware: the Agent gets its challenge.
func TestAgentsConnectIsPublic(t *testing.T) {
	e := newEnv(t, false)
	c, err := wire.Dial(t.Context(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/api/agents/v1/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if m, err := c.Recv(t.Context()); err != nil || m.GetChallenge() == nil {
		t.Fatalf("want a challenge, got %v, %v", m, err)
	}
}

// Q83, Q92: a delete its Host's Agent must do waits (202, saying so); forget removes from the Control Plane only.
func TestDeleteWaitingAndForget(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	e.ctrl.apps["offline"] = domain.AppView{Application: domain.Application{Name: "offline"}}
	e.ctrl.apps["gone"] = domain.AppView{Application: domain.Application{Name: "gone"}}
	if b := e.expect("DELETE", "/api/apps/offline?cascade=true", "", 202); !strings.Contains(b, "waiting for the Host edge") {
		t.Fatal(b)
	}
	e.expect("DELETE", "/api/apps/gone?forget=true", "", 204)
	if _, ok := e.ctrl.apps["gone"]; ok {
		t.Fatal("not forgotten")
	}
}

func TestVersionIsPublic(t *testing.T) {
	e := newEnv(t, false)
	version.Version = "v1.2.3"
	t.Cleanup(func() { version.Version = "" })
	if b := e.expect("GET", "/api/version", "", 200); !strings.Contains(b, `"version":"v1.2.3"`) {
		t.Fatal(b)
	}
}

func TestRegistryCredentialRejectsGitURL(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	for _, reg := range []string{"https://github.com/obleey/", "github.com/obleey"} {
		if b := e.expect("POST", "/api/settings/registry-credentials", `{"registry":"`+reg+`","username":"u","password":"p"}`, 400); !strings.Contains(b, "Repository credentials") {
			t.Fatalf("%s: %s", reg, b)
		}
	}
}

func TestCheckRepo(t *testing.T) {
	e := newEnv(t, false)
	e.expect("POST", "/api/repos/check", `{"repoURL":"https://github.com/obleey/x.git"}`, 401)
	e.login()
	b := e.expect("POST", "/api/repos/check", `{"repoURL":"https://github.com/obleey/x.git"}`, 200)
	if b != `{"access":"credential","credentialPrefix":"https://github.com/obleey/","defaultBranch":"master"}`+"\n" {
		t.Fatal(b)
	}
	e.expect("POST", "/api/repos/check", `{"repoURL":""}`, 400)
}

func TestRepoCredentialsUsedBy(t *testing.T) {
	e := newEnv(t, false)
	e.src.creds = []domain.RepoCredential{{ID: 1, URLPrefix: "https://github.com/"}, {ID: 2, URLPrefix: "https://github.com/obleey/"}}
	for name, url := range map[string]string{"b": "https://github.com/obleey/b.git", "a": "https://github.com/obleey/a.git", "c": "https://github.com/other/c.git", "d": "https://gitlab.com/d.git"} {
		e.ctrl.apps[name] = domain.AppView{Application: domain.Application{Name: name, RepoURL: url}}
	}
	e.login()
	var got []struct {
		ID     int64    `json:"id"`
		UsedBy []string `json:"usedBy"`
	}
	if err := json.Unmarshal([]byte(e.expect("GET", "/api/settings/repo-credentials", "", 200)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || strings.Join(got[0].UsedBy, ",") != "c" || strings.Join(got[1].UsedBy, ",") != "a,b" {
		t.Fatalf("%+v", got)
	}
}

func TestHostKeysAPI(t *testing.T) {
	e := newEnv(t, false)
	e.expect("GET", "/api/settings/hosts/prod/keys", "", 401)
	e.login()
	e.ctrl.hosts["prod"] = domain.Host{Name: "prod", AgeRecipient: "age1prod"}
	b := e.expect("GET", "/api/settings/hosts/prod/keys", "", 200)
	var keys []map[string]any
	if err := json.Unmarshal([]byte(b), &keys); err != nil || len(keys) != 1 || keys[0]["recipient"] != "age1prod" || keys[0]["current"] != true {
		t.Fatalf("%s %v", b, err)
	}
	// the private half is born on the Agent and never leaves it (Q108): nothing makes, takes or removes one here
	e.expect("POST", "/api/settings/hosts/prod/keys/generate", "", 404)
	e.expect("POST", "/api/settings/hosts/prod/keys/import", `{}`, 404)
	e.expect("DELETE", "/api/settings/hosts/prod/keys/age1prod", "", 404)
	e.expect("GET", "/api/settings/hosts/nope/keys", "", 404)
}

// An internal error is logged at error with the request it answered (Q62); the client gets it too.
func TestInternalErrorIsLoggedWithItsRequest(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	e.src.err = errors.New("disk full")
	if code, _ := e.do("GET", "/api/settings/repo-credentials", ""); code != http.StatusInternalServerError {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(e.log.String(), `level=ERROR msg=http method=GET path=/api/settings/repo-credentials status=500`) ||
		!strings.Contains(e.log.String(), `err="disk full"`) {
		t.Errorf("log:\n%s", e.log.String())
	}
}

// Q28 behind a reverse proxy: the Agent's address is the one the API works out (X-Forwarded-For from a trusted proxy),
// so the allowlist holds behind one; from anyone else, the header is ignored and can't sneak an Agent in.
func TestAgentAllowlistBehindProxy(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	st, err := store.Open(filepath.Join(t.TempDir(), "omegacd.db"), crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	hub := agents.New(st, nil)
	invite, _ := st.CreateInvite(ctx, "web", time.Now().Add(time.Hour))
	_, key, _ := ed25519.GenerateKey(nil)
	if _, err := st.Enroll(ctx, invite, key.Public().(ed25519.PublicKey), "age1web", time.Now()); err != nil {
		t.Fatal(err)
	}
	st.ApproveHost(ctx, "web", domain.Fingerprint(key.Public().(ed25519.PublicKey)))
	if _, err := hub.SetAllowlist(ctx, "web", []string{"203.0.113.0/24"}); err != nil {
		t.Fatal(err)
	}
	// connect answers the challenge as the Agent behind a proxy that says it came from xff, and returns the answer
	connect := func(url, xff string) *agentv1.Envelope {
		c, err := wire.Dial(ctx, url, http.Header{"X-Forwarded-For": {xff}})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		e, _ := c.Recv(ctx)
		c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_Hello{Hello: &agentv1.Hello{AgentVersion: "(devel)"}}})
		c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_ChallengeResponse{ChallengeResponse: &agentv1.ChallengeResponse{
			PublicKey: key.Public().(ed25519.PublicKey),
			Signature: ed25519.Sign(key, append([]byte(agents.ChallengePrefix), e.GetChallenge().Nonce...))}}})
		e, _ = c.Recv(ctx)
		return e
	}
	serve := func(trusted string) string {
		proxies, _ := ParseTrustedProxies(trusted)
		srv := httptest.NewServer(New(Config{Store: st, Agents: hub, TrustedProxies: proxies}))
		t.Cleanup(srv.Close)
		return "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/agents/v1/connect"
	}

	behind := serve("127.0.0.1")
	if e := connect(behind, "203.0.113.7"); !e.GetEnrollmentStatus().GetApproved() {
		t.Fatalf("from an allowed address through the proxy: %v", e)
	}
	if e := connect(behind, "198.51.100.1"); !strings.Contains(e.GetError().GetMessage(), "not from 198.51.100.1") {
		t.Fatalf("from another address through the proxy: %v", e)
	}
	if e := connect(serve(""), "203.0.113.7"); !strings.Contains(e.GetError().GetMessage(), "not from 127.0.0.1") {
		t.Fatalf("a forged X-Forwarded-For from an untrusted peer was believed: %v", e)
	}
}

// failingWriter is a client that went away: every write fails.
type failingWriter struct{ h http.Header }

func (f failingWriter) Header() http.Header       { return f.h }
func (f failingWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }
func (f failingWriter) WriteHeader(int)           {}

// Q114: an admin gets the backup tar, logged with how they signed in; a backup that fails is an error, not half a
// file; without a backup configured, or a place to stage it, it says so; a client leaving mid-download is logged.
func TestBackup(t *testing.T) {
	e := newEnv(t, false)
	e.expect("GET", "/api/backup", "", 401)
	e.login()
	if b := e.expect("GET", "/api/backup", "", 200); b != "the tar" || !strings.Contains(e.log.String(), `msg="backup taken" via=session`) {
		t.Fatalf("%q\n%s", b, e.log.String())
	}
	e.store.backupErr = errors.New("disk full")
	e.expect("GET", "/api/backup", "", 500)

	logs := &strings.Builder{}
	log, _ := logging.New(logs, "debug", "text")
	none := &server{Config: Config{Log: log}}
	rec := httptest.NewRecorder()
	none.backup(rec, httptest.NewRequest("GET", "/api/backup", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("not configured: %d", rec.Code)
	}
	s := &server{Config: Config{Log: log, Backup: func(ctx context.Context, w io.Writer) error { _, err := io.WriteString(w, "tar"); return err }}}
	req := httptest.NewRequest("GET", "/api/backup", nil)
	req.Header.Set("Authorization", "Bearer ocd_x")
	s.backup(failingWriter{http.Header{}}, req)
	if !strings.Contains(logs.String(), "backup not fully sent") || !strings.Contains(logs.String(), "via=token") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	file := filepath.Join(t.TempDir(), "a-file")
	os.WriteFile(file, nil, 0o600)
	t.Setenv("TMPDIR", file)
	rec = httptest.NewRecorder()
	s.backup(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("no temporary folder: %d", rec.Code)
	}
}
