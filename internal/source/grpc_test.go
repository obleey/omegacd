package source

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
	sourcev1 "github.com/obleey/omegacd/internal/proto/source/v1"
)

// standalone runs s as a Source Server on a free port with new certificates; it returns the URL and the cert dir.
func standalone(t *testing.T, s domain.Source) (string, string) {
	dir := t.TempDir()
	if err := GenerateCerts(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := ServerTLS(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	Serve(g, s)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go g.Serve(l)
	t.Cleanup(g.Stop)
	return "https://" + l.Addr().String(), dir
}

func TestGRPCMatchesInProcess(t *testing.T) {
	f, _, c1, _ := setup(t)
	f.commit(map[string]string{"apps/web.yaml": "name: web\nrepoURL: x\nhost: local\nautoSync: true\n", "bad/compose.yaml": "services:\n  b:\n    build: .\n"})
	local := server(t)
	url, dir := standalone(t, local)
	remote, err := Dial(url, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	ctx := context.Background()

	head, err := remote.Resolve(ctx, f.dir, "main")
	if want, _ := local.Resolve(ctx, f.dir, "main"); err != nil || head != want {
		t.Fatalf("Resolve = %s, %v; want %s", head, err, want)
	}
	if _, err := remote.Resolve(ctx, f.dir, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown revision: %v, want ErrNotFound", err)
	}
	remote.Invalidate(f.dir)
	for _, spec := range []domain.RenderSpec{
		{RepoURL: f.dir, Revision: c1, Path: "app"},
		{RepoURL: f.dir, Revision: head, Path: "apps", Apps: true},
	} {
		got, err := remote.Render(ctx, spec)
		want, _ := local.Render(ctx, spec)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Render %s:\n got %+v %v\nwant %+v", spec.Path, got, err, want)
		}
		for _, file := range got.Files {
			if b, err := remote.Blob(ctx, file.SHA256); err != nil || blob.Hash(b) != file.SHA256 {
				t.Fatalf("Blob %s: %v", file.Path, err)
			}
		}
	}
	bad, err := remote.Render(ctx, domain.RenderSpec{RepoURL: f.dir, Revision: head, Path: "bad"})
	if err != nil || !errors.Is(bad.Invalid, domain.ErrInvalid) {
		t.Errorf("build: Invalid = %v, %v", bad.Invalid, err)
	}
	if _, err := remote.Blob(ctx, blob.Hash([]byte("never stored"))); err == nil {
		t.Error("missing blob: no error")
	}
	if d, err := remote.Diff(ctx, f.dir, c1, head, "apps"); err != nil || d == "" {
		t.Errorf("Diff = %q, %v", d, err)
	}
	a, err := remote.CheckAccess(ctx, f.dir)
	if want, _ := local.CheckAccess(ctx, f.dir); err != nil || a != want {
		t.Errorf("CheckAccess = %+v, %v; want %+v", a, err, want)
	}
}

func TestGRPCRefusesClientsWithoutCertificate(t *testing.T) {
	url, dir := standalone(t, server(t))
	addr := url[len("https://"):]
	pool, err := caPool(dir)
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // a client certificate from another CA
	if err := GenerateCerts(other, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	otherCert, err := tls.LoadX509KeyPair(filepath.Join(other, clientCert), filepath.Join(other, clientKey))
	if err != nil {
		t.Fatal(err)
	}
	for name, certs := range map[string][]tls.Certificate{"no certificate": nil, "another CA's certificate": {otherCert}} {
		cfg := &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", Certificates: certs, MinVersion: tls.VersionTLS13}
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatal(err)
		}
		_, err = sourcev1.NewSourceServiceClient(conn).Resolve(context.Background(), &sourcev1.ResolveRequest{RepoUrl: "x", Ref: "main"})
		conn.Close()
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := GenerateCerts(dir, nil); err == nil {
		t.Error("GenerateCerts overwrote existing certificates")
	}
}
