package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/obleey/omegacd/internal/domain"
)

func server(t *testing.T) *Server {
	s, err := NewServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRender(t *testing.T) {
	f, _, c1, c2 := setup(t)
	s := server(t)
	ctx := context.Background()
	spec := func(sha, p string) domain.RenderSpec {
		return domain.RenderSpec{RepoURL: f.dir, Revision: sha, Path: p}
	}
	r1, err := s.Render(ctx, spec(c1, "app"))
	if err != nil || r1.Invalid != nil {
		t.Fatal(err, r1.Invalid)
	}
	var got []string
	for _, file := range r1.Files {
		got = append(got, file.Path+" "+strconv.FormatUint(uint64(file.Mode), 8))
	}
	if want := []string{"compose.yaml 644", "db.sops.env 644", "run.sh 755"}; !slices.Equal(got, want) {
		t.Fatalf("files %q, want %q", got, want)
	}
	if b, err := s.Blob(ctx, r1.Files[0].SHA256); err != nil || string(b) != "services: {}\n" {
		t.Fatalf("Blob = %q, %v", b, err)
	}

	// deterministic: same files, same result, whatever the commit or the spelling of the path
	s2 := server(t)
	r2, _ := s2.Render(ctx, spec(c2, "app/"))
	if !slices.Equal(r1.Files, r2.Files) || r1.Checksum != r2.Checksum {
		t.Fatalf("app unchanged between commits, but rendered differently:\n%v\n%v", r1, r2)
	}
	root1, _ := s.Render(ctx, spec(c1, ""))
	root2, _ := s.Render(ctx, spec(c2, "."))
	if root1.Checksum == root2.Checksum {
		t.Error("README changed but the root's checksum did not")
	}
	c3 := f.commit(map[string]string{"app/compose.yaml": "services: {a: {image: x}}\n", "app/" + domain.MarkerFile: "other"})
	r3, err := s.Render(ctx, spec(c3, "app")) // c3 not fetched yet: exercises fetch-on-miss
	if err != nil || r3.Checksum == r1.Checksum {
		t.Fatalf("compose.yaml changed: %v, checksum %s", err, r3.Checksum)
	}
	if slices.ContainsFunc(r3.Files, func(f domain.File) bool { return f.Path == domain.MarkerFile }) {
		t.Error("a marker file was taken from Git")
	}
	if _, err := s.Render(ctx, spec(c1, "missing")); err == nil || !strings.Contains(err.Error(), `path "missing" not found`) {
		t.Errorf("missing path: %v", err)
	}
}

func TestRenderRejectsEscapingSymlink(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(filepath.Join(f.dir, "app"), 0o755)
	os.Symlink("../../etc/passwd", filepath.Join(f.dir, "app", "evil"))
	wt, _ := f.repo.Worktree()
	wt.Add("app/evil")
	h, _ := wt.Commit("c", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()}})
	r, err := server(t).Render(context.Background(), domain.RenderSpec{RepoURL: f.dir, Revision: h.String(), Path: "app"})
	if err != nil || !errors.Is(r.Invalid, domain.ErrInvalid) {
		t.Errorf("want Invalid, got %v, %v", r.Invalid, err)
	}
	if r.Files[0].Mode&fs.ModeSymlink == 0 {
		t.Errorf("mode %v, want a symlink", r.Files[0].Mode)
	}
}

func TestRenderApps(t *testing.T) {
	f := newFixture(t)
	c := f.commit(map[string]string{"apps/web.yaml": "name: web\nrepoURL: x\nhost: local\n"})
	r, err := server(t).Render(context.Background(), domain.RenderSpec{RepoURL: f.dir, Revision: c, Path: "apps", Apps: true})
	if err != nil || r.Invalid != nil || len(r.Apps) != 1 || r.Apps[0].Name != "web" {
		t.Fatalf("Render = %+v, %v", r, err)
	}
}

func TestRenderRejectsBuild(t *testing.T) {
	f := newFixture(t)
	c := f.commit(map[string]string{"compose.yaml": "services:\n  app:\n    build: .\n"})
	r, err := server(t).Render(context.Background(), domain.RenderSpec{RepoURL: f.dir, Revision: c})
	if err != nil || !errors.Is(r.Invalid, domain.ErrInvalid) || !strings.Contains(r.Invalid.Error(), "uses build:") {
		t.Fatalf("Invalid = %v, %v", r.Invalid, err)
	}
}

func TestValidateBindSources(t *testing.T) {
	tests := []struct {
		volume  string
		wantErr string // "" = accepted
	}{
		{"./conf.yaml:/etc/app.yaml:ro", ""},  // file from Git
		{"./html:/html", ""},                  // folder from Git
		{".:/app", ""},                        // the whole app folder
		{"data:/data", ""},                    // named volume
		{"/opt/omegacd-it-missing:/data", ""}, // absolute path: never touched, Docker creates it
		{"./data:/data", "mounts ./data, which is not in Git"},
		{"./html/cache:/cache", "mounts ./html/cache, which is not in Git"},
		{"{type: bind, source: ./db, target: /db}", "mounts ./db, which is not in Git"},
		{"../shared:/shared", "mounts ../shared, outside the Application's files"},
		{"./html/../../x:/x", "outside the Application's files"},
		{"ROOT/other:/other", ""}, // absolute, even right next to the app folder
		{"~/media:/media", ""},    // home dir: absolute too
	}
	for _, tt := range tests {
		root := t.TempDir()
		dir := filepath.Join(root, "web") // as in the work dir
		if err := os.MkdirAll(filepath.Join(dir, "html"), 0o755); err != nil {
			t.Fatal(err)
		}
		vol := strings.Replace(tt.volume, "ROOT", root, 1)
		if !strings.HasPrefix(vol, "{") {
			vol = strconv.Quote(vol)
		}
		files := map[string]string{
			"conf.yaml": "a: 1\n",
			"web.env":   "A=1\n",
			"compose.yaml": "services:\n  web:\n    image: busybox\n    env_file: web.env\n    configs: [conf]\n    volumes: [" + vol + "]\n" +
				"configs:\n  conf: {file: ./conf.yaml}\nvolumes:\n  data: {}\n",
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		err := validate(context.Background(), domain.Application{Name: "web"}, dir)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: validate = %v, want accepted", tt.volume, err)
		case tt.wantErr != "" && (!errors.Is(err, domain.ErrInvalid) || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("%s: validate = %v, want ErrInvalid containing %q", tt.volume, err, tt.wantErr)
		}
	}
}

// Without its secrets a compose file may not load; Render then leaves it to the final render on the Host.
func TestValidateNeedsSecrets(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: ${IMAGE:?set in .env}\n"), 0o644)
	if err := validate(context.Background(), domain.Application{Name: "web"}, dir); err != nil {
		t.Fatal(err)
	}
}

func TestPlainSecrets(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`services:
  db:
    image: mariadb
    environment:
      MARIADB_ROOT_PASSWORD: hunter2        # warn
      MARIADB_PASSWORD: ${DB_PASSWORD}      # from a SOPS .env: fine
      MARIADB_PASSWORD_FILE: /run/secrets/x # a path: fine
      MARIADB_DATABASE: wordpress           # not a secret name
      REQUIRE_PASSWORD: "true"              # a flag
  web:
    image: app
    environment:
      - API_TOKEN=abc123                    # warn (list form)
      - SESSION_SECRET=$${KEEP}             # $ escapes: treated as templated
      - GF_SECURITY_ADMIN_PASSWORD          # no value: passed through from the env
      - EMPTY_SECRET=
`), 0o644)
	os.WriteFile(filepath.Join(dir, "prod.yaml"), []byte("services:\n  web:\n    environment: {AWS_SECRET_ACCESS_KEY: wJalr}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("services: [\n"), 0o644)

	got := plainSecrets(domain.Application{ComposeFiles: []string{"compose.yaml", "prod.yaml", "broken.yaml", "missing.yaml"}}, dir)
	var keys []string
	for _, w := range got {
		keys = append(keys, strings.SplitN(w, " is in plain text", 2)[0])
	}
	if want := []string{"db: MARIADB_ROOT_PASSWORD", "web: API_TOKEN", "web: AWS_SECRET_ACCESS_KEY"}; !slices.Equal(keys, want) {
		t.Fatalf("warned about %q, want %q", keys, want)
	}
	if want := "db: MARIADB_ROOT_PASSWORD is in plain text in compose.yaml, readable by anyone with access to the repository; " +
		"put it in a SOPS-encrypted .env and write MARIADB_ROOT_PASSWORD: ${MARIADB_ROOT_PASSWORD}"; got[0] != want {
		t.Fatalf("message:\n%s", got[0])
	}
	if strings.Contains(strings.Join(got, ""), "hunter2") {
		t.Fatal("a warning must not repeat the secret")
	}
}

func TestRenderCacheKeepsRecentlyUsed(t *testing.T) {
	f := newFixture(t)
	var shas []string
	for i := range 3 {
		shas = append(shas, f.commit(map[string]string{"compose.yaml": fmt.Sprintf("services: {s%d: {image: x}}\n", i)}))
	}
	s := server(t)
	s.maxRendered = 2
	ctx := t.Context()
	render := func(sha string) {
		if _, err := s.Render(ctx, domain.RenderSpec{RepoURL: f.dir, Revision: sha}); err != nil {
			t.Fatal(err)
		}
	}
	cachedRevs := func() (revs []string) {
		for e := s.recent.Front(); e != nil; e = e.Next() {
			revs = append(revs, strings.Split(e.Value.(*cached).key, "\x00")[1][:1])
		}
		return revs
	}
	render(shas[0])
	render(shas[1])
	render(shas[0]) // used again: now the most recent
	render(shas[2]) // evicts shas[1], the least recently used
	if got, want := cachedRevs(), []string{shas[2][:1], shas[0][:1]}; !slices.Equal(got, want) || len(s.rendered) != 2 {
		t.Fatalf("cached %v (%d in map), want %v", got, len(s.rendered), want)
	}
}

func TestCollectBlobs(t *testing.T) {
	f := newFixture(t)
	old := f.commit(map[string]string{"compose.yaml": "services: {old: {image: x}}\n"})
	cur := f.commit(map[string]string{"compose.yaml": "services: {cur: {image: x}}\n"})
	s := server(t)
	s.maxRendered = 1
	ctx := t.Context()
	rOld, _ := s.Render(ctx, domain.RenderSpec{RepoURL: f.dir, Revision: old})
	rCur, _ := s.Render(ctx, domain.RenderSpec{RepoURL: f.dir, Revision: cur}) // evicts old's render
	ageBlobs(t, s)
	if n, err := s.collectBlobs(); err != nil || n != 1 {
		t.Fatalf("collected %d, %v; want old's compose.yaml only", n, err)
	}
	if _, err := s.Blob(ctx, rOld.Files[0].SHA256); err == nil {
		t.Error("an unused blob survived")
	}
	if _, err := s.Blob(ctx, rCur.Files[0].SHA256); err != nil {
		t.Errorf("a cached render's blob was collected: %v", err)
	}
	// a cache only: rendering the old commit again puts it back (evicting cur's render, whose aged blob goes)
	rOld, _ = s.Render(ctx, domain.RenderSpec{RepoURL: f.dir, Revision: old})
	if n, err := s.collectBlobs(); err != nil || n != 1 {
		t.Fatalf("collected %d, %v; want cur's compose.yaml only", n, err)
	}
	if b, err := s.Blob(ctx, rOld.Files[0].SHA256); err != nil || !strings.Contains(string(b), "old") {
		t.Errorf("re-rendered blob: %q, %v", b, err)
	}
}

// ageBlobs makes every blob older than the grace period.
func ageBlobs(t *testing.T, s *Server) {
	past := time.Now().Add(-2 * blobGrace)
	filepath.WalkDir(filepath.Dir(s.staging)+"/blobs", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			os.Chtimes(p, past, past)
		}
		return err
	})
}
