package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/obleey/dockercd/internal/domain"
)

type fakeStore struct {
	domain.Store // unimplemented methods panic
	mu           sync.Mutex
	hash         string
	mustChange   bool
	tokens       map[string]domain.APIToken
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
	mu      sync.Mutex
	apps    map[string]domain.AppView
	events  chan domain.Event
	changed chan string
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

func (c *fakeController) DeleteApp(ctx context.Context, name string, cascade bool) error {
	if _, err := c.GetApp(ctx, name); err != nil {
		return err
	}
	delete(c.apps, name)
	return nil
}

func (c *fakeController) Sync(ctx context.Context, name string) (int64, error) {
	if _, err := c.GetApp(ctx, name); err != nil {
		return 0, err
	}
	return 7, nil
}

func (c *fakeController) Rollback(_ context.Context, name string, opID int64) (int64, error) {
	return opID + 1, nil
}

func (c *fakeController) RepoChanged(_ context.Context, url string) { c.changed <- url }

func (c *fakeController) Subscribe() (<-chan domain.Event, func()) { return c.events, func() {} }

type env struct {
	t     *testing.T
	srv   *httptest.Server
	store *fakeStore
	ctrl  *fakeController
	c     *http.Client
}

func newEnv(t *testing.T, mustChange bool) *env {
	store := &fakeStore{tokens: map[string]domain.APIToken{}}
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.MinCost)
	store.hash, store.mustChange = string(hash), mustChange
	ctrl := &fakeController{apps: map[string]domain.AppView{}, events: make(chan domain.Event, 1), changed: make(chan string, 4)}
	h := New(Config{
		Controller: ctrl, Store: store, WebhookSecret: "s3",
		WebhookParse: func(provider string, h http.Header, body []byte, secret string) ([]string, error) {
			if h.Get("X-Sig") != secret {
				return nil, errors.New("bad signature")
			}
			return []string{"https://example.com/r.git"}, nil
		},
		UI: fstest.MapFS{
			"index.html":            {Data: []byte("INDEX")},
			"_app/immutable/app.js": {Data: []byte("JS")},
		},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &env{t: t, srv: srv, store: store, ctrl: ctrl, c: &http.Client{Jar: jar}}
}

// do sends a request; hdr pairs are header key/values. Returns status and body.
func (e *env) do(method, path, body string, hdr ...string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "dockercd")
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
	if b := e.expect("POST", "/api/apps/web/sync", "", 202); !strings.Contains(b, `"opID":7`) {
		t.Fatal(b)
	}
	if b := e.expect("POST", "/api/apps/web/rollback", `{"opID":3}`, 202); !strings.Contains(b, `"opID":4`) {
		t.Fatal(b)
	}
	e.expect("POST", "/api/apps/nope/sync", "", 404)
	e.expect("DELETE", "/api/apps/web?cascade=true", "", 204)
	e.expect("DELETE", "/api/apps/web", "", 404)
	e.expect("GET", "/api/nothing", "", 404)
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
