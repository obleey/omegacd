package agent

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func newWorkDir(t *testing.T) (*WorkDir, *source, string) {
	c, db, state := openCache(t, 10)
	return NewWorkDir(filepath.Join(t.TempDir(), "work"), db, c), &source{}, state
}

func (w *WorkDir) deploy(t *testing.T, src *source, a App, rev string, files map[string]string) error {
	t.Helper()
	r := src.revision(a.Name, rev, files)
	if err := w.cache.Add(t.Context(), r, src.fetch); err != nil {
		t.Fatal(err)
	}
	a.Revision = rev
	return w.Write(t.Context(), a, r)
}

func inode(t *testing.T, path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func TestWriteInPlace(t *testing.T) {
	w, src, _ := newWorkDir(t)
	web := App{Name: "web"}
	if err := w.deploy(t, src, web, "r1", map[string]string{"compose.yaml": "v1", "nginx.conf": "a"}); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(w.Root, "web", "nginx.conf")
	before := inode(t, conf) // what a single-file bind mount holds on to
	if err := w.deploy(t, src, web, "r2", map[string]string{"compose.yaml": "v2", "nginx.conf": "b"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(conf); string(b) != "b" || inode(t, conf) != before {
		t.Fatalf("nginx.conf = %q, inode %d → %d: want the new content in the same file", b, before, inode(t, conf))
	}
}

func TestWriteRefusesForeignFolder(t *testing.T) {
	w, src, _ := newWorkDir(t)
	foreign := filepath.Join(w.Root, "alloy")
	os.MkdirAll(foreign, 0o750)
	os.WriteFile(filepath.Join(foreign, "data.db"), []byte("someone's data"), 0o600)
	err := w.deploy(t, src, App{Name: "alloy"}, "r1", map[string]string{"compose.yaml": "x"})
	if err == nil || !strings.Contains(err.Error(), "refusing to sync into") {
		t.Fatalf("synced into a foreign folder: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(foreign, "data.db")); string(b) != "someone's data" {
		t.Fatal("the foreign folder's files were touched")
	}
}

func TestWriteKeepsItsFolderAcrossRestarts(t *testing.T) {
	w, src, state := newWorkDir(t)
	if err := w.deploy(t, src, App{Name: "web"}, "r1", map[string]string{"compose.yaml": "x"}); err != nil {
		t.Fatal(err)
	}
	w.db.Close()
	db := openDB(t, filepath.Join(state, "agent.db")) // the Agent restarted: the folder holds files, and is still its own
	defer db.Close()
	w.db, w.cache.db = db, db
	if err := w.deploy(t, src, App{Name: "web"}, "r2", map[string]string{"compose.yaml": "y"}); err != nil {
		t.Fatalf("own folder refused after a restart: %v", err)
	}
}

func TestWriteChildApplications(t *testing.T) {
	w, src, _ := newWorkDir(t)
	for _, name := range []string{"web", "api"} {
		if err := w.deploy(t, src, App{Name: name, Parent: "root"}, "r1", map[string]string{"compose.yaml": name}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(w.Root, "root", "api", "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	// web leaves the App of Apps: its old folder goes, the App of Apps keeps api's
	if err := w.deploy(t, src, App{Name: "web"}, "r2", map[string]string{"compose.yaml": "web"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "root", "web")); !os.IsNotExist(err) {
		t.Errorf("old folder left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "web", "compose.yaml")); err != nil {
		t.Error(err)
	}
}

func TestPathParity(t *testing.T) {
	bind := func(src, dst string) container.MountPoint {
		return container.MountPoint{Type: "bind", Source: src, Destination: dst}
	}
	for name, tc := range map[string]struct {
		mounts []container.MountPoint
		err    string
	}{
		"same path":                {[]container.MountPoint{bind("/opt/services", "/opt/services")}, ""},
		"parent mounted, same":     {[]container.MountPoint{bind("/opt", "/opt"), bind("/var/run/docker.sock", "/var/run/docker.sock")}, ""},
		"different host path":      {[]container.MountPoint{bind("/srv/omegacd", "/opt/services")}, "is /srv/omegacd on the host"},
		"not mounted":              {[]container.MountPoint{bind("/data", "/data")}, "not mounted from the host"},
		"a volume":                 {[]container.MountPoint{{Type: "volume", Name: "work", Destination: "/opt/services"}}, "volume mount"},
		"the closest mount counts": {[]container.MountPoint{bind("/opt", "/opt"), bind("/elsewhere", "/opt/services")}, "is /elsewhere"},
	} {
		err := pathParity("/opt/services", tc.mounts)
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%s: %v, want %q", name, err, tc.err)
		}
		if err != nil && !strings.Contains(err.Error(), "-v /opt/services:/opt/services") {
			t.Errorf("%s: the message doesn't say how to fix it: %v", name, err)
		}
	}
}
