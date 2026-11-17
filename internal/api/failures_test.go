package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/obleey/omegacd/internal/domain"
)

var errLocked = errors.New("database is locked")

func (s *fakeSource) SaveCredential(_ context.Context, c domain.RepoCredential) (domain.RepoCredential, error) {
	return c, s.err
}

func (s *fakeSource) DeleteCredential(_ context.Context, id int64) error {
	if id != 1 {
		return domain.ErrNotFound
	}
	return s.err
}

// Logs writes c.logLines, then fails with c.logErr.
func (c *fakeController) Logs(_ context.Context, _, _ string, _ bool, _ int, w io.Writer) error {
	c.mu.Lock()
	lines, err := c.logLines, c.logErr
	c.mu.Unlock()
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	return err
}

func (c *fakeController) logsAre(lines []string, err error) {
	c.mu.Lock()
	c.logLines, c.logErr = lines, err
	c.mu.Unlock()
}

// The store failing under a signed-in admin: each request fails with a 500 and the error, logged with the request;
// a session that can't be read or extended is logged too (Q132).
func TestAuthStoreFailures(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	for _, tc := range []struct{ method, path, body, broken string }{
		{"GET", "/api/apps", "", "AdminPasswordHash"},
		{"GET", "/api/auth/me", "", "AdminPasswordHash"},
		{"POST", "/api/auth/password", changeBody(adminPassword, newPassword), "AdminPasswordHash"},
		{"POST", "/api/auth/password", changeBody(adminPassword, newPassword), "SetAdminPassword"},
		{"POST", "/api/auth/password", changeBody(adminPassword, newPassword+"2"), "DeleteSessionsExcept"},
		{"POST", "/api/auth/logout", "", "DeleteSession"},
	} {
		e.store.breaks(tc.broken, errLocked)
		if b := e.expect(tc.method, tc.path, tc.body, 500); !strings.Contains(b, "database is locked") {
			t.Errorf("%s %s: %s", tc.method, tc.path, b)
		}
		e.store.breaks(tc.broken, nil)
	}
	if !strings.Contains(e.log.String(), `level=ERROR msg=http method=POST path=/api/auth/logout status=500`) {
		t.Errorf("logs:\n%s", e.log.String())
	}

	e.store.mu.Lock()
	for h := range e.store.sessions { // an hour into the session: it slides
		e.store.sessions[h] = time.Now().Add(sessionTTL - 2*time.Hour)
	}
	e.store.mu.Unlock()
	e.store.breaks("ExtendSession", errLocked)
	e.expect("GET", "/api/apps", "", 200)
	e.store.breaks("GetSession", errLocked)
	e.expect("GET", "/api/apps", "", 401)
	for _, want := range []string{`level=ERROR msg="extend session" err="database is locked"`,
		`level=ERROR msg="get session" err="database is locked"`} {
		if !strings.Contains(e.log.String(), want) {
			t.Errorf("no %s", want)
		}
	}

	e = newEnv(t, false)
	e.store.breaks("AdminPasswordHash", errLocked)
	e.expect("POST", "/api/auth/login", loginBody(), 500)
	e.store.breaks("AdminPasswordHash", nil)
	e.store.breaks("CreateSession", errLocked)
	e.expect("POST", "/api/auth/login", loginBody(), 500)
	e.expect("POST", "/api/auth/login", `not json`, 400)
}

// Q148: a new password is 8 to 72 bytes, and not the current one; the first admin password from the environment too.
func TestPasswordLength(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	e.expect("POST", "/api/auth/password", `not json`, 400)
	e.expect("POST", "/api/auth/password", changeBody(adminPassword, newPassword[:5]), 400)
	long := strings.Repeat("x", 73)
	if b := e.expect("POST", "/api/auth/password", changeBody(adminPassword, long), 400); !strings.Contains(b, "at most 72 bytes") {
		t.Fatal(b)
	}
	s := &fakeStore{}
	if err := EnsureAdmin(context.Background(), s, long, nil); err == nil || !strings.Contains(err.Error(), "OMEGACD_ADMIN_PASSWORD is 73 bytes: at most 72") {
		t.Errorf("a first password too long: %v", err)
	}
	s.breaks("SetAdminPassword", errLocked)
	if err := EnsureAdmin(context.Background(), s, newPassword, nil); !errors.Is(err, errLocked) {
		t.Errorf("not saved: %v", err)
	}
}

// Requests that can't be what they ask for: bodies that aren't JSON, ids that aren't numbers, names that don't match,
// what isn't there.
func TestBadRequests(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	e.ctrl.CreateApp(context.Background(), domain.Application{Name: "web"})
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"PUT", "/api/apps/web", "not json", 400},
		{"PUT", "/api/apps/web", `{"name":"other"}`, 400},
		{"POST", "/api/apps/web/rollback", "not json", 400},
		{"GET", "/api/apps/ghost/export", "", 404},
		{"POST", "/api/repos/check", "not json", 400},
		{"POST", "/api/settings/repo-credentials", `{"username":"x"}`, 400},
		{"PUT", "/api/settings/repo-credentials/x", `{}`, 400},
		{"DELETE", "/api/settings/repo-credentials/x", "", 400},
		{"DELETE", "/api/settings/repo-credentials/2", "", 404},
		{"DELETE", "/api/settings/repo-credentials/1", "", 204},
		{"POST", "/api/settings/hosts/edge/approve", "not json", 400},
		{"POST", "/api/settings/hosts/edge/rename", "not json", 400},
		{"PUT", "/api/settings/hosts/edge/allow-ips", "not json", 400},
		{"POST", "/api/settings/registry-credentials", `{"username":"x"}`, 400},
		{"PUT", "/api/settings/registry-credentials/9", `{"registry":"ghcr.io"}`, 404},
		{"DELETE", "/api/settings/registry-credentials/x", "", 400},
		{"DELETE", "/api/settings/registry-credentials/9", "", 404},
		{"POST", "/api/settings/tokens", `{"name":" "}`, 400},
		{"DELETE", "/api/settings/tokens/x", "", 400},
		{"DELETE", "/api/settings/tokens/9", "", 404},
		{"PATCH", "/index.html", "", 405},
	} {
		e.expect(tc.method, tc.path, tc.body, tc.want)
	}
	e.expect("GET", "/api/apps/web/logs/c1?tail=many", "", 200) // a tail that isn't a number: the default
}

// Registry credentials and tokens round trip through the API; the store failing to list them fails the request.
func TestRegistryCredentialsAndTokens(t *testing.T) {
	e := newEnv(t, false)
	e.login()
	e.expect("POST", "/api/settings/registry-credentials", `{"registry":"ghcr.io","username":"me","password":"`+newPassword+`"}`, 201)
	if b := e.expect("PUT", "/api/settings/registry-credentials/1", `{"registry":"ghcr.io","username":"you"}`, 200); !strings.Contains(b, `"username":"you"`) {
		t.Error(b)
	}
	if b := e.expect("GET", "/api/settings/registry-credentials", "", 200); !strings.Contains(b, `"registry":"ghcr.io"`) {
		t.Error(b)
	}
	e.store.breaks("ListRegistryCredentials", errLocked)
	e.expect("PUT", "/api/settings/registry-credentials/1", `{"registry":"ghcr.io"}`, 500)
	e.store.breaks("ListRegistryCredentials", nil)
	e.expect("DELETE", "/api/settings/registry-credentials/1", "", 204)

	e.expect("POST", "/api/settings/tokens", `{"name":"ci"}`, 201)
	if b := e.expect("GET", "/api/settings/tokens", "", 200); !strings.Contains(b, `"name":"ci"`) {
		t.Error(b)
	}
	e.expect("DELETE", "/api/settings/tokens/1", "", 204)
}

// A server that accepts no Agents refuses what needs them; one without webhooks says so.
func TestWithoutAgentsOrWebhooks(t *testing.T) {
	e := newEnvWith(t, false, func(c *Config) { c.Agents, c.WebhookParse = nil, nil })
	e.login()
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/settings/hosts/edge/approve", `{"fingerprint":"x"}`},
		{"POST", "/api/settings/hosts/edge/rotate", ""},
		{"POST", "/api/settings/hosts/edge/unlock", ""},
		{"PUT", "/api/settings/hosts/edge/allow-ips", `{"allowIPs":[]}`},
	} {
		if b := e.expect(tc.method, tc.path, tc.body, 400); !strings.Contains(b, "accepts no Agents") {
			t.Errorf("%s %s: %s", tc.method, tc.path, b)
		}
	}
	e.expect("POST", "/api/webhooks/github", "{}", 501)
	e.expect("POST", "/api/webhooks/github", strings.Repeat("x", 5<<20+1), 400)
}

// Without the UI embedded, the server says so on every page.
func TestNoUI(t *testing.T) {
	e := newEnvWith(t, false, func(c *Config) { c.UI = nil })
	if b := e.expect("GET", "/", "", 200); !strings.Contains(b, "The UI is not embedded in this build") {
		t.Fatal(b)
	}
}

// The client trusted when its address can't be parsed is that address; garbage in X-Forwarded-For stops the walk at
// the proxy.
func TestClientIPEdges(t *testing.T) {
	proxies, _ := ParseTrustedProxies("127.0.0.1")
	s := &server{Config: Config{TrustedProxies: proxies}}
	if ip := s.clientIP(&http.Request{RemoteAddr: "@pipe"}); ip != "@pipe" {
		t.Errorf("unparsable: %q", ip)
	}
	r := &http.Request{RemoteAddr: "127.0.0.1:5000", Header: http.Header{"X-Forwarded-For": {"203.0.113.9, garbage"}}}
	if ip := s.clientIP(r); ip != "127.0.0.1" {
		t.Errorf("garbage: %q", ip)
	}
}

// failFlush is a response writer whose client is gone: every flush fails.
type failFlush struct{ *httptest.ResponseRecorder }

func (failFlush) FlushError() error { return errors.New("client gone") }

// A stream whose client went away stops at the next flush: the events, and logs that keep coming.
func TestStreamsStopWhenTheClientIsGone(t *testing.T) {
	e := newEnv(t, false)
	s := &server{Config: Config{Controller: e.ctrl}}
	e.ctrl.events <- domain.Event{Type: "app", App: "web"}
	done := make(chan struct{})
	go func() {
		s.events(failFlush{httptest.NewRecorder()}, httptest.NewRequest("GET", "/api/events", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("kept streaming to a client gone")
	}

	e.ctrl.logsAre([]string{"one", "two"}, nil)
	w := failFlush{httptest.NewRecorder()}
	s.logs(w, httptest.NewRequest("GET", "/api/apps/web/logs/c1", nil))
	if strings.Count(w.Body.String(), "event: log") != 1 {
		t.Errorf("went on after the first failed flush: %q", w.Body.String())
	}

	e.ctrl.logsAre(nil, nil) // nothing at all: the stream just ends
	w = failFlush{httptest.NewRecorder()}
	s.logs(w, httptest.NewRequest("GET", "/api/apps/web/logs/c1", nil))
	if !strings.Contains(w.Body.String(), "event: end") {
		t.Errorf("no end: %q", w.Body.String())
	}
	e.ctrl.logsAre([]string{"one"}, errors.New("container gone"))
	ok := okFlush{httptest.NewRecorder()} // the error after a line already went: an error event
	s.logs(ok, httptest.NewRequest("GET", "/api/apps/web/logs/c1", nil))
	if !strings.Contains(ok.Body.String(), "event: error\ndata: container gone") {
		t.Errorf("no error event: %q", ok.Body.String())
	}
}

type okFlush struct{ *httptest.ResponseRecorder }

func (okFlush) FlushError() error { return nil }

// /metrics lists each Application's status; one it can't list fails the scrape with why.
func TestMetricsApps(t *testing.T) {
	e := newEnv(t, false)
	e.ctrl.CreateApp(context.Background(), domain.Application{Name: "web"})
	if b := e.expect("GET", "/metrics", "", 200); !strings.Contains(b, `omegacd_app_info{app="web",health="",sync_status="OutOfSync"} 1`) {
		t.Fatal(b)
	}
	e.ctrl.mu.Lock()
	e.ctrl.listErr = errLocked
	e.ctrl.mu.Unlock()
	if b := e.expect("GET", "/metrics", "", 500); !strings.Contains(b, "database is locked") {
		t.Fatal(b)
	}
}
