package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/obleey/omegacd/internal/domain"
)

func TestCheckAccessLocalIsPublic(t *testing.T) {
	f, g, _, _ := setup(t)
	got, err := g.CheckAccess(t.Context(), f.dir)
	if err != nil || got.Access != "public" || got.DefaultBranch != "main" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestCheckAccessDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	url := srv.URL + "/owner/repo.git"

	g := New(t.TempDir(), nil)
	got, err := g.CheckAccess(t.Context(), url)
	if err != nil || got.Access != "denied" || !strings.Contains(got.Message, "no repository credential") {
		t.Fatalf("no credential: got %+v, %v", got, err)
	}

	const token = "s3cr3t-token"
	g = New(t.TempDir(), func(context.Context) ([]domain.RepoCredential, error) {
		return []domain.RepoCredential{{URLPrefix: srv.URL + "/owner/", Username: "owner", Password: token}}, nil
	})
	got, err = g.CheckAccess(t.Context(), url)
	if err != nil || got.Access != "denied" || !strings.Contains(got.Message, srv.URL+"/owner/") || strings.Contains(got.Message, token) {
		t.Fatalf("rejected credential: got %+v, %v", got, err)
	}
}

func TestCheckAccessUnreachable(t *testing.T) {
	got, err := New(t.TempDir(), nil).CheckAccess(t.Context(), "http://127.0.0.1:1/x.git")
	if err != nil || got.Access != "error" || got.Message == "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestMatchCredential(t *testing.T) {
	creds := []domain.RepoCredential{
		{ID: 1, URLPrefix: "https://github.com/"},
		{ID: 2, URLPrefix: "https://github.com/obleey/"},
		{ID: 3, URLPrefix: "git@github.com:"},
	}
	for url, want := range map[string]int64{
		"https://github.com/obleey/omegacd.git": 2,
		"https://github.com/other/x.git":        1,
		"git@github.com:obleey/x.git":           3,
		"https://gitlab.com/obleey/x.git":       0,
	} {
		var got int64
		if c := MatchCredential(creds, url); c != nil {
			got = c.ID
		}
		if got != want {
			t.Errorf("%s: got credential %d, want %d", url, got, want)
		}
	}
}

// gitHTTP serves the repository at dir over smart HTTP (git http-backend), to owner:token only, like a private
// repository on a Git host.
func gitHTTP(t *testing.T, dir string) string {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(dir), "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "owner" || p != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/" + filepath.Base(dir)
}

func credsOf(cs ...domain.RepoCredential) func(context.Context) ([]domain.RepoCredential, error) {
	return func(context.Context) ([]domain.RepoCredential, error) { return cs, nil }
}

// Every answer of the access check: through a credential, and each reason it can't read a repository.
func TestCheckAccessAnswers(t *testing.T) {
	f, _, _, _ := setup(t)
	private := gitHTTP(t, f.dir)
	statusSrv := func(code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(srv.Close)
		return srv.URL + "/owner/repo.git"
	}
	empty := newFixture(t)
	headless := newFixture(t) // HEAD names a branch that isn't there: no HEAD to list
	headless.commit(map[string]string{"a": "b"})
	if err := headless.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/gone")); err != nil {
		t.Fatal(err)
	}
	key := sshKey(t)
	for _, tc := range []struct {
		name, url string
		creds     []domain.RepoCredential
		access    string
		message   string
		branch    string
	}{
		{"a credential that works", private, []domain.RepoCredential{{URLPrefix: private, Username: "owner", Password: token}}, "credential", "", "main"},
		{"a credential without a password", private, []domain.RepoCredential{{URLPrefix: private, Username: "owner"}}, "denied", "has no password/token or SSH key", ""},
		{"not found", statusSrv(http.StatusNotFound), nil, "denied", "Repository not found or private; no repository credential", ""},
		{"forbidden", statusSrv(http.StatusForbidden), nil, "denied", "Authorization failed; no repository credential", ""},
		{"an empty repository", empty.dir, nil, "public", "", ""},
		{"no HEAD", headless.dir, nil, "public", "", ""},
		{"a URL that doesn't parse", "https://%zz/r.git", nil, "error", "invalid URL escape", ""},
		{"SSH without a credential", "git@example.com:org/repo.git", nil, "denied", "SSH needs a repository credential with a private key", ""},
		{"SSH with a password only", "git@example.com:org/repo.git", []domain.RepoCredential{{URLPrefix: "git@example.com:", Password: token}}, "denied", "has no password/token or SSH key", ""},
		{"SSH with a key that doesn't parse", "git@example.com:org/repo.git", []domain.RepoCredential{{URLPrefix: "git@example.com:", SSHPrivateKey: "not a key"}}, "error", "ssh key for git@example.com:", ""},
		{"SSH without known_hosts", "git@example.com:org/repo.git", []domain.RepoCredential{{URLPrefix: "git@example.com:", SSHPrivateKey: key}}, "error", "no known_hosts file configured", ""},
	} {
		got, err := New(t.TempDir(), credsOf(tc.creds...)).CheckAccess(t.Context(), tc.url)
		if err != nil || got.Access != tc.access || !strings.Contains(got.Message, tc.message) || got.DefaultBranch != tc.branch || strings.Contains(got.Message, token) {
			t.Errorf("%s: %+v, %v", tc.name, got, err)
		}
	}

	failing := New(t.TempDir(), func(context.Context) ([]domain.RepoCredential, error) { return nil, errors.New("disk on fire") })
	if _, err := failing.CheckAccess(t.Context(), private); err == nil || !strings.Contains(err.Error(), "load repo credentials: disk on fire") {
		t.Errorf("credentials that can't be loaded: %v", err)
	}
	if _, err := failing.Resolve(t.Context(), private, "main"); err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("a fetch without its credentials: %v", err)
	}
	g := New(t.TempDir(), credsOf(domain.RepoCredential{URLPrefix: "git@example.com:", SSHPrivateKey: key}))
	g.KnownHostsFile = filepath.Join(t.TempDir(), "missing")
	if _, err := g.auth(t.Context(), "git@example.com:org/repo.git"); err == nil || !strings.Contains(err.Error(), "load known_hosts") {
		t.Errorf("a known_hosts file that isn't there: %v", err)
	}
}
