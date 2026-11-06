package controller

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWorkDirLayout(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	work := r.c.cfg.WorkDir
	r.c.CreateApp(ctx, app("solo"))
	r.mustSync(t, "solo")
	r.c.CreateApp(ctx, parentApp("root"))
	r.g.define("aaaaaaaa", "name: web\nrepoURL: x\nhost: local\n")
	r.mustSync(t, "root")
	r.childrenSynced(t, "web")

	for path, want := range map[string]string{
		"solo/" + domain.MarkerFile:     "solo",
		"solo/compose.yaml":             "services: {}\n",
		"root/" + domain.MarkerFile:     "root", // the App of Apps' folder holds only its children's
		"root/web/" + domain.MarkerFile: "web",
		"root/web/compose.yaml":         "services: {}\n",
	} {
		if got := readFile(t, filepath.Join(work, path)); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(work, "root"))
	if len(entries) != 2 {
		t.Errorf("App of Apps folder holds %v; its definitions are only staged", entries)
	}
	if staged, _ := os.ReadDir(filepath.Join(work, ".staging")); len(staged) != 0 {
		t.Errorf("staging left behind: %v", staged)
	}
	if _, err := os.Stat(filepath.Join(work, domain.LegacyDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old layout's folder created: %v", err)
	}
}

func TestSyncRefusesFoldersOmegaCDDidNotCreate(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	work := r.c.cfg.WorkDir
	r.c.CreateApp(ctx, app("alloy"))
	dir := filepath.Join(work, "alloy")
	os.MkdirAll(dir, 0o750)
	os.WriteFile(filepath.Join(dir, "data.db"), []byte("host-only data"), 0o600)

	want := "refusing to sync into " + dir + `: the folder exists and OmegaCD did not create it for Application "alloy"; ` +
		"rename the Application, pick another work dir, or move the folder away"
	if op := r.syncNow(t, "alloy"); op.Phase != domain.OpFailed || op.Message != want {
		t.Fatalf("foreign folder: %s %q", op.Phase, op.Message)
	}
	os.WriteFile(filepath.Join(dir, domain.MarkerFile), []byte("other"), 0o644)
	if op := r.syncNow(t, "alloy"); op.Phase != domain.OpFailed || op.Message != want {
		t.Fatalf("folder of another Application: %s %q", op.Phase, op.Message)
	}
	if r.e.syncCount() != 0 || readFile(t, filepath.Join(dir, "data.db")) != "host-only data" {
		t.Fatal("synced into the foreign folder")
	}
	// deleting the Application leaves the folder alone too
	if err := r.c.DeleteApp(ctx, "alloy", true, false); err != nil || readFile(t, filepath.Join(dir, "data.db")) != "host-only data" {
		t.Fatalf("delete: %v", err)
	}

	// an App of Apps' folder too: its children are not created in someone else's folder
	os.MkdirAll(filepath.Join(work, "root"), 0o750)
	os.WriteFile(filepath.Join(work, "root", "notes.txt"), []byte("x"), 0o600)
	r.c.CreateApp(ctx, parentApp("root"))
	r.g.define("aaaaaaaa", "name: web\nrepoURL: x\nhost: local\n")
	r.mustSync(t, "root")
	r.childrenSynced(t, "web")
	if ops, _ := r.st.ListOps(ctx, "web"); ops[0].Phase != domain.OpFailed || !strings.Contains(ops[0].Message, "refusing to sync into "+filepath.Join(work, "root")+":") {
		t.Fatalf("child in a foreign App of Apps folder: %+v", ops[0])
	}

	// an empty folder (or one with only empty directories, as Docker leaves missing bind sources) is fine
	os.MkdirAll(filepath.Join(work, "fresh", "conf.d"), 0o750)
	r.c.CreateApp(ctx, app("fresh"))
	r.mustSync(t, "fresh")
	if got := readFile(t, filepath.Join(work, "fresh", domain.MarkerFile)); got != "fresh" {
		t.Fatalf("marker = %q", got)
	}
	r.mustSync(t, "fresh") // and its own folder stays its own
}

func TestClaimDir(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, files map[string]string) string {
		dir := filepath.Join(root, name)
		os.MkdirAll(dir, 0o750)
		for f, content := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o750)
			os.WriteFile(filepath.Join(dir, f), []byte(content), 0o644)
		}
		return dir
	}
	for _, tt := range []struct {
		name string
		dir  string
		ok   bool
	}{
		{"missing", filepath.Join(root, "missing", "web"), true},
		{"empty", mk("empty", nil), true},
		{"a file deep down", mk("dirs", map[string]string{"a/b/.keep": ""}), false}, // even an empty one
		{"marked for web", mk("mine", map[string]string{domain.MarkerFile: "web", "compose.yaml": "x"}), true},
		{"marked for another app", mk("other", map[string]string{domain.MarkerFile: "api"}), false},
		{"unmarked files", mk("foreign", map[string]string{"data": "x"}), false},
		{"unmarked old layout", mk("legacy", map[string]string{"compose.yaml": "x"}), true},
	} {
		err := claimDir(tt.dir, "web", filepath.Join(root, "legacy"))
		if (err == nil) != tt.ok {
			t.Errorf("%s: %v", tt.name, err)
		}
		if b, _ := os.ReadFile(filepath.Join(tt.dir, domain.MarkerFile)); tt.ok && string(b) != "web" {
			t.Errorf("%s: marker %q", tt.name, b)
		}
	}
	os.RemoveAll(filepath.Join(root, "dirs", "a", "b", ".keep"))
	if err := claimDir(filepath.Join(root, "dirs"), "web", ""); err != nil {
		t.Errorf("only empty directories: %v", err)
	}
}

// After the upgrade from <work dir>/apps/<name>: a start touches no containers; the app's next Sync moves it and
// deletes its old folder, folders of other apps stay.
func TestOldLayoutMovesOnSync(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	work := r.c.cfg.WorkDir
	old := filepath.Join(work, domain.LegacyDir)
	for path, content := range map[string]string{
		"web/compose.yaml": "services: {}\n", ".web.decrypted": "db.env\n",
		"other/compose.yaml": "services: {}\n", // not an Application (any more): not OmegaCD's to delete
		"root/apps.yaml":     "name: x\n",      // an App of Apps' definitions
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(old, path)), 0o750)
		os.WriteFile(filepath.Join(old, path), []byte(content), 0o644)
	}
	r.st.apps["web"], r.st.apps["root"] = app("web"), parentApp("root")
	r.st.ops = append(r.st.ops, domain.SyncOperation{App: "web", Phase: domain.OpSucceeded, Revision: "aaaaaaaa", TreeHash: "tree-aaaaaaaa"})

	r.c.restoreWorkDirs(ctx)
	r.waitIdle(t, "web")
	if r.e.syncCount() != 0 {
		t.Fatal("a start synced an app still in the old layout")
	}
	if _, err := os.Stat(filepath.Join(old, "web", "compose.yaml")); err != nil {
		t.Fatalf("old folder gone before a Sync: %v", err)
	}
	// until then its status, Diff and files are read from there, its decrypted files still masked
	if got := r.c.liveDir(app("web")); got != filepath.Join(old, "web") {
		t.Fatalf("liveDir before the move: %s", got)
	}
	if !r.c.readDecrypted("web")["db.env"] {
		t.Fatal("old layout's decrypted list not read")
	}
	// the new paths make the services differ; that's the move, not drift
	r.e.mu.Lock()
	r.e.drift = []domain.ServiceDiff{{Service: "web", Change: "changed", Fields: []string{"config"}}}
	r.e.mu.Unlock()
	if v, _ := r.c.Refresh(ctx, "web"); v.SyncStatus != domain.OutOfSync || !strings.Contains(v.SyncMessage, "on the next Sync") {
		t.Fatalf("before the move: %s %q", v.SyncStatus, v.SyncMessage)
	}
	r.e.mu.Lock()
	r.e.drift = nil
	r.e.mu.Unlock()

	r.mustSync(t, "web")
	if got := r.c.liveDir(app("web")); got != filepath.Join(work, "web") {
		t.Fatalf("liveDir after the move: %s", got)
	}
	if got := readFile(t, filepath.Join(work, "web", "compose.yaml")); got != "services: {}\n" {
		t.Fatalf("new folder: %q", got)
	}
	for path, want := range map[string]bool{"web": false, ".web.decrypted": false, "root": false, "other/compose.yaml": true} {
		if _, err := os.Stat(filepath.Join(old, path)); (err == nil) != want {
			t.Errorf("apps/%s: exists=%v, want %v", path, err == nil, want)
		}
	}

	// once the last old folder is gone, so is apps/
	os.RemoveAll(filepath.Join(old, "other"))
	os.MkdirAll(filepath.Join(old, "web"), 0o750)
	r.mustSync(t, "web")
	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty apps/ left: %v", err)
	}
}
