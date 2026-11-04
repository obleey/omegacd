//go:build integration

package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/obleey/omegacd/internal/domain"
)

// dind is a docker:dind container serving its engine over TLS and, if openssh could be installed, over ssh.
type dind struct {
	id         string
	tls        domain.Host
	ssh        domain.Host // zero if sshd could not be set up
	knownHosts string
}

func dockerCmd(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func startDind(t *testing.T) dind {
	t.Helper()
	id, err := dockerCmd("run", "-d", "--privileged", "-e", "DOCKER_TLS_CERTDIR=/certs",
		"-p", "127.0.0.1::2376", "-p", "127.0.0.1::22", "docker:29-dind")
	if err != nil {
		t.Skipf("cannot start docker:dind: %v: %s", err, id)
	}
	id = id[strings.LastIndex(id, "\n")+1:] // skip pull progress on the first run
	t.Cleanup(func() { dockerCmd("rm", "-f", "-v", id) })
	port := func(p string) string {
		out, err := dockerCmd("port", id, p)
		if err != nil {
			t.Fatalf("docker port: %v: %s", err, out)
		}
		return out[strings.LastIndex(out, ":")+1:]
	}
	d := dind{id: id}
	certs := t.TempDir()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(certs, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		// the daemon starts listening only after the entrypoint has written all certs
		out, err := dockerCmd("exec", id, "docker", "version")
		if err == nil {
			if out, err = dockerCmd("cp", id+":/certs/client/.", certs); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Skipf("dind did not come up: %v: %s", err, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
	d.tls = domain.Host{Name: "dind-tls", URL: "tcp://127.0.0.1:" + port("2376"), WorkDir: "/srv/omegacd",
		TLSCA: read("ca.pem"), TLSCert: read("cert.pem"), TLSKey: read("key.pem")}

	// ssh: openssh in the dind container, root logs in with a generated key; dial-stdio uses the local socket.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	setup := `set -e
apk add --no-cache openssh-server >/dev/null
ln -sf /usr/local/bin/docker /usr/bin/docker
ssh-keygen -A >/dev/null
mkdir -p /root/.ssh && chmod 700 /root/.ssh
echo "$1" > /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys
sed -i 's/^root:[!*]*:/root::/' /etc/shadow
/usr/sbin/sshd
cat /etc/ssh/ssh_host_ed25519_key.pub`
	if out, err := dockerCmd("exec", id, "sh", "-c", setup, "sh", string(ssh.MarshalAuthorizedKey(sshPub))); err != nil {
		t.Logf("ssh setup failed, ssh:// untested: %v: %s", err, out)
	} else {
		sshPort := port("22")
		d.knownHosts = filepath.Join(t.TempDir(), "known_hosts")
		os.WriteFile(d.knownHosts, []byte("[127.0.0.1]:"+sshPort+" "+out[strings.LastIndex(out, "ssh-ed25519"):]+"\n"), 0o600)
		d.ssh = domain.Host{Name: "dind-ssh", URL: "ssh://root@127.0.0.1:" + sshPort, WorkDir: "/srv/omegacd-ssh",
			SSHPrivateKey: string(pem.EncodeToMemory(block))}
	}
	return d
}

func newRemoteEngine(t *testing.T, d dind) *Engine {
	t.Helper()
	hosts := []domain.Host{d.tls}
	if d.ssh.Name != "" {
		hosts = append(hosts, d.ssh)
	}
	e, err := New(Config{
		Hosts: func(_ context.Context, name string) (domain.Host, error) {
			for _, h := range hosts {
				if h.Name == name {
					return h, nil
				}
			}
			return domain.Host{}, domain.ErrNotFound
		},
		ListHosts:      func(context.Context) ([]domain.Host, error) { return hosts, nil },
		KnownHostsFile: d.knownHosts,
		KeyDir:         t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	deadline := time.Now().Add(60 * time.Second)
	for {
		st := e.HostStatus(context.Background(), d.tls.Name)
		if st.Connected {
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("dind engine not reachable: %s", st.Error)
		}
		time.Sleep(time.Second)
	}
	return e
}

func TestRemoteHost(t *testing.T) {
	d := startDind(t)
	e := newRemoteEngine(t, d)
	t.Run("tls", func(t *testing.T) { remoteLifecycle(t, e, d, d.tls) })
	t.Run("ssh", func(t *testing.T) {
		if d.ssh.Name == "" {
			t.Skip("sshd could not be set up in dind")
		}
		remoteLifecycle(t, e, d, d.ssh)
	})
}

func remoteLifecycle(t *testing.T, e *Engine, d dind, h domain.Host) {
	ctx := context.Background()
	if st := e.HostStatus(ctx, h.Name); !st.Connected || st.ServerVersion == "" || st.Name != h.Name {
		t.Fatalf("HostStatus = %+v", st)
	}
	app := domain.Application{Name: fmt.Sprintf("omegacd-it-%d", mrand.IntN(1e9)), Host: h.Name}
	t.Cleanup(func() { e.Remove(context.Background(), app) })
	remote := remoteAppDir(h, app.Name)
	inDind := func(args ...string) (string, error) { return dockerCmd(append([]string{"exec", d.id}, args...)...) }

	seen := make(chan [2]string, 256)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.Watch(wctx, func(host, p string) { seen <- [2]string{host, p} })

	dir := t.TempDir()
	write(t, dir, "compose.yaml", `
services:
  web:
    image: busybox:1.36
    command: ["sleep", "3600"]
    env_file: app.env
    configs: [{source: conf, target: /etc/app.conf}]
    secrets: [pw]
    volumes: ["./site.conf:/etc/site.conf:ro"]
configs:
  conf: {file: ./app.conf}
secrets:
  pw: {file: ./pw.txt}
`)
	write(t, dir, "app.env", "A=1\n")
	write(t, dir, "app.conf", "v1\n")
	write(t, dir, "site.conf", "s1\n")
	write(t, dir, "stale.txt", "old\n")
	os.WriteFile(filepath.Join(dir, "pw.txt"), []byte("secret\n"), 0o600)

	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if out, err := inDind("cat", remote+"/app.conf", remote+"/stale.txt"); err != nil || out != "v1\nold" {
		t.Fatalf("remote files = %q, %v", out, err)
	}
	if out, _ := inDind("stat", "-c", "%a", remote+"/pw.txt"); out != "600" {
		t.Errorf("remote secret mode = %s, want 600", out)
	}
	ls, err := e.Status(ctx, app)
	if err != nil || ls.Health != domain.Healthy || len(ls.Containers) != 1 {
		t.Fatalf("Status = %+v, %v", ls, err)
	}
	webID := ls.Containers[0].ID
	if diffs, err := e.Diff(ctx, app, dir, ""); err != nil || len(diffs) != 0 {
		t.Fatalf("Diff after sync = %+v, %v; want empty", diffs, err)
	}
	stage := t.TempDir()
	if out, err := exec.Command("cp", "-a", dir+"/.", stage).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if diffs, err := e.Diff(ctx, app, stage, "/var/lib/omegacd/apps/"+app.Name); err != nil || len(diffs) != 0 {
		t.Fatalf("Diff from staging = %+v, %v; want empty", diffs, err)
	}
	if exists, managed, err := e.ProjectExists(ctx, h.Name, app.Name); err != nil || !exists || !managed {
		t.Errorf("ProjectExists = %v, %v, %v", exists, managed, err)
	}
	if exists, _, _ := e.ProjectExists(ctx, domain.LocalHost, app.Name); exists {
		t.Error("project exists on local host too")
	}

	inode := func(f string) string { out, _ := inDind("stat", "-c", "%i", remote+"/"+f); return out }
	confInode, siteInode := inode("app.conf"), inode("site.conf")
	write(t, dir, "app.conf", "v2\n")
	os.Remove(filepath.Join(dir, "stale.txt"))
	if diffs, _ := e.Diff(ctx, app, dir, ""); len(diffs) != 1 || diffs[0].Change != "changed" {
		t.Fatalf("Diff after config change = %+v", diffs)
	}
	if err := e.Sync(ctx, app, dir, domain.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	ls, _ = e.Status(ctx, app)
	if len(ls.Containers) != 1 || ls.Containers[0].ID == webID || ls.Health != domain.Healthy {
		t.Errorf("after config change: %+v; want web recreated and Healthy", ls)
	}
	if out, err := inDind("docker", "exec", ls.Containers[0].ID, "cat", "/etc/app.conf"); out != "v2" {
		t.Errorf("config in container = %q, %v", out, err)
	}
	if _, err := inDind("test", "-e", remote+"/stale.txt"); err == nil {
		t.Error("stale file survived sync")
	}
	if got := inode("app.conf"); got != confInode {
		t.Errorf("changed file got a new inode: %s -> %s", confInode, got)
	}
	if got := inode("site.conf"); got != siteInode {
		t.Errorf("unchanged file got a new inode: %s -> %s", siteInode, got)
	}
	if diffs, err := e.Diff(ctx, app, dir, ""); err != nil || len(diffs) != 0 {
		t.Errorf("Diff after re-sync = %+v, %v", diffs, err)
	}

	var buf strings.Builder
	if err := e.Logs(ctx, app.Host, ls.Containers[0].ID, false, 0, &buf); err != nil {
		t.Errorf("Logs on remote container: %v", err)
	}

	deadline := time.After(10 * time.Second)
watch:
	for {
		select {
		case ev := <-seen:
			if ev == [2]string{h.Name, app.Name} {
				break watch
			}
		case <-deadline:
			t.Error("Watch reported no event for the remote project")
			break watch
		}
	}

	if err := e.Remove(ctx, app); err != nil {
		t.Fatal(err)
	}
	if exists, _, err := e.ProjectExists(ctx, h.Name, app.Name); err != nil || exists {
		t.Errorf("ProjectExists after Remove = %v, %v", exists, err)
	}
	if _, err := inDind("test", "-e", remote); err == nil {
		t.Error("remote app dir survived Remove")
	}
}
