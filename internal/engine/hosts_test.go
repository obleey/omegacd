package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
)

func TestSSHFlags(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if _, err := sshFlags("/k/id", ""); err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("empty known_hosts: err = %v", err)
	}
	if _, err := sshFlags("/k/id", kh); err == nil {
		t.Error("missing known_hosts file: err = nil")
	}
	os.WriteFile(kh, nil, 0o600)
	got, err := sshFlags("/k/id", kh)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-i", "/k/id", "-o", "UserKnownHostsFile=" + kh, "-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes"}
	if !slices.Equal(got, want) {
		t.Errorf("sshFlags = %q, want %q", got, want)
	}
}

func TestDialSSHRequiresKnownHosts(t *testing.T) {
	e := &Engine{cfg: Config{KeyDir: t.TempDir()}}
	_, err := e.dial(domain.Host{Name: "box", URL: "ssh://deploy@box", SSHPrivateKey: "KEY"})
	if err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("dial without known_hosts: err = %v", err)
	}
	fi, err := os.Stat(filepath.Join(e.cfg.KeyDir, "box", "id"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v, %v; want mode 0600", fi, err)
	}
	if _, err := e.dial(domain.Host{Name: "../x", URL: "tcp://x:2375"}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("dial with path in name: err = %v", err)
	}
	if _, err := e.dial(domain.Host{Name: "x", URL: "http://x"}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("dial http://: err = %v", err)
	}
}

func TestConnRouting(t *testing.T) {
	local := &hostConn{host: domain.Host{Name: domain.LocalHost}}
	var asked []string
	e := &Engine{conns: map[string]*hostConn{domain.LocalHost: local}, cfg: Config{
		Hosts: func(_ context.Context, name string) (domain.Host, error) {
			asked = append(asked, name)
			return domain.Host{}, domain.ErrNotFound
		},
	}}
	for _, h := range []string{"", domain.LocalHost} {
		if hc, err := e.conn(context.Background(), h); err != nil || hc != local {
			t.Errorf("conn(%q) = %v, %v; want local", h, hc, err)
		}
	}
	if _, err := e.conn(context.Background(), "edge"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("conn(unknown) err = %v", err)
	}
	if !slices.Equal(asked, []string{"edge"}) {
		t.Errorf("Hosts called with %q; want only edge", asked)
	}
}

func TestRemoteAppDir(t *testing.T) {
	web, alloy := domain.Application{Name: "web"}, domain.Application{Name: "alloy", Parent: "platform"}
	for _, tt := range []struct {
		h    domain.Host
		app  domain.Application
		want string
	}{
		{domain.Host{}, web, "/var/lib/omegacd/web"},
		{domain.Host{WorkDir: "/srv/cd/"}, web, "/srv/cd/web"},
		{domain.Host{WorkDir: "/opt/services"}, alloy, "/opt/services/platform/alloy"}, // a Child Application: in its App of Apps' folder
	} {
		if got := remoteAppDir(tt.h, tt.app); got != tt.want {
			t.Errorf("remoteAppDir(%q, %s) = %s, want %s", tt.h.WorkDir, tt.app.Dir(), got, tt.want)
		}
	}
}

// Diff loads from the local working dir or a staging copy; on a remote Host both must end up with remote paths.
func TestPlaceForRemote(t *testing.T) {
	remote := &hostConn{host: domain.Host{Name: "edge", WorkDir: "/srv"}}
	app := domain.Application{Name: "web"}
	for _, asDir := range []string{"", "/var/lib/omegacd/web"} {
		dir := t.TempDir()
		write := func(name, content string) { os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644) }
		write("compose.yaml", `
services:
  web:
    image: busybox
    env_file: app.env
    volumes: ["./site.conf:/etc/site.conf", "/var/log:/host-log"]
    configs: [conf]
    secrets: [pw]
configs:
  conf: {file: ./app.conf}
secrets:
  pw: {file: ./pw.txt}
`)
		for _, f := range []string{"app.env", "site.conf", "app.conf", "pw.txt"} {
			write(f, "X=1\n")
		}
		p, err := (&Engine{}).load(context.Background(), app, dir)
		if err != nil {
			t.Fatal(err)
		}
		placeFor(p, remote, app, dir, asDir)
		s := p.Services["web"]
		got := []string{p.WorkingDir, s.EnvFiles[0].Path, s.Volumes[0].Source, s.Volumes[1].Source, p.Configs["conf"].File, p.Secrets["pw"].File}
		want := []string{"/srv/web", "/srv/web/app.env", "/srv/web/site.conf", "/var/log", "/srv/web/app.conf", "/srv/web/pw.txt"}
		if !slices.Equal(got, want) {
			t.Errorf("asDir %q: paths = %q, want %q", asDir, got, want)
		}
	}
}

func TestOnHostNamesHostPaths(t *testing.T) {
	app := domain.Application{Name: "alloy", Parent: "helion"}
	err := fmt.Errorf("%w: open /data/work/helion/alloy/compose.yml: no such file", domain.ErrInvalid)
	got := onHost(domain.Host{Name: "helion", WorkDir: "/opt/services"}, app, "/data/work/helion/alloy", err)
	if got.Error() != "invalid: open /opt/services/helion/alloy/compose.yml: no such file" || !errors.Is(got, domain.ErrInvalid) {
		t.Fatalf("got %v", got)
	}
	if local := onHost(domain.Host{Name: domain.LocalHost}, app, "/data/work/helion/alloy", err); local != err {
		t.Fatalf("local: %v", local)
	}
}
