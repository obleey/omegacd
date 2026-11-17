package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/logging"
)

// scriptedEngine deploys nothing: it answers what the test set, nothing wrong by default.
type scriptedEngine struct {
	domain.Engine
	status func() (domain.LiveState, error)
	drift  []domain.ServiceDiff
	logs   func(id string) error
}

func (scriptedEngine) Validate(context.Context, domain.Application, string) error { return nil }
func (scriptedEngine) Sync(context.Context, domain.Application, string, domain.SyncOptions) error {
	return nil
}
func (scriptedEngine) Remove(context.Context, domain.Application, bool) error { return nil }

func (e scriptedEngine) Status(context.Context, domain.Application) (domain.LiveState, error) {
	if e.status != nil {
		return e.status()
	}
	return domain.LiveState{Health: domain.Healthy, Containers: []domain.Container{{ID: "c1"}}}, nil
}

func (e scriptedEngine) Diff(context.Context, domain.Application, string, string) ([]domain.ServiceDiff, error) {
	return e.drift, nil
}

func (scriptedEngine) ProjectFiles(context.Context, domain.Application, string) ([]domain.Resource, error) {
	return nil, nil
}

func (scriptedEngine) Inspect(context.Context, domain.Application, string, string) (map[string]string, error) {
	return nil, errors.New("volume gone")
}

func (e scriptedEngine) Logs(_ context.Context, _ domain.Application, id string, _ bool, _ int, _ io.Writer) error {
	if e.logs != nil {
		return e.logs(id)
	}
	return nil
}

// failSource is a secret source that can't decrypt, or says it wrote a file that isn't there.
type failSource struct{ wrote string }

func (s failSource) Decrypt(string) ([]Decrypted, error) {
	if s.wrote != "" {
		return []Decrypted{{Path: s.wrote}}, nil
	}
	return nil, errors.New("no key for it")
}

// testRig is an Agent with a real database, cache and work dir, and an engine that only answers.
func testRig(t *testing.T, eng domain.Engine) (*Agent, *source, *logging.Buffer) {
	t.Helper()
	cache, db, _ := openCache(t, 10)
	logs := &logging.Buffer{}
	work := NewWorkDir(filepath.Join(t.TempDir(), "work"), db, cache, nil)
	ag := New(db, cache, work, NewSecrets(-1), eng, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return ag, &source{}, logs
}

var oneCompose = map[string]string{"compose.yaml": "services: {}\n"}

// What stops a deploy before anything runs: its files, its folder, its secrets, a compose file it names.
func TestDeployFailures(t *testing.T) {
	ag, src, _ := testRig(t, scriptedEngine{})
	ctx := t.Context()
	r := src.revision("web", "r1", oneCompose)
	if err := ag.Deploy(ctx, App{Name: "web"}, r, func(string) ([]byte, error) { return nil, errors.New("offline") }, nil); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("a file it can't fetch: %v", err)
	}
	if err := ag.Deploy(ctx, App{Name: "web", WorkDir: "relative"}, r, src.fetch, nil); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a folder it may not write: %v", err)
	}
	r.ComposeFiles = []string{"other.yaml"}
	if err := ag.Deploy(ctx, App{Name: "web"}, r, src.fetch, nil); err == nil || !strings.Contains(err.Error(), `compose file "other.yaml" not found`) {
		t.Errorf("a compose file not in the Revision: %v", err)
	}
	ag.secrets = NewSecrets(-1, failSource{})
	if err := ag.Deploy(ctx, App{Name: "web"}, src.revision("web", "r1", oneCompose), src.fetch, nil); err == nil || !strings.Contains(err.Error(), "no key for it") {
		t.Errorf("secrets it can't decrypt: %v", err)
	}
}

// Without its lock (its state dir gone), nothing that changes what runs starts.
func TestLockFailures(t *testing.T) {
	ag, src, _ := testRig(t, scriptedEngine{})
	ctx := t.Context()
	ag.db.SaveApp(ctx, App{Name: "web", Revision: "r1"})
	ag.db.path = filepath.Join(t.TempDir(), "gone", "agent.db")
	r := src.revision("web", "r1", oneCompose)
	for name, err := range map[string]error{
		"deploy":    ag.Deploy(ctx, App{Name: "web"}, r, src.fetch, nil),
		"remove":    ag.Remove(ctx, "web", false),
		"rollback":  ag.Rollback(ctx, "web", "r1"),
		"restore":   ag.Restore(ctx),
		"self-heal": ag.selfHeal(ctx, "web"),
	} {
		if err == nil || !strings.Contains(err.Error(), "agent lock") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Asking about an Application not deployed here says so; an engine failing says so; a container replaced meanwhile
// is left out of the logs (Q146).
func TestReadFailures(t *testing.T) {
	ctx := t.Context()
	ag, src, logs := testRig(t, scriptedEngine{logs: func(id string) error {
		if id == "c1" {
			return domain.ErrNotFound
		}
		return errors.New("engine down")
	}, status: func() (domain.LiveState, error) {
		return domain.LiveState{Containers: []domain.Container{{ID: "c1"}, {ID: "c2"}}}, nil
	}})
	if _, err := ag.Status(ctx, "web"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("status: %v", err)
	}
	if _, err := ag.Drift(ctx, "web"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("drift: %v", err)
	}
	if err := ag.Logs(ctx, "web", "", false, 1, io.Discard); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("logs: %v", err)
	}
	if err := ag.Deploy(ctx, App{Name: "web"}, src.revision("web", "r1", oneCompose), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	if err := ag.Logs(ctx, "web", "", false, 1, io.Discard); err == nil || err.Error() != "engine down" {
		t.Errorf("logs, the engine failing: %v", err)
	}
	if !strings.Contains(logs.String(), "logs: container gone, being replaced") {
		t.Errorf("logs:\n%s", logs.String())
	}
	ag.engine = scriptedEngine{status: func() (domain.LiveState, error) { return domain.LiveState{}, errors.New("engine down") }}
	if err := ag.Logs(ctx, "web", "", false, 1, io.Discard); err == nil {
		t.Error("logs without the status")
	}
	if err := ag.selfHeal(ctx, "web"); err != nil { // Self-Heal off: not its to heal
		t.Error(err)
	}
	ag.db.SaveApp(ctx, App{Name: "web", Revision: "r1", SelfHeal: true, Dir: ag.work.Dir(App{Name: "web"})})
	if err := ag.selfHeal(ctx, "web"); err == nil || err.Error() != "engine down" {
		t.Errorf("self-heal without the status: %v", err)
	}
	ag.engine = scriptedEngine{drift: []domain.ServiceDiff{{Service: "web", Change: "changed"}}}
	if err := ag.selfHeal(ctx, "web"); err != nil || !strings.Contains(logs.String(), "why=\"drifted: 1 services\"") {
		t.Errorf("self-heal of a drift: %v\n%s", err, logs.String())
	}
}

// After the engine's events come back, every Application is checked; Run logs a restore that failed (Q132).
func TestWatchAndRunCheckEveryApplication(t *testing.T) {
	ag, _, logs := testRig(t, watchEngine{})
	ctx := t.Context()
	ag.db.SaveApp(ctx, App{Name: "web"})
	var changed []string
	ag.Watch(ctx, func(app string) { changed = append(changed, app) })
	if strings.Join(changed, ",") != "web" {
		t.Fatalf("changed %v", changed)
	}
	ag.db.Close()
	ag.Run(ctx)
	if !strings.Contains(logs.String(), `msg="restore failed; fix the cause, then restart the Agent to try again"`) {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

// A folder it can't delete fails the removal, which keeps the Application.
func TestRemoveFolderFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root deletes anything") // the suite runs as a normal user
	}
	ag, _, _ := testRig(t, scriptedEngine{})
	ctx := t.Context()
	parent := t.TempDir()
	dir := filepath.Join(parent, "web")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "f"), nil, 0o644)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	ag.db.SaveApp(ctx, App{Name: "web", Dir: dir})
	if err := ag.Remove(ctx, "web", false); err == nil {
		t.Fatal("removed a folder it can't delete")
	}
	if _, err := ag.db.GetApp(ctx, "web"); err != nil {
		t.Fatalf("forgotten anyway: %v", err)
	}
}

// corrupt writes a value its column's type can't read back.
func corrupt(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if _, err := db.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// The cache's database and folder failing: each fails what asked it.
func TestCacheFailures(t *testing.T) {
	ctx := t.Context()
	if _, err := OpenCache(nil, filepath.Join(writeFile(t, "a file"), "cache"), 1); err == nil {
		t.Error("a cache under a file")
	}
	c, db, _ := openCache(t, 1)
	src := &source{}
	corrupt(t, db, `INSERT INTO revisions (app, revision, checksum, files, compose_files, profiles, added_at) VALUES ('web', 'r0', '', 'x', '[]', '[]', 0)`)
	if err := c.Add(ctx, src.revision("web", "r1", oneCompose), src.fetch); err == nil {
		t.Error("trimmed revisions it can't read")
	}
	corrupt(t, db, `DELETE FROM revisions`)
	corrupt(t, db, `INSERT INTO apps (name, parent, revision, self_heal, prune, prune_volumes, updated_at) VALUES ('web', '', '', 'x', 0, 0, 0)`)
	if err := c.Add(ctx, src.revision("web", "r1", oneCompose), src.fetch); err == nil {
		t.Error("trimmed without the deployed Revision")
	}
	db.Close()
	if _, err := c.Revision(ctx, "web", "r1"); err == nil {
		t.Error("a revision from a closed database")
	}
	if _, err := c.Collect(ctx, 0); err == nil {
		t.Error("collected with a closed database")
	}
	if err := c.Add(ctx, src.revision("web", "r2", map[string]string{"x": "new"}), src.fetch); err == nil {
		t.Error("cached into a closed database")
	}
	if os.Getuid() == 0 {
		return // root writes anywhere; the suite runs as a normal user
	}
	c, _, state := openCache(t, 1)
	os.Chmod(filepath.Join(state, "cache"), 0o500)
	t.Cleanup(func() { os.Chmod(filepath.Join(state, "cache"), 0o700) })
	if err := c.Add(ctx, src.revision("web", "r3", map[string]string{"y": "newer"}), src.fetch); err == nil {
		t.Error("cached into a folder it can't write")
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The Agent's database refusing what it can't read: a file that isn't one, a schema an old migration can't apply,
// rows written wrong.
func TestDBFailures(t *testing.T) {
	ctx := t.Context()
	if _, err := OpenDB(writeFile(t, strings.Repeat("not a database ", 100))); err == nil {
		t.Error("opened a file that isn't a database")
	}
	path := filepath.Join(t.TempDir(), "agent.db")
	d := openDB(t, path)
	corrupt(t, d, `PRAGMA user_version = 0`)
	d.Close()
	if _, err := OpenDB(path); err == nil || !strings.Contains(err.Error(), "migration 1") {
		t.Errorf("migrating a database that has its tables: %v", err)
	}

	d = openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	defer d.Close()
	corrupt(t, d, `INSERT INTO apps (name, parent, revision, self_heal, prune, prune_volumes, updated_at) VALUES ('web', '', '', 'x', 0, 0, 0)`)
	if _, err := d.ListApps(ctx); err == nil {
		t.Error("listed an app it can't read")
	}
	corrupt(t, d, `INSERT INTO revisions (app, revision, checksum, files, compose_files, profiles, added_at) VALUES ('a', 'r1', '', '[]', '[]', '[]', 'x')`)
	if _, err := d.Revisions(ctx, "a"); err == nil {
		t.Error("a revision whose time isn't a number")
	}
	corrupt(t, d, `UPDATE revisions SET added_at = 0, files = 'x'`)
	if _, err := d.Revisions(ctx, "a"); err == nil || !strings.Contains(err.Error(), "revision r1 of a") {
		t.Errorf("a revision whose files aren't JSON: %v", err)
	}
	if _, err := d.files(ctx); err == nil {
		t.Error("the files of revisions it can't read")
	}
	corrupt(t, d, `INSERT INTO outbox (recorded_at, entry) VALUES (0, x'ff')`)
	if _, err := d.Outbox(ctx); err == nil || !strings.Contains(err.Error(), "outbox entry 1") {
		t.Errorf("an outbox entry that isn't one: %v", err)
	}
	d.Close()
	if _, err := d.Revisions(ctx, "a"); err == nil {
		t.Error("revisions from a closed database")
	}
	if err := d.DeleteApp(ctx, "web"); err == nil {
		t.Error("deleted from a closed database")
	}
	if _, err := d.files(ctx); err == nil {
		t.Error("files from a closed database")
	}
}

// OMEGACD_AGENT_SECRET_GROUP takes a group's name as its gid, and refuses a negative one.
func TestConfigSecretGroupName(t *testing.T) {
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skip("this user's group has no name")
	}
	base := map[string]string{"HOME": "/home/x"}
	base["OMEGACD_AGENT_SECRET_GROUP"] = g.Name
	if c, err := LoadConfig(env(base), 1000); err != nil || c.SecretGroup != os.Getgid() {
		t.Errorf("by name: %d, %v", c.SecretGroup, err)
	}
	base["OMEGACD_AGENT_SECRET_GROUP"] = "-5"
	if _, err := LoadConfig(env(base), 1000); err == nil || !strings.Contains(err.Error(), "want a group name or gid") {
		t.Errorf("a negative gid: %v", err)
	}
}

// The signing key's file in a state it can't be read from: a rotation's leftover it can't remove, a link to itself, a
// folder; and a CA file that isn't there.
func TestSigningKeyFailures(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "signing.key.next", "x"), 0o700)
	if _, err := SigningKey(dir); err == nil {
		t.Error("a leftover rotation it can't remove")
	}
	if _, err := rotate(filepath.Join(dir, "signing.key.next")); err == nil {
		t.Error("rotated over a folder it can't remove")
	}
	dir = t.TempDir()
	os.Symlink("signing.key", filepath.Join(dir, "signing.key"))
	if _, err := SigningKey(dir); err == nil {
		t.Error("a key file that links to itself")
	}
	dir = t.TempDir()
	os.Mkdir(filepath.Join(dir, "signing.key"), 0o700)
	if _, err := SigningKey(dir); err == nil {
		t.Error("a key file that is a folder")
	}
	if _, err := TrustCA(filepath.Join(dir, "none.pem")); err == nil || !strings.Contains(err.Error(), "OMEGACD_AGENT_CA_FILE") {
		t.Errorf("a CA file that isn't there: %v", err)
	}
	if _, err := HostKey(filepath.Join(dir, "gone")); err == nil {
		t.Error("a Host Key into a state dir that isn't there")
	}

}

// Files a source says it decrypted get their mode; one that isn't there fails, and so does a group the Agent's user
// isn't in.
func TestSecretModeFailures(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewSecrets(-1, failSource{wrote: "gone.env"}).Decrypt(dir); err == nil {
		t.Error("the mode of a file that isn't there")
	}
	if os.Getuid() == 0 {
		return // root joins any group; the suite runs as a normal user
	}
	os.WriteFile(filepath.Join(dir, "x.env"), nil, 0o600)
	if _, err := NewSecrets(0, failSource{wrote: "x.env"}).Decrypt(dir); err == nil || !strings.Contains(err.Error(), "OMEGACD_AGENT_SECRET_GROUP") {
		t.Errorf("a group it isn't in: %v", err)
	}
}

// Restoring: what isn't deployed is skipped; a database it can't read, a Revision not cached, a folder it may not
// write fail the restore, each saying which Application.
func TestRestoreFailures(t *testing.T) {
	ag, src, _ := testRig(t, scriptedEngine{status: func() (domain.LiveState, error) { return domain.LiveState{}, nil }})
	ctx := t.Context()
	ag.db.SaveApp(ctx, App{Name: "never"})
	ag.db.SaveApp(ctx, App{Name: "uncached", Revision: "r9"})
	r := src.revision("nowhere", "r1", oneCompose)
	ag.cache.Add(ctx, r, src.fetch)
	ag.db.SaveApp(ctx, App{Name: "nowhere", Revision: "r1", WorkDir: "relative"})
	for name, err := range map[string]error{"files": ag.RestoreFiles(ctx), "restore": ag.Restore(ctx)} {
		if err == nil || !strings.Contains(err.Error(), "restore uncached") || !strings.Contains(err.Error(), "restore nowhere") ||
			strings.Contains(err.Error(), "never") {
			t.Errorf("%s: %v", name, err)
		}
	}
	ag.db.Close()
	if ag.RestoreFiles(ctx) == nil || ag.Restore(ctx) == nil {
		t.Error("restored from a closed database")
	}
}

// reclaim leaves alone a folder that isn't there and refuses one outside the work dir, before asking the engine.
func TestReclaimRefuses(t *testing.T) {
	root := t.TempDir()
	if err := reclaim(t.Context(), root, filepath.Join(root, "gone"), ""); err != nil {
		t.Errorf("a folder that isn't there: %v", err)
	}
	if err := reclaim(t.Context(), root, t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "is not in the work dir") {
		t.Errorf("a folder elsewhere: %v", err)
	}
	t.Setenv("DOCKER_HOST", "bogus")
	if err := reclaim(t.Context(), root, root, ""); err == nil {
		t.Error("an engine address that isn't one")
	}
	if _, err := DetectEngine(t.Context()); err == nil {
		t.Error("detected an engine at an address that isn't one")
	}
}

// Rolling back reads the cached Revisions: a database that can't, fails it.
func TestRollbackUnreadable(t *testing.T) {
	ag, _, _ := testRig(t, scriptedEngine{})
	ctx := t.Context()
	ag.db.SaveApp(ctx, App{Name: "web"})
	corrupt(t, ag.db, `INSERT INTO revisions (app, revision, checksum, files, compose_files, profiles, added_at) VALUES ('web', 'r1', '', 'x', '[]', '[]', 0)`)
	if err := ag.Rollback(ctx, "web", "r1"); err == nil {
		t.Fatal("rolled back to a revision it can't read")
	}
}

// Writing a folder: a database it can't read, an App of Apps' folder that holds another's files.
func TestWorkDirFailures(t *testing.T) {
	w, src, _ := newWorkDir(t)
	ctx := t.Context()
	os.MkdirAll(filepath.Join(w.Root, "platform"), 0o755)
	os.WriteFile(filepath.Join(w.Root, "platform", "notes.txt"), []byte("someone's"), 0o644)
	if err := w.deploy(t, src, App{Name: "web", Parent: "platform"}, "r1", oneCompose); err == nil {
		t.Error("claimed an App of Apps' folder holding another's files")
	}
	corrupt(t, w.db, `INSERT INTO apps (name, parent, revision, self_heal, prune, prune_volumes, updated_at) VALUES ('other', 'platform', '', 'x', 0, 0, 0)`)
	if err := w.deploy(t, src, App{Name: "api", Parent: "platform"}, "r1", oneCompose); err == nil {
		t.Error("listed the App of Apps' children from rows it can't read")
	}
	corrupt(t, w.db, `UPDATE apps SET name = 'api'`)
	if err := w.Write(ctx, App{Name: "api"}, src.revision("api", "r1", oneCompose)); err == nil {
		t.Error("wrote over a folder whose owner it can't read")
	}
}

// The checksums of what is deployed leave out an Application never deployed.
func TestChecksumsSkipNeverDeployed(t *testing.T) {
	ag, src, _ := testRig(t, scriptedEngine{})
	ctx := t.Context()
	ag.db.SaveApp(ctx, App{Name: "never"})
	r := src.revision("web", "r1", oneCompose)
	r.Checksum = "abcd"
	if err := ag.Deploy(ctx, App{Name: "web"}, r, src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	if r := ag.checksums(ctx); len(r.Checksums) != 1 {
		t.Fatalf("%v", r.Checksums)
	}
}

// A status report whose volume's details can't be read goes without them, saying so at debug.
func TestStatusReportWithoutDetails(t *testing.T) {
	ag, src, logs := testRig(t, scriptedEngine{status: func() (domain.LiveState, error) {
		return domain.LiveState{Resources: []domain.Resource{{Kind: "volume", Name: "data"}}}, nil
	}})
	ctx := t.Context()
	if err := ag.Deploy(ctx, App{Name: "web"}, src.revision("web", "r1", oneCompose), src.fetch, nil); err != nil {
		t.Fatal(err)
	}
	rep, err := ag.statusReport(ctx, "web")
	if err != nil || string(rep.Details) != "{}" || !strings.Contains(logs.String(), `msg="status report: no details"`) {
		t.Fatalf("%v, %v\n%s", rep, err, logs.String())
	}
}
