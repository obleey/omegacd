// Package enginetest is for tests only: the container engine at DOCKER_HOST behind a proxy that fails the API calls a
// test picks (Q142), so the code that calls Docker is tested failing without a hook in it.
package enginetest

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path"
	"strings"
	"testing"
)

// Faulty starts a proxy to the engine at DOCKER_HOST (the default socket when unset) and returns its address, for
// DOCKER_HOST. answer gets every request and returns the status to fail it with, or 0 to pass it on; 200 fails it
// inside its stream, as a pull that broke off does. It stops with t.
func Faulty(t testing.TB, answer func(*http.Request) int) string {
	t.Helper()
	network, addr, _ := strings.Cut(cmp.Or(os.Getenv("DOCKER_HOST"), "unix:///var/run/docker.sock"), "://")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.Out.URL.Scheme, r.Out.URL.Host = "http", "engine" },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}},
		FlushInterval: -1, // event and log streams go through as they come
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := answer(r); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			msg := fmt.Sprintf("enginetest: %s %s failed on purpose", r.Method, r.URL.Path)
			if status == http.StatusOK { // a stream's own error line
				fmt.Fprintf(w, `{"errorDetail":{"message":%q},"error":%q}`, msg, msg)
				return
			}
			fmt.Fprintf(w, `{"message":%q}`, msg)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return "tcp://" + srv.Listener.Addr().String()
}

// Is reports whether r is method on the API path pattern, without the API version (path.Match: * is one segment),
// e.g. Is(r, "POST", "/containers/*/stop").
func Is(r *http.Request, method, pattern string) bool {
	p := r.URL.Path
	if strings.HasPrefix(p, "/v") {
		if i := strings.Index(p[1:], "/"); i >= 0 {
			p = p[i+1:]
		}
	}
	ok, _ := path.Match(pattern, p)
	return r.Method == method && ok
}
