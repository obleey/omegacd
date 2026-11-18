package source

import (
	"context"

	"github.com/obleey/omegacd/internal/domain"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/obleey/omegacd/internal/logging"
	sourcev1 "github.com/obleey/omegacd/internal/proto/source/v1"
)

// Q156: the Source Server says its version, its cached Repositories and its caches' size; the same over gRPC.
func TestStatus(t *testing.T) {
	f, _, _, _ := setup(t)
	ctx := context.Background()
	s := server(t)
	st, err := s.Status(ctx)
	if err != nil || st.Version == "" || st.Repositories != 0 {
		t.Fatalf("empty: %+v %v", st, err)
	}
	if _, err := s.Render(ctx, appSpec(t, s, f.dir)); err != nil {
		t.Fatal(err)
	}
	st, err = s.Status(ctx)
	if err != nil || st.Repositories != 1 || st.RepoCacheBytes == 0 || st.BlobCacheBytes == 0 {
		t.Fatalf("after a clone and a render: %+v %v", st, err)
	}

	url, dir := standalone(t, s)
	remote, err := Dial(url, dir, logging.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if got, err := remote.Status(ctx); err != nil || got != st {
		t.Fatalf("over gRPC: %+v %v, want %+v", got, err, st)
	}
}

// A cache that can't be read fails the status, saying so; a missing one is empty.
func TestStatusUnreadable(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	if err := os.WriteFile(s.cacheDir, nil, 0o600); err != nil { // a file where the clones go
		t.Fatal(err)
	}
	if _, err := s.Status(ctx); err == nil || !strings.Contains(err.Error(), "source server status") {
		t.Fatalf("a file for the cache: %v", err)
	}
	if n, err := dirSize(filepath.Join(t.TempDir(), "gone")); err != nil || n != 0 {
		t.Fatalf("missing: %d %v", n, err)
	}
	if os.Getuid() == 0 {
		return // root reads anything; the suite runs as a normal user
	}
	s = server(t)
	locked := filepath.Join(s.dir, "blobs", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if _, err := s.Status(ctx); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("an unreadable blob folder: %v", err)
	}
}

// A Source Server older than Status answers "unimplemented": its version is unknown, not an error (Q156).
func TestStatusOfAnOlderSourceServer(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateCerts(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := ServerTLS(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	sourcev1.RegisterSourceServiceServer(g, sourcev1.UnimplementedSourceServiceServer{})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go g.Serve(l)
	t.Cleanup(g.Stop)
	c, err := Dial("https://"+l.Addr().String(), dir, logging.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if st, err := c.Status(context.Background()); err != nil || st.Version != "" {
		t.Fatalf("%+v %v", st, err)
	}
	g.Stop() // and one that's gone is an error
	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("no error from a stopped Source Server")
	}
}

// Q188: a standalone Source Server's log lines carry the app and Sync Operation of the Control Plane's call.
func TestTrail(t *testing.T) {
	f, _, _, _ := setup(t)
	logs := &logging.Buffer{}
	s := server(t)
	s.Log, _ = logging.New(logs, "debug", "text")
	url, dir := standalone(t, s)
	remote, err := Dial(url, dir, logging.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	ctx := logging.With(context.Background(), "app", "web", "op", 7)
	rendered, err := remote.Render(ctx, appSpec(t, remote, f.dir, ctx))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range rendered.Files {
		if _, err := remote.Blob(ctx, file.SHA256); err != nil { // a stream
			t.Fatal(err)
		}
	}
	for _, want := range []string{`msg=resolved`, `msg=rendered`} {
		line := ""
		for l := range strings.SplitSeq(logs.String(), "\n") {
			if strings.Contains(l, want) {
				line = l
			}
		}
		if !strings.Contains(line, "app=web") || !strings.Contains(line, "op=7") {
			t.Errorf("%s: %q\n%s", want, line, logs.String())
		}
	}
	if _, err := remote.Resolve(context.Background(), f.dir, "main"); err != nil { // no trail: none sent
		t.Fatal(err)
	}
}

// appSpec resolves main and renders the fixture's app folder at it, in ctx (one with a trail, or none).
func appSpec(t *testing.T, s domain.Source, repo string, ctxs ...context.Context) domain.RenderSpec {
	t.Helper()
	ctx := append(ctxs, context.Background())[0]
	head, err := s.Resolve(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	return domain.RenderSpec{RepoURL: repo, Revision: head, Path: "app"}
}
