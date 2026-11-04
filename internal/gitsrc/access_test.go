package gitsrc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
