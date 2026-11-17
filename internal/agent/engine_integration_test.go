//go:build integration

package agent

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/enginetest"
)

// fail fails every call to method on pattern.
func fail(method, pattern string) func(*http.Request) int {
	return func(r *http.Request) int {
		if enginetest.Is(r, method, pattern) {
			return http.StatusInternalServerError
		}
		return 0
	}
}

// DetectEngine fails with the engine's answer when it can't tell its version or its settings (Q142).
func TestDetectEngineFails(t *testing.T) {
	for _, pattern := range []string{"/version", "/info"} {
		t.Run(pattern, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", enginetest.Faulty(t, fail("GET", pattern)))
			if _, err := DetectEngine(t.Context()); err == nil || !strings.Contains(err.Error(), "failed on purpose") {
				t.Fatalf("%v", err)
			}
		})
	}
}

// reclaim's helper container failing at each step fails the reclaim, saying which folder: outside a container it can't
// find its own image; an image that isn't there; the engine refusing to start it or to wait for it; and its script
// failing (a folder under a file).
func TestReclaimFails(t *testing.T) {
	ctx := t.Context()
	root := os.Getenv("OMEGACD_IT_ROOT")
	if root == "" {
		root = t.TempDir()
	}
	root, err := os.MkdirTemp(root, "reclaim-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := filepath.Join(root, "web")
	os.MkdirAll(dir, 0o755)
	const image = "docker.io/library/busybox:1.36"
	c, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if res, err := c.ImagePull(ctx, image, client.ImagePullOptions{}); err != nil || res.Wait(ctx) != nil {
		t.Fatalf("pull %s: %v", image, err)
	}

	if err := reclaim(ctx, root, dir, ""); err == nil {
		t.Error("found its own image outside a container")
	}
	if err := reclaim(ctx, root, dir, "omegacd.invalid/no-such-image:1"); err == nil || !strings.Contains(err.Error(), "reclaim "+dir) {
		t.Errorf("an image that isn't there: %v", err)
	}
	os.WriteFile(filepath.Join(root, "file"), nil, 0o644)
	if err := reclaim(ctx, root, filepath.Join(root, "file", "x"), image); err == nil || !strings.Contains(err.Error(), "exit") {
		t.Errorf("its script failing: %v", err)
	}
	for _, pattern := range []string{"/containers/*/start", "/containers/*/wait"} {
		t.Run(pattern, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", enginetest.Faulty(t, fail("POST", pattern)))
			if err := reclaim(ctx, root, dir, image); err == nil || !strings.Contains(err.Error(), "failed on purpose") {
				t.Fatalf("%v", err)
			}
		})
	}
}
