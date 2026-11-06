package source

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/obleey/omegacd/internal/domain"
)

type fixture struct {
	t    *testing.T
	dir  string
	repo *git.Repository
}

func newFixture(t *testing.T) *fixture {
	dir := t.TempDir()
	r, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: "refs/heads/main"}})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t, dir, r}
}

// commit writes files (nil content deletes) and commits, returning the SHA.
func (f *fixture) commit(files map[string]string) string {
	f.t.Helper()
	wt, err := f.repo.Worktree()
	if err != nil {
		f.t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(f.dir, name)
		if content == "" {
			if _, err := wt.Remove(name); err != nil {
				f.t.Fatal(err)
			}
			continue
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			f.t.Fatal(err)
		}
		if _, err := wt.Add(name); err != nil {
			f.t.Fatal(err)
		}
	}
	h, err := wt.Commit("c", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()}})
	if err != nil {
		f.t.Fatal(err)
	}
	return h.String()
}

func setup(t *testing.T) (*fixture, *Git, string, string) {
	f := newFixture(t)
	c1 := f.commit(map[string]string{"README.md": "hi\n", "app/compose.yaml": "services: {}\n", "app/run.sh": "#!/bin/sh\n", "app/db.sops.env": "enc\n"})
	if _, err := f.repo.CreateTag("v1", plumbing.NewHash(c1), &git.CreateTagOptions{Message: "v1", Tagger: &object.Signature{Name: "t", Email: "t@t", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	c2 := f.commit(map[string]string{"README.md": "hello\n"})
	g := New(t.TempDir(), nil)
	return f, g, c1, c2
}

func TestResolve(t *testing.T) {
	f, g, c1, c2 := setup(t)
	ctx := context.Background()
	for rev, want := range map[string]string{"main": c2, "v1": c1, c1: c1, c1[:8]: c1} {
		got, err := g.Resolve(ctx, f.dir, rev)
		if err != nil || got != want {
			t.Errorf("Resolve(%s) = %s, %v; want %s", rev, got, err, want)
		}
	}
	if _, err := g.Resolve(ctx, f.dir, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown rev: %v", err)
	}
}

func TestResolveFetchCooldown(t *testing.T) {
	f, g, _, c2 := setup(t)
	ctx := context.Background()
	if got, _ := g.Resolve(ctx, "file://"+f.dir, "main"); got != c2 {
		t.Fatalf("got %s", got)
	}
	c3 := f.commit(map[string]string{"x": "y"})
	if got, _ := g.Resolve(ctx, "file://"+f.dir, "main"); got != c2 {
		t.Errorf("within cooldown: got %s, want cached %s", got, c2)
	}
	g.Invalidate(f.dir) // a webhook or Refresh: matched by normalized URL, so the other spelling counts
	if got, _ := g.Resolve(ctx, "file://"+f.dir, "main"); got != c3 {
		t.Errorf("after Invalidate: got %s, want %s", got, c3)
	}
	c4 := f.commit(map[string]string{"x": "z"})
	g.FetchCooldown = 0
	if got, _ := g.Resolve(ctx, "file://"+f.dir, "main"); got != c4 {
		t.Errorf("after cooldown: got %s, want %s", got, c4)
	}
}

func TestDiff(t *testing.T) {
	f, g, c1, c2 := setup(t)
	ctx := context.Background()
	c3 := f.commit(map[string]string{"app/compose.yaml": "services: {web: {}}\n"})
	d, err := g.Diff(ctx, f.dir, c1, c3, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d, "--- a/app/compose.yaml") || !strings.Contains(d, "+services: {web: {}}") || strings.Contains(d, "README") {
		t.Errorf("diff:\n%s", d)
	}
	if d, _ := g.Diff(ctx, f.dir, c1, c2, "app"); d != "" {
		t.Errorf("app unchanged but diff:\n%s", d)
	}
	if d, _ := g.Diff(ctx, f.dir, "", c1, "app"); !strings.Contains(d, "+++ b/app/run.sh") {
		t.Errorf("empty-tree diff:\n%s", d)
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://GitHub.com/Org/Repo.git":       "github.com/Org/Repo",
		"https://user:tok@github.com/org/repo/": "github.com/org/repo",
		"git@github.com:org/repo.git":           "github.com/org/repo",
		"ssh://git@github.com/org/repo":         "github.com/org/repo",
		"http://gitea.local:3000/org/repo":      "gitea.local:3000/org/repo",
		"file:///tmp/repo":                      "/tmp/repo",
		"/tmp/repo/":                            "/tmp/repo",
	} {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func sign(body, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestParseWebhook(t *testing.T) {
	gh := `{"repository":{"html_url":"https://github.com/o/r","clone_url":"https://github.com/o/r.git","ssh_url":"git@github.com:o/r.git"}}`
	gl := `{"project":{"git_http_url":"https://gitlab.com/o/r.git","git_ssh_url":"git@gitlab.com:o/r.git"}}`
	h := func(kv ...string) http.Header {
		hd := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			hd.Set(kv[i], kv[i+1])
		}
		return hd
	}
	cases := []struct {
		name, provider, body string
		header               http.Header
		urls                 int
		sigErr               bool
	}{
		{"github ok", "github", gh, h("X-GitHub-Event", "push", "X-Hub-Signature-256", "sha256="+sign(gh, "s")), 3, false},
		{"github bad sig", "github", gh, h("X-GitHub-Event", "push", "X-Hub-Signature-256", "sha256="+sign(gh, "x")), 0, true},
		{"github ping", "github", gh, h("X-GitHub-Event", "ping", "X-Hub-Signature-256", "sha256="+sign(gh, "s")), 0, false},
		{"gitlab ok", "gitlab", gl, h("X-Gitlab-Event", "Push Hook", "X-Gitlab-Token", "s"), 2, false},
		{"gitlab tag", "gitlab", gl, h("X-Gitlab-Event", "Tag Push Hook", "X-Gitlab-Token", "s"), 2, false},
		{"gitlab bad token", "gitlab", gl, h("X-Gitlab-Event", "Push Hook", "X-Gitlab-Token", "x"), 0, true},
		{"gitea ok", "gitea", gh, h("X-Gitea-Event", "push", "X-Gitea-Signature", sign(gh, "s")), 3, false},
		{"forgejo ok", "gitea", gh, h("X-Forgejo-Event", "push", "X-Forgejo-Signature", sign(gh, "s")), 3, false},
		{"gitea bad sig", "gitea", gh, h("X-Gitea-Event", "push", "X-Gitea-Signature", "zz"), 0, true},
	}
	for _, c := range cases {
		urls, err := ParseWebhook(c.provider, c.header, []byte(c.body), "s")
		if errors.Is(err, ErrSignature) != c.sigErr || (!c.sigErr && err != nil) || len(urls) != c.urls {
			t.Errorf("%s: urls=%v err=%v", c.name, urls, err)
		}
	}
	if urls, err := ParseWebhook("github", h("X-GitHub-Event", "push"), []byte(gh), ""); err != nil || len(urls) != 3 {
		t.Errorf("no secret: %v %v", urls, err)
	}
}
