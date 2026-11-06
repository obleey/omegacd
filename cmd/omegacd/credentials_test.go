package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/source"
	"github.com/obleey/omegacd/internal/store"
)

// Credentials an older version kept in the Control Plane's database move to the Source Server and leave no secret
// behind; losing the database then loses no credential.
func TestMoveRepoCredentials(t *testing.T) {
	ctx := t.Context()
	data := t.TempDir()
	id, _ := age.GenerateX25519Identity()
	db := filepath.Join(data, "omegacd.db")
	st, err := store.Open(db, crypto.NewSealer(id))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []domain.RepoCredential{
		{URLPrefix: "https://github.com/", Username: "me", Password: "ghp_secret"},
		{URLPrefix: "git@gitlab.com:", SSHPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----"},
	} {
		if _, err := st.SaveRepoCredential(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	src, err := source.NewServer(filepath.Join(data, "source"))
	if err != nil {
		t.Fatal(err)
	}
	// one already moved by a run that stopped before deleting it
	if _, err := src.SaveCredential(ctx, domain.RepoCredential{URLPrefix: "https://github.com/", Password: "ghp_secret"}); err != nil {
		t.Fatal(err)
	}
	if err := moveRepoCredentials(ctx, st, src); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.ListRepoCredentials(ctx); len(left) != 0 {
		t.Fatalf("still in the Control Plane's database: %+v", left)
	}
	st.Close()
	b, _ := os.ReadFile(db)
	if strings.Contains(string(b), "github.com") || strings.Contains(string(b), "gitlab.com") {
		t.Fatal("the Control Plane's database still holds a credential")
	}

	os.Remove(db) // the Control Plane's database is lost
	src, err = source.NewServer(filepath.Join(data, "source"))
	if err != nil {
		t.Fatal(err)
	}
	list, _ := src.ListCredentials(ctx)
	if len(list) != 2 || !list[0].HasPassword || !list[1].HasSSHKey {
		t.Fatalf("Source Server holds %+v", list)
	}
}
