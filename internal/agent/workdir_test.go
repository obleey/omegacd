package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/obleey/omegacd/internal/domain"
)

func newWorkDir(t *testing.T) (*WorkDir, *source, string) {
	c, db, state := openCache(t, 10)
	return NewWorkDir(filepath.Join(t.TempDir(), "work"), db, c, nil), &source{}, state
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

// A deployed folder is complete although decrypting replaced x.sops.env with x.env; a wiped one isn't, and neither is
// one where Docker made a directory in place of a file.
func TestComplete(t *testing.T) {
	dir := t.TempDir()
	r := Revision{Files: []domain.File{{Path: "compose.yaml"}, {Path: "conf/app.conf"}, {Path: ".sops.env"}}}
	os.MkdirAll(filepath.Join(dir, "conf"), 0o755)
	for _, f := range []string{"compose.yaml", "conf/app.conf", ".env"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
	}
	if !complete(dir, r) {
		t.Fatal("a deployed folder counts as incomplete")
	}
	os.Remove(filepath.Join(dir, "conf/app.conf"))
	os.Mkdir(filepath.Join(dir, "conf/app.conf"), 0o755) // Docker's placeholder
	if complete(dir, r) {
		t.Fatal("a placeholder directory counts as the file")
	}
	if complete(t.TempDir(), r) {
		t.Fatal("an empty folder counts as complete")
	}
}

// Q57: an Application in a second allowed root deploys there (as a Child too); one outside every allowed root is
// refused before a file is written; moving between roots leaves no folder behind, and never removes a root.
func TestWriteInItsOwnWorkDir(t *testing.T) {
	w, src, _ := newWorkDir(t)
	other := filepath.Join(t.TempDir(), "srv")
	w.Allow = []string{other}
	files := map[string]string{"compose.yaml": "services: {}\n"}

	web := App{Name: "web", WorkDir: filepath.Join(other, "apps")} // under an allowed root; made when missing
	if err := w.deploy(t, src, web, "r1", files); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "apps", "web", "compose.yaml")); err != nil {
		t.Fatalf("not in its own work dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "web")); err == nil {
		t.Fatal("also in the Host's work dir")
	}
	kid := App{Name: "api", Parent: "root", WorkDir: other}
	if err := w.deploy(t, src, kid, "r1", files); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "root", "api", "compose.yaml")); err != nil {
		t.Fatalf("a Child in its own work dir: %v", err)
	}

	for _, bad := range []string{"/etc", filepath.Join(t.TempDir(), "elsewhere"), "relative/dir", other + "/../escape"} {
		evil := App{Name: "evil", WorkDir: bad}
		err := w.deploy(t, src, evil, "r1", files)
		if !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("work dir %q: %v", bad, err)
		}
		if _, serr := os.Stat(w.Dir(evil)); serr == nil {
			t.Fatalf("work dir %q: a folder was written", bad)
		}
	}
	if err := w.deploy(t, src, App{Name: "evil", WorkDir: "/etc"}, "r1", files); err == nil ||
		!strings.Contains(err.Error(), "OMEGACD_AGENT_WORKDIR_ALLOW: "+other) {
		t.Fatalf("the refusal names the setting and its roots: %v", err)
	}

	web.WorkDir = "" // back to the Host's work dir
	if err := w.deploy(t, src, web, "r2", files); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "apps", "web")); err == nil {
		t.Fatal("moving between roots left the old folder behind")
	}
	if _, err := os.Stat(filepath.Join(other, "apps")); err != nil {
		t.Fatal("moving removed the root it named")
	}
	if _, err := os.Stat(filepath.Join(w.Root, "web", "compose.yaml")); err != nil {
		t.Fatalf("not moved: %v", err)
	}
}

// A root the Agent's user can't create fails with the folder and the command that makes it once.
func TestWorkDirItCannotCreate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can create any folder")
	}
	w, src, _ := newWorkDir(t)
	locked := filepath.Join(t.TempDir(), "locked")
	os.Mkdir(locked, 0o500)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	w.Allow = []string{locked}
	err := w.deploy(t, src, App{Name: "web", WorkDir: filepath.Join(locked, "apps")}, "r1", map[string]string{"compose.yaml": "services: {}\n"})
	if err == nil || !strings.Contains(err.Error(), "sudo install -d -o ") || !strings.Contains(err.Error(), filepath.Join(locked, "apps")) {
		t.Fatalf("got %v", err)
	}
}
