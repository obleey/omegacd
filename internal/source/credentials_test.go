package source

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
)

const token = "s3cr3t-token"

// privateRepo answers like a Git host that wants owner:token.
func privateRepo(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "owner" || p != token {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		http.Error(w, "not a real repository", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCredentials(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	url := privateRepo(t)
	if a, _ := s.CheckAccess(ctx, url+"/owner/repo.git"); a.Access != "denied" {
		t.Fatalf("without a credential: %+v", a)
	}
	c, err := s.SaveCredential(ctx, domain.RepoCredential{URLPrefix: url + "/owner/", Username: "owner", Password: token})
	if err != nil || c.ID == 0 || !c.HasPassword || c.Password != "" {
		t.Fatalf("Save = %+v, %v", c, err)
	}
	// the access check uses it: past authentication, the fake host's error shows
	if a, _ := s.CheckAccess(ctx, url+"/owner/repo.git"); a.Access != "error" || strings.Contains(a.Message, token) {
		t.Fatalf("with the credential: %+v", a)
	}
	if _, err := s.SaveCredential(ctx, domain.RepoCredential{URLPrefix: url + "/owner/"}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate prefix: %v", err)
	}
	if _, err := s.SaveCredential(ctx, domain.RepoCredential{ID: 99, URLPrefix: "x"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown ID: %v", err)
	}
	if _, err := s.SaveCredential(ctx, domain.RepoCredential{URLPrefix: " "}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("no prefix: %v", err)
	}
	// an update without a password keeps it
	if _, err := s.SaveCredential(ctx, domain.RepoCredential{ID: c.ID, URLPrefix: url + "/owner/", Username: "owner"}); err != nil {
		t.Fatal(err)
	}
	other, _ := s.SaveCredential(ctx, domain.RepoCredential{URLPrefix: "https://gitlab.com/", SSHPrivateKey: "KEY"})

	// on disk: encrypted with a key of its own, owner-only
	b, _ := os.ReadFile(filepath.Join(dir, "credentials.age"))
	if len(b) == 0 || strings.Contains(string(b), token) || strings.Contains(string(b), "gitlab") {
		t.Fatal("credentials stored in plain text")
	}
	if fi, err := os.Stat(filepath.Join(dir, "source.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("source.key: %v %v", fi, err)
	}

	// a restart keeps them, secrets included; listing never shows a secret
	s, err = NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListCredentials(ctx)
	if len(list) != 2 || list[0].Password != "" || !list[0].HasPassword || list[1].SSHPrivateKey != "" || !list[1].HasSSHKey {
		t.Fatalf("after restart: %+v", list)
	}
	if a, _ := s.CheckAccess(ctx, url+"/owner/repo.git"); a.Access != "error" {
		t.Fatalf("after restart, the password was lost: %+v", a)
	}
	if err := s.DeleteCredential(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCredential(ctx, other.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if list, _ := s.ListCredentials(ctx); len(list) != 1 {
		t.Fatalf("after delete: %+v", list)
	}

	// another key can't read them, and says so instead of starting empty
	os.Remove(filepath.Join(dir, "source.key"))
	if _, err := NewServer(dir); err == nil {
		t.Fatal("started with credentials it can't decrypt")
	}
}
