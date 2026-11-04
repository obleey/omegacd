package gitsrc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/obleey/omegacd/internal/domain"
)

func TestSSHHostKeyVerification(t *testing.T) {
	_, clientKey, _ := ed25519.GenerateKey(rand.Reader)
	block, err := gossh.MarshalPrivateKey(clientKey, "")
	if err != nil {
		t.Fatal(err)
	}
	creds := func(context.Context) ([]domain.RepoCredential, error) {
		return []domain.RepoCredential{{URLPrefix: "git@example.com:", SSHPrivateKey: string(pem.EncodeToMemory(block))}}, nil
	}
	const url = "git@example.com:org/repo.git"

	g := New(t.TempDir(), creds)
	if _, err := g.auth(t.Context(), url); err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("ssh without known_hosts: err = %v, want refusal", err)
	}

	hostPub, _, _ := ed25519.GenerateKey(rand.Reader)
	trusted, _ := gossh.NewPublicKey(hostPub)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := gossh.NewPublicKey(otherPub)
	g.KnownHostsFile = filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(g.KnownHostsFile, []byte("example.com "+string(gossh.MarshalAuthorizedKey(trusted))), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := g.auth(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	cb := auth.(*gitssh.PublicKeys).HostKeyCallback
	addr := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 22}
	if err := cb("example.com:22", addr, trusted); err != nil {
		t.Errorf("trusted host key rejected: %v", err)
	}
	if err := cb("example.com:22", addr, other); err == nil {
		t.Error("unknown host key accepted")
	}
}
