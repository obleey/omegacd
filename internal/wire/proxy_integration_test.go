//go:build integration

package wire

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// proxyTimeout stands in for a proxy's 60 s idle timeout, so the tests take seconds.
const proxyTimeout = 2 * time.Second

// throughNginx serves connections (handled by handle) behind a real nginx container that cuts a connection idle for
// proxyTimeout, as reverse proxies do, and returns its ws:// address and the container.
func throughNginx(t *testing.T, handle func(*Conn)) (url, container string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		skipUnlessRequired(t, "no docker CLI")
	}
	l, err := net.Listen("tcp", "0.0.0.0:0") // the container reaches the test through the Docker host
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" { // hostPort asking whether nginx is up
			return
		}
		c, err := Accept(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		handle(c)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	port := l.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	conf := fmt.Sprintf(`events {}
http {
  server {
    listen 80;
    location / {
      proxy_pass http://host.docker.internal:%d;
      proxy_http_version 1.1;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection "upgrade";
      proxy_read_timeout %ds;
      proxy_send_timeout %ds;
    }
  }
}
`, port, int(proxyTimeout.Seconds()), int(proxyTimeout.Seconds()))
	if err := os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	// a fixed port, as a real proxy has: a container's random one changes when it restarts
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyPort := free.Addr().(*net.TCPAddr).Port
	free.Close()
	out, err := exec.Command("docker", "run", "-d", "--rm", "--add-host", "host.docker.internal:host-gateway",
		"-p", fmt.Sprintf("127.0.0.1:%d:80", proxyPort), "-v", filepath.Join(dir, "nginx.conf")+":/etc/nginx/nginx.conf:ro",
		"docker.io/library/nginx:1.29-alpine").CombinedOutput()
	if err != nil {
		t.Fatalf("nginx: %v: %s", err, out)
	}
	container = strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", container).Run() })
	return "ws://" + hostPort(t, container), container
}

// hostPort is where the container's port 80 is published, once nginx answers there: an HTTP answer, not just an
// accepted connection, which Docker's port forwarding gives before nginx in the container listens.
func hostPort(t *testing.T, container string) string {
	t.Helper()
	client := http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		out, err := exec.Command("docker", "port", container, "80").Output()
		if addr := strings.TrimSpace(strings.Split(string(out), "\n")[0]); err == nil && addr != "" {
			if res, err := client.Get("http://" + addr + "/"); err == nil {
				res.Body.Close()
				return addr
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("nginx never answered: %v", err)
		}
	}
}

// Through a real reverse proxy: pings keep a quiet connection open for many times the proxy's idle timeout (Q51),
// while without them the proxy cuts it, which shows the timeout is real.
func TestQuietConnectionThroughProxy(t *testing.T) {
	ctx := t.Context()
	url, _ := throughNginx(t, func(c *Conn) {
		for {
			e, err := c.Recv(ctx)
			if err != nil {
				return
			}
			c.Send(ctx, e)
		}
	})

	c, err := dial(ctx, url, nil, nil, proxyTimeout/4)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() { // read, as the Agent does: it answers pongs
		for {
			if _, err := c.Recv(ctx); err != nil {
				return
			}
		}
	}()
	select {
	case <-c.Done():
		t.Fatalf("the proxy cut a pinged connection: %v", c.Err())
	case <-time.After(4 * proxyTimeout):
	}
	if err := c.Send(ctx, hello("still here")); err != nil {
		t.Fatal(err)
	}

	quiet, err := dial(ctx, url, nil, nil, time.Hour) // no pings
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	go func() {
		for {
			if _, err := quiet.Recv(ctx); err != nil {
				return
			}
		}
	}()
	select {
	case <-quiet.Done():
	case <-time.After(4 * proxyTimeout):
		t.Fatal("the proxy never cut a connection without pings: the test proves nothing")
	}
}

// The proxy restarts: the Agent's side dials again and is served on a new connection.
func TestReconnectThroughProxyRestart(t *testing.T) {
	backoff = func(int) time.Duration { return 200 * time.Millisecond }
	t.Cleanup(func() { backoff = Backoff })
	var served atomic.Int32
	url, container := throughNginx(t, func(c *Conn) {
		served.Add(1)
		c.Recv(context.Background())
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go Keep(ctx, func(ctx context.Context) (*Conn, error) { return dial(ctx, url, nil, nil, proxyTimeout/4) },
		func(ctx context.Context, c *Conn) error {
			_, err := c.Recv(ctx)
			return err
		}, nil)
	wait := func(n int32) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); served.Load() < n; time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("connections served: %d, want %d", served.Load(), n)
			}
		}
	}
	wait(1)
	if out, err := exec.Command("docker", "restart", "-t", "0", container).CombinedOutput(); err != nil {
		t.Fatalf("restart nginx: %v: %s", err, out)
	}
	hostPort(t, container) // nginx answers again, on the same address
	wait(2)
}

func skipUnlessRequired(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("OMEGACD_TEST_ALL") != "" {
		t.Fatalf("make test-all requires this test to run: "+format, args...)
	}
	t.Skipf(format, args...)
}
