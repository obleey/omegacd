// Package store is the SQLite implementation of domain.Store.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/domain"
)

// schemaBase is PRAGMA user_version of v0.2's first schema (Q105): v0.1, and v0.2 before M4-05, counted their
// migrations from 1, so any version below it is a database this version can't read.
const schemaBase = 1000

// migrations are applied in order from v0.2's fresh schema; PRAGMA user_version records schemaBase plus how many ran.
var migrations = []string{`
CREATE TABLE apps (
	name TEXT PRIMARY KEY,
	repo_url TEXT NOT NULL,
	path TEXT NOT NULL,
	target_revision TEXT NOT NULL,
	compose_files TEXT NOT NULL,
	profiles TEXT NOT NULL,
	host TEXT NOT NULL,
	auto_sync INTEGER NOT NULL,
	self_heal INTEGER NOT NULL,
	prune INTEGER NOT NULL,
	prune_volumes INTEGER NOT NULL,
	adopt INTEGER NOT NULL,
	rolled_back INTEGER NOT NULL,
	kind TEXT NOT NULL,
	parent TEXT NOT NULL,
	not_in_git INTEGER NOT NULL,
	work_dir TEXT NOT NULL,
	deleting TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE ops (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app TEXT NOT NULL REFERENCES apps(name) ON DELETE CASCADE,
	revision TEXT NOT NULL,
	tree_hash TEXT NOT NULL,
	phase TEXT NOT NULL,
	message TEXT NOT NULL,
	trigger TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL
);
CREATE INDEX ops_app ON ops(app, id);
CREATE TABLE registry_credentials (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	registry TEXT NOT NULL UNIQUE,
	username TEXT NOT NULL,
	password TEXT NOT NULL
);
CREATE TABLE tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	last_used INTEGER NOT NULL
);
CREATE TABLE admin (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	password_hash TEXT NOT NULL,
	must_change INTEGER NOT NULL
);
CREATE TABLE sessions (
	hash TEXT PRIMARY KEY,
	expires_at INTEGER NOT NULL
);
CREATE TABLE hosts (
	name TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	signing_key BLOB UNIQUE,
	previous_signing_key BLOB,
	age_recipient TEXT NOT NULL DEFAULT '',
	approved INTEGER NOT NULL DEFAULT 0,
	locked TEXT NOT NULL DEFAULT '',
	agent TEXT NOT NULL DEFAULT '',
	allow_ips TEXT NOT NULL DEFAULT '[]',
	outbox_seq INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE invites (
	host TEXT PRIMARY KEY REFERENCES hosts(name) ON DELETE CASCADE,
	hash TEXT NOT NULL UNIQUE,
	expires_at INTEGER NOT NULL
);
CREATE TABLE agent_status (
	app TEXT PRIMARY KEY REFERENCES apps(name) ON DELETE CASCADE,
	status TEXT NOT NULL
);
CREATE TABLE agent_diff (
	app TEXT PRIMARY KEY REFERENCES apps(name) ON DELETE CASCADE,
	diff TEXT NOT NULL
);
`, `
-- M4-06, as of the Host's Agent's last resync: each Application it runs with its deployed tree's checksum (Q119), and
-- the ones the Control Plane doesn't know (Q118), JSON
ALTER TABLE hosts ADD COLUMN agent_apps TEXT NOT NULL DEFAULT '{}';
ALTER TABLE hosts ADD COLUMN unmanaged TEXT NOT NULL DEFAULT '[]';
`, `
-- M4-07: each render's file list (JSON) by its tree checksum, what Sync Operations' tree_hash name (Q120)
CREATE TABLE renders (
	tree TEXT PRIMARY KEY,
	files TEXT NOT NULL
);
`}

type Store struct {
	db     *sql.DB
	sealer *crypto.Sealer
}

var _ domain.Store = (*Store)(nil)

func Open(path string, sealer *crypto.Sealer) (*Store, error) {
	if err := checkVersion(path); err != nil {
		return nil, err
	}
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil { // coverage: unreachable, sql.Open only checks the driver name, which is registered
		return nil, err
	}
	s := &Store{db: db, sealer: sealer}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ErrOldDatabase: the database is v0.1's, or v0.2's from before its fresh schema (Q90, Q105).
var ErrOldDatabase = errors.New("this database is from OmegaCD v0.1 (or a v0.2 development build before v0.2's schema), " +
	"which this version can't read and won't change: export the Application definitions with v0.1's omegacd app export, " +
	"then start this version with a new, empty data directory and create them again (there is no migration)")

// checkVersion refuses an older database before anything opens it for writing: opening it read-write could change it
// (its journal mode), and it must be left as it was (Q90). A file that isn't there yet is a new database.
func checkVersion(path string) error {
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
	if err != nil { // coverage: unreachable, sql.Open only checks the driver name, which is registered
		return err
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return nil // no file: a new database; a broken one fails at Open with its own error
	}
	if v > 0 && v < schemaBase {
		return fmt.Errorf("%s (user_version %d): %w", path, v, ErrOldDatabase)
	}
	return nil
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v == 0 {
		v = schemaBase
	}
	if v > schemaBase+len(migrations) { // Q134: an older binary must not run on a schema it doesn't know
		return fmt.Errorf("the database is version %d, newer than this OmegaCD knows (%d): upgrade OmegaCD", v, schemaBase+len(migrations))
	}
	for i := v - schemaBase; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil { // coverage: unreachable, beginning a transaction on a healthy SQLite that just answered user_version
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaBase+i+1)); err != nil { // coverage: unreachable, setting user_version in a healthy transaction that just ran its migration can't fail
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil { // coverage: unreachable, committing a healthy SQLite transaction
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func mapErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %v", domain.ErrAlreadyExists, err)
		}
	}
	return err
}

// affected turns "0 rows changed" into ErrNotFound.
func affected(res sql.Result, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if n, err := res.RowsAffected(); err != nil { // coverage: unreachable, the sqlite driver always reports the rows a statement changed
		return err
	} else if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

type scanner interface{ Scan(...any) error }

// ---- apps ----

const appCols = `name, repo_url, path, target_revision, compose_files, profiles, host, auto_sync, self_heal, prune, adopt, rolled_back, kind, parent, not_in_git, prune_volumes, work_dir, created_at`

// appRead is appCols plus what only SetAppDeleting writes.
const appRead = appCols + `, deleting`

func scanApp(r scanner) (domain.Application, error) {
	var a domain.Application
	var files, profiles string
	var created int64
	if err := r.Scan(&a.Name, &a.RepoURL, &a.Path, &a.TargetRevision, &files, &profiles, &a.Host,
		&a.AutoSync, &a.SelfHeal, &a.Prune, &a.Adopt, &a.RolledBack, &a.Kind, &a.Parent, &a.NotInGit, &a.PruneVolumes, &a.WorkDir, &created, &a.Deleting); err != nil {
		return a, mapErr(err)
	}
	a.CreatedAt = fromUnix(created)
	if err := json.Unmarshal([]byte(files), &a.ComposeFiles); err != nil {
		return a, err
	}
	return a, json.Unmarshal([]byte(profiles), &a.Profiles)
}

func appArgs(a domain.Application) []any {
	files, _ := json.Marshal(a.ComposeFiles)
	profiles, _ := json.Marshal(a.Profiles)
	return []any{a.Name, a.RepoURL, a.Path, a.TargetRevision, string(files), string(profiles), a.Host,
		a.AutoSync, a.SelfHeal, a.Prune, a.Adopt, a.RolledBack, a.Kind, a.Parent, a.NotInGit, a.PruneVolumes, a.WorkDir}
}

func (s *Store) ListApps(ctx context.Context) ([]domain.Application, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appRead+` FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	apps := []domain.Application{}
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

func (s *Store) GetApp(ctx context.Context, name string) (domain.Application, error) {
	return scanApp(s.db.QueryRowContext(ctx, `SELECT `+appRead+` FROM apps WHERE name = ?`, name))
}

func (s *Store) CreateApp(ctx context.Context, a domain.Application) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO apps (`+appCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		append(appArgs(a), toUnix(a.CreatedAt))...)
	return mapErr(err)
}

// UpdateApp keeps CreatedAt; the app is identified by Name.
func (s *Store) UpdateApp(ctx context.Context, a domain.Application) error {
	args := appArgs(a)
	return affected(s.db.ExecContext(ctx, `UPDATE apps SET repo_url=?, path=?, target_revision=?, compose_files=?,
		profiles=?, host=?, auto_sync=?, self_heal=?, prune=?, adopt=?, rolled_back=?, kind=?, parent=?, not_in_git=?, prune_volumes=?,
		work_dir=? WHERE name=?`, append(args[1:], a.Name)...))
}

func (s *Store) DeleteApp(ctx context.Context, name string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM apps WHERE name = ?`, name))
}

// ---- ops ----

const opCols = `id, app, revision, tree_hash, phase, message, trigger, started_at, finished_at`

func scanOp(r scanner) (domain.SyncOperation, error) {
	var o domain.SyncOperation
	var started, finished int64
	if err := r.Scan(&o.ID, &o.App, &o.Revision, &o.TreeHash, &o.Phase, &o.Message, &o.Trigger, &started, &finished); err != nil {
		return o, mapErr(err)
	}
	o.StartedAt, o.FinishedAt = fromUnix(started), fromUnix(finished)
	return o, nil
}

func (s *Store) CreateOp(ctx context.Context, op domain.SyncOperation) (int64, error) {
	if op.StartedAt.IsZero() {
		op.StartedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO ops (app, revision, tree_hash, phase, message, trigger, started_at, finished_at)
		VALUES (?,?,?,?,?,?,?,0)`, op.App, op.Revision, op.TreeHash, domain.OpRunning, op.Message, op.Trigger, toUnix(op.StartedAt))
	if err != nil {
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
			return 0, domain.ErrNotFound
		}
		return 0, mapErr(err)
	}
	return res.LastInsertId()
}

// Backup writes a consistent copy of the database to path, a file that must not exist, while the server runs (Q114).
func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// FinishOp records how a Sync Operation ended. History is kept until retention prunes it (PruneOps, Q112).
func (s *Store) FinishOp(ctx context.Context, id int64, phase domain.OpPhase, message string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE ops SET phase=?, message=?, finished_at=? WHERE id=?`,
		phase, message, time.Now().UnixNano(), id))
}

func (s *Store) SetOpPhase(ctx context.Context, id int64, phase domain.OpPhase, message string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE ops SET phase=?, message=? WHERE id=? AND finished_at=0`, phase, message, id))
}

func (s *Store) SupersedeWaitingOps(ctx context.Context, app string, by int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ops SET phase=?, message=?, finished_at=? WHERE app=? AND phase=? AND id<>?`,
		domain.OpSuperseded, fmt.Sprintf("superseded by Sync Operation %d before the Host came back", by),
		time.Now().UnixNano(), app, domain.OpWaiting, by)
	return err
}

func (s *Store) GetOp(ctx context.Context, id int64) (domain.SyncOperation, error) {
	return scanOp(s.db.QueryRowContext(ctx, `SELECT `+opCols+` FROM ops WHERE id = ?`, id))
}

// PruneOps deletes finished Sync Operations beyond the newest keep of each Application (0: no limit) and those finished
// before olderThan (zero: no limit), except each Application's newest succeeded one, the deployed Revision's (Q112). It
// returns how many went.
func (s *Store) PruneOps(ctx context.Context, keep int, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ops WHERE id IN (
		SELECT id FROM (SELECT id, finished_at, ROW_NUMBER() OVER (PARTITION BY app ORDER BY id DESC) AS n FROM ops)
		WHERE finished_at > 0 AND ((? > 0 AND n > ?) OR finished_at < ?))
		AND id NOT IN (SELECT MAX(id) FROM ops WHERE phase = ? GROUP BY app)`,
		keep, keep, toUnix(olderThan), domain.OpSucceeded)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SaveRender keeps a render's file list by its tree checksum (Q120); one already kept stays.
func (s *Store) SaveRender(ctx context.Context, tree string, files []domain.File) error {
	b, err := json.Marshal(files)
	if err != nil { // coverage: unreachable, a slice of plain structs always marshals
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO renders (tree, files) VALUES (?, ?)`, tree, b)
	return err
}

// Render is the file list kept for tree, or ErrNotFound.
func (s *Store) Render(ctx context.Context, tree string) ([]domain.File, error) {
	var b []byte
	if err := s.db.QueryRowContext(ctx, `SELECT files FROM renders WHERE tree = ?`, tree).Scan(&b); err != nil {
		return nil, mapErr(err)
	}
	var files []domain.File
	return files, json.Unmarshal(b, &files)
}

// PruneRenders deletes the renders no Sync Operation names (Q125) and returns how many went, with the blobs the
// renders left still use.
func (s *Store) PruneRenders(ctx context.Context) (int64, map[string]bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM renders WHERE tree NOT IN (SELECT tree_hash FROM ops)`)
	if err != nil {
		return 0, nil, err
	}
	n, _ := res.RowsAffected()
	rows, err := s.db.QueryContext(ctx, `SELECT files FROM renders`)
	if err != nil { // coverage: unreachable, a plain SELECT right after a DELETE on the same healthy database
		return 0, nil, err
	}
	defer rows.Close()
	used := map[string]bool{}
	for rows.Next() {
		var b []byte
		var files []domain.File
		if err := rows.Scan(&b); err != nil { // coverage: unreachable, files is a NOT NULL TEXT column
			return 0, nil, err
		}
		if err := json.Unmarshal(b, &files); err != nil {
			return 0, nil, err
		}
		for _, f := range files {
			used[f.SHA256] = true
		}
	}
	return n, used, rows.Err()
}

func (s *Store) ListOps(ctx context.Context, app string) ([]domain.SyncOperation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+opCols+` FROM ops WHERE app = ? ORDER BY id DESC`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ops := []domain.SyncOperation{}
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil { // coverage: unreachable, scanning ops' fixed columns into their own types
			return nil, err
		}
		ops = append(ops, o)
	}
	return ops, rows.Err()
}

func (s *Store) LastSucceededOp(ctx context.Context, app string) (domain.SyncOperation, error) {
	return scanOp(s.db.QueryRowContext(ctx, `SELECT `+opCols+` FROM ops WHERE app = ? AND phase = ? ORDER BY id DESC LIMIT 1`,
		app, domain.OpSucceeded))
}

// ---- credentials ----

func (s *Store) ListRegistryCredentials(ctx context.Context) ([]domain.RegistryCredential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, registry, username, password FROM registry_credentials ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	creds := []domain.RegistryCredential{}
	for rows.Next() {
		var c domain.RegistryCredential
		var pw string
		if err := rows.Scan(&c.ID, &c.Registry, &c.Username, &pw); err != nil { // coverage: unreachable, scanning fixed columns into their own types
			return nil, err
		}
		if c.Password, err = s.sealer.Open(pw); err != nil {
			return nil, fmt.Errorf("open registry credential %d: %w", c.ID, err)
		}
		c.HasPassword = c.Password != ""
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

func (s *Store) SaveRegistryCredential(ctx context.Context, c domain.RegistryCredential) (int64, error) {
	pw, err := s.sealer.Seal(c.Password)
	if err != nil { // coverage: unreachable, age encrypting in memory to its own recipient
		return 0, err
	}
	if c.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO registry_credentials (registry, username, password) VALUES (?,?,?)`,
			c.Registry, c.Username, pw)
		if err != nil {
			return 0, mapErr(err)
		}
		return res.LastInsertId()
	}
	return c.ID, affected(s.db.ExecContext(ctx, `UPDATE registry_credentials SET registry=?, username=?,
		password=COALESCE(NULLIF(?, ''), password) WHERE id=?`, c.Registry, c.Username, pw, c.ID))
}

func (s *Store) DeleteRegistryCredential(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM registry_credentials WHERE id = ?`, id))
}

// ---- hosts ----

// hostSelect reads a Host with its Agent's enrollment
const hostSelect = `SELECT name, created_at, signing_key, age_recipient, approved, locked, agent, allow_ips, agent_apps, unmanaged FROM hosts`

func (s *Store) scanHost(r scanner) (domain.Host, error) {
	var h domain.Host
	var created int64
	var locked, agent, allow, agentApps, unmanaged string
	if err := r.Scan(&h.Name, &created,
		&h.SigningKey, &h.AgeRecipient, &h.Approved, &locked, &agent, &allow, &agentApps, &unmanaged); err != nil {
		return h, mapErr(err)
	}
	if err := json.Unmarshal([]byte(agentApps), &h.AgentApps); err != nil {
		return h, fmt.Errorf("host %s: agent_apps: %w", h.Name, err)
	}
	if err := json.Unmarshal([]byte(unmanaged), &h.Unmanaged); err != nil {
		return h, fmt.Errorf("host %s: unmanaged: %w", h.Name, err)
	}
	if err := json.Unmarshal([]byte(allow), &h.AllowIPs); err != nil {
		return h, fmt.Errorf("host %s: allow_ips: %w", h.Name, err)
	}
	if agent != "" {
		h.Agent = &domain.AgentInfo{}
		if err := json.Unmarshal([]byte(agent), h.Agent); err != nil {
			return h, fmt.Errorf("host %s: agent: %w", h.Name, err)
		}
	}
	if locked != "" {
		if err := json.Unmarshal([]byte(locked), &h.Locked); err != nil {
			return h, fmt.Errorf("host %s: locked: %w", h.Name, err)
		}
	}
	h.CreatedAt = fromUnix(created)
	if len(h.SigningKey) > 0 {
		h.Fingerprint = domain.Fingerprint(h.SigningKey)
	}
	return h, nil
}

func (s *Store) ListHosts(ctx context.Context) ([]domain.Host, error) {
	rows, err := s.db.QueryContext(ctx, hostSelect+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hosts := []domain.Host{}
	for rows.Next() {
		h, err := s.scanHost(rows)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

func (s *Store) GetHost(ctx context.Context, name string) (domain.Host, error) {
	return s.scanHost(s.db.QueryRowContext(ctx, hostSelect+` WHERE name = ?`, name))
}

func (s *Store) HostBySigningKey(ctx context.Context, key []byte) (domain.Host, error) {
	if len(key) == 0 {
		return domain.Host{}, domain.ErrNotFound
	}
	return s.scanHost(s.db.QueryRowContext(ctx, hostSelect+` WHERE signing_key = ?1 OR previous_signing_key = ?1`, key))
}

func (s *Store) RotateSigningKey(ctx context.Context, host string, old, next []byte) error {
	if len(next) == 0 {
		return fmt.Errorf("%w: no new key", domain.ErrInvalid)
	}
	err := affected(s.db.ExecContext(ctx, `UPDATE hosts SET previous_signing_key = signing_key, signing_key = ?
		WHERE name = ? AND signing_key = ?`, next, host, old))
	if errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("%w: Host %s's key changed meanwhile", domain.ErrInvalid, host)
	}
	return err
}

func (s *Store) ConfirmSigningKey(ctx context.Context, host string, key []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET
		signing_key = CASE WHEN previous_signing_key = ?1 THEN previous_signing_key ELSE signing_key END,
		previous_signing_key = NULL
		WHERE name = ?2 AND ?1 IN (signing_key, previous_signing_key)`, key, host)
	return mapErr(err)
}

func (s *Store) LockHost(ctx context.Context, host string, conns []domain.AgentConnection) error {
	b, err := json.Marshal(conns)
	if err != nil { // coverage: unreachable, marshalling plain structs
		return err
	}
	return affected(s.db.ExecContext(ctx, `UPDATE hosts SET locked = ? WHERE name = ?`, string(b), host))
}

func (s *Store) SetAllowIPs(ctx context.Context, host string, cidrs []string) error {
	b, err := json.Marshal(cidrs)
	if err != nil { // coverage: unreachable, marshalling strings
		return err
	}
	if len(cidrs) == 0 {
		b = []byte("[]")
	}
	return affected(s.db.ExecContext(ctx, `UPDATE hosts SET allow_ips = ? WHERE name = ?`, string(b), host))
}

func (s *Store) ApplyAgentReport(ctx context.Context, host string, seq uint64, r domain.AgentReport) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var last uint64
	if err := tx.QueryRowContext(ctx, `SELECT outbox_seq FROM hosts WHERE name = ?`, host).Scan(&last); err != nil {
		return false, mapErr(err)
	}
	if seq <= last {
		return false, nil
	}
	if r.At.IsZero() {
		r.At = time.Now() // finished_at 0 would read as unfinished
	}
	at := toUnix(r.At)
	if r.Op != 0 {
		// a report on a Sync the Control Plane sent; one on another Host's Application, or already finished, changes nothing
		_, err = tx.ExecContext(ctx, `UPDATE ops SET phase=?, message=?, finished_at=?, tree_hash=COALESCE(NULLIF(?, ''), tree_hash)
			WHERE id=? AND app=? AND finished_at=0 AND app IN (SELECT name FROM apps WHERE host=?)`,
			r.Phase, r.Message, at, r.Checksum, r.Op, r.App, host)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO ops (app, revision, tree_hash, phase, message, trigger, started_at, finished_at)
			SELECT name, ?, ?, ?, ?, ?, ?, ? FROM apps WHERE name=? AND host=?`,
			r.Revision, r.Checksum, r.Phase, r.Message, r.Trigger, at, at, r.App, host)
		if err == nil && r.Trigger == "rollback" && r.Phase == domain.OpSucceeded {
			_, err = tx.ExecContext(ctx, `UPDATE apps SET rolled_back=1, auto_sync=0 WHERE name=? AND host=?`, r.App, host)
		}
		if err == nil && r.Trigger == "rollback" && r.Phase == domain.OpSucceeded {
			// the rollback on the Host is the later, deliberate act: a Sync still waiting must not undo it on reconnect
			_, err = tx.ExecContext(ctx, `UPDATE ops SET phase=?, message='superseded by a rollback on the Host', finished_at=?
				WHERE app=? AND phase=?`, domain.OpSuperseded, at, r.App, domain.OpWaiting)
		}
	}
	if err != nil { // coverage: unreachable, statements after the first in a transaction that holds the write lock (_txlock=immediate) on a healthy SQLite
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE hosts SET outbox_seq=? WHERE name=?`, seq, host); err != nil { // coverage: unreachable, the same transaction, holding the write lock, on a healthy SQLite
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) SetAppDeleting(ctx context.Context, name, how string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE apps SET deleting = ? WHERE name = ?`, how, name))
}

func (s *Store) ApplyAgentRemoval(ctx context.Context, host string, seq uint64, app string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil { // coverage: unreachable, starting a transaction on a healthy SQLite
		return false, err
	}
	defer tx.Rollback()
	var last uint64
	if err := tx.QueryRowContext(ctx, `SELECT outbox_seq FROM hosts WHERE name = ?`, host).Scan(&last); err != nil {
		return false, mapErr(err)
	}
	if seq <= last {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE name = ? AND host = ? AND deleting <> ''`, app, host); err != nil { // coverage: unreachable, a healthy SQLite write
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE hosts SET outbox_seq = ? WHERE name = ?`, seq, host); err != nil { // coverage: unreachable, a healthy SQLite write
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) SaveAgentStatus(ctx context.Context, host, app string, st domain.AgentStatus) error {
	b, err := json.Marshal(st)
	if err != nil { // coverage: unreachable, a struct of strings, maps and slices always encodes
		return err
	}
	return affected(s.db.ExecContext(ctx, `INSERT INTO agent_status (app, status) SELECT name, ? FROM apps WHERE name = ? AND host = ?
		ON CONFLICT (app) DO UPDATE SET status = excluded.status`, string(b), app, host))
}

func (s *Store) AgentStatus(ctx context.Context, app string) (domain.AgentStatus, error) {
	var st domain.AgentStatus
	var b string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM agent_status WHERE app = ?`, app).Scan(&b); err != nil {
		return st, mapErr(err)
	}
	return st, json.Unmarshal([]byte(b), &st)
}

func (s *Store) SaveAgentDiff(ctx context.Context, app string, d domain.AgentDiff) error {
	b, err := json.Marshal(d)
	if err != nil { // coverage: unreachable, a struct of strings and slices always encodes
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_diff (app, diff) VALUES (?, ?)
		ON CONFLICT (app) DO UPDATE SET diff = excluded.diff`, app, string(b))
	if err != nil {
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
			return domain.ErrNotFound
		}
	}
	return err
}

func (s *Store) AgentDiff(ctx context.Context, app string) (domain.AgentDiff, error) {
	var d domain.AgentDiff
	var b string
	if err := s.db.QueryRowContext(ctx, `SELECT diff FROM agent_diff WHERE app = ?`, app).Scan(&b); err != nil {
		return d, mapErr(err)
	}
	return d, json.Unmarshal([]byte(b), &d)
}

func (s *Store) AgentLeft(ctx context.Context, host string, at time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE hosts SET agent = json_set(agent, '$.leftAt', ?) WHERE name = ? AND agent <> ''`,
		at.UTC().Format(time.RFC3339Nano), host))
}

func (s *Store) RecordAgent(ctx context.Context, host string, info domain.AgentInfo) error {
	b, err := json.Marshal(info)
	if err != nil { // coverage: unreachable, marshalling a plain struct
		return err
	}
	return affected(s.db.ExecContext(ctx, `UPDATE hosts SET agent = ? WHERE name = ?`, string(b), host))
}

func (s *Store) UnlockHost(ctx context.Context, host string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE hosts SET locked = '' WHERE name = ?`, host))
}

// ---- enrollment ----

func (s *Store) CreateInvite(ctx context.Context, host string, expires time.Time) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	invite := "omegacd-invite-" + hex.EncodeToString(b)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var key []byte
	err = tx.QueryRowContext(ctx, `SELECT signing_key FROM hosts WHERE name = ?`, host).Scan(&key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO hosts (name, created_at) VALUES (?, ?)`, host, toUnix(time.Now())); err != nil { // coverage: unreachable, this transaction just found no Host by that name, and SQLite has one writer
			return "", mapErr(err)
		}
	case err != nil: // coverage: unreachable, a read in a transaction holding the write lock (_txlock=immediate) on a healthy SQLite
		return "", err
	case len(key) > 0:
		return "", fmt.Errorf("%w: an Agent is enrolled for Host %s already", domain.ErrAlreadyExists, host)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO invites (host, hash, expires_at) VALUES (?, ?, ?)
		ON CONFLICT (host) DO UPDATE SET hash = excluded.hash, expires_at = excluded.expires_at`,
		host, hashToken(invite), toUnix(expires)); err != nil { // coverage: unreachable, the same transaction, holding the write lock, on a healthy SQLite
		return "", err
	}
	return invite, tx.Commit()
}

// errInvite is all an Agent learns about an invite it can't use: not which of unknown, used or expired
var errInvite = fmt.Errorf("%w: the invite is unknown, used or expired: add the Host again for a new one", domain.ErrInvalid)

func (s *Store) Enroll(ctx context.Context, invite string, signingKey []byte, ageRecipient string, now time.Time) (domain.Host, error) {
	if len(signingKey) == 0 || ageRecipient == "" {
		return domain.Host{}, fmt.Errorf("%w: enrolling needs the Agent's signing key and age recipient", domain.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Host{}, err
	}
	defer tx.Rollback()
	var host string
	var expires int64
	err = tx.QueryRowContext(ctx, `DELETE FROM invites WHERE hash = ? RETURNING host, expires_at`, hashToken(invite)).Scan(&host, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Host{}, errInvite
	}
	if err != nil { // coverage: unreachable, a statement in a transaction holding the write lock (_txlock=immediate) on a healthy SQLite
		return domain.Host{}, err
	}
	if !now.Before(fromUnix(expires)) {
		tx.Commit() // an expired invite is used up too
		return domain.Host{}, errInvite
	}
	if _, err := tx.ExecContext(ctx, `UPDATE hosts SET signing_key = ?, age_recipient = ?, approved = 0 WHERE name = ?`,
		signingKey, ageRecipient, host); err != nil { // coverage: unreachable, the same transaction, holding the write lock, on a healthy SQLite
		return domain.Host{}, mapErr(err)
	}
	if err := tx.Commit(); err != nil { // coverage: unreachable, committing a healthy SQLite transaction
		return domain.Host{}, err
	}
	return s.GetHost(ctx, host)
}

// SetAgentApps records what a Host's Agent said at its resync: each Application it runs with its deployed tree's
// checksum (Q119), and the ones the Control Plane doesn't know (Q118).
func (s *Store) SetAgentApps(ctx context.Context, host string, apps map[string]string, unmanaged []string) error {
	a, _ := json.Marshal(apps)
	u, _ := json.Marshal(append([]string{}, unmanaged...)) // [] for none, never null
	return affected(s.db.ExecContext(ctx, `UPDATE hosts SET agent_apps = ?, unmanaged = ? WHERE name = ?`, string(a), string(u), host))
}

func (s *Store) ApproveHost(ctx context.Context, host, fingerprint string) error {
	h, err := s.GetHost(ctx, host)
	if err != nil {
		return err
	}
	if h.Fingerprint == "" {
		return fmt.Errorf("%w: no Agent has enrolled for Host %s yet", domain.ErrInvalid, host)
	}
	if strings.TrimSpace(fingerprint) != h.Fingerprint {
		return fmt.Errorf("%w: Host %s's Agent has the key %s, not %s: compare again with what the Agent printed",
			domain.ErrInvalid, host, h.Fingerprint, fingerprint)
	}
	// the key too: approved is for the key just compared, not one that replaced it meanwhile
	return affected(s.db.ExecContext(ctx, `UPDATE hosts SET approved = 1 WHERE name = ? AND signing_key = ?`, host, h.SigningKey))
}

func (s *Store) DeleteHost(ctx context.Context, name string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM hosts WHERE name = ?`, name))
}

// ---- tokens ----

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func (s *Store) ListTokens(ctx context.Context) ([]domain.APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, hash, created_at, last_used FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []domain.APIToken{}
	for rows.Next() {
		var t domain.APIToken
		var created, used int64
		if err := rows.Scan(&t.ID, &t.Name, &t.Hash, &created, &used); err != nil { // coverage: unreachable, scanning fixed columns into their own types
			return nil, err
		}
		t.CreatedAt, t.LastUsed = fromUnix(created), fromUnix(used)
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

func (s *Store) CreateToken(ctx context.Context, name string) (domain.APIToken, string, error) {
	if name == "" {
		return domain.APIToken{}, "", fmt.Errorf("%w: token name is required", domain.ErrInvalid)
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	plaintext := "ocd_" + base64.RawURLEncoding.EncodeToString(raw)
	t := domain.APIToken{Name: name, Hash: hashToken(plaintext), CreatedAt: time.Now().UTC()}
	res, err := s.db.ExecContext(ctx, `INSERT INTO tokens (name, hash, created_at, last_used) VALUES (?,?,?,0)`,
		t.Name, t.Hash, toUnix(t.CreatedAt))
	if err != nil {
		return domain.APIToken{}, "", mapErr(err)
	}
	if t.ID, err = res.LastInsertId(); err != nil { // coverage: unreachable, the sqlite driver always reports the row it inserted
		return domain.APIToken{}, "", err
	}
	return t, plaintext, nil
}

func (s *Store) DeleteToken(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id))
}

func (s *Store) VerifyToken(ctx context.Context, plaintext string) (domain.APIToken, error) {
	var t domain.APIToken
	var created, used int64
	now := time.Now().UTC()
	err := s.db.QueryRowContext(ctx, `UPDATE tokens SET last_used = ? WHERE hash = ? RETURNING id, name, hash, created_at, last_used`,
		toUnix(now), hashToken(plaintext)).Scan(&t.ID, &t.Name, &t.Hash, &created, &used)
	if err != nil {
		return t, mapErr(err)
	}
	t.CreatedAt, t.LastUsed = fromUnix(created), fromUnix(used)
	return t, nil
}

// ---- admin ----

func (s *Store) AdminPasswordHash(ctx context.Context) (string, bool, error) {
	var hash string
	var must bool
	err := s.db.QueryRowContext(ctx, `SELECT password_hash, must_change FROM admin WHERE id = 1`).Scan(&hash, &must)
	return hash, must, mapErr(err)
}

func (s *Store) SetAdminPassword(ctx context.Context, hash string, mustChange bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin (id, password_hash, must_change) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET password_hash = excluded.password_hash, must_change = excluded.must_change`, hash, mustChange)
	return err
}

// ---- sessions ----

func (s *Store) CreateSession(ctx context.Context, tokenHash string, expires time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, time.Now().UnixNano()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (hash, expires_at) VALUES (?, ?)`, tokenHash, toUnix(expires))
	return mapErr(err)
}

func (s *Store) GetSession(ctx context.Context, tokenHash string) (time.Time, error) {
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM sessions WHERE hash = ? AND expires_at > ?`,
		tokenHash, time.Now().UnixNano()).Scan(&exp)
	return fromUnix(exp), mapErr(err)
}

func (s *Store) ExtendSession(ctx context.Context, tokenHash string, expires time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE hash = ?`, toUnix(expires), tokenHash))
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteSessionsExcept(ctx context.Context, keepHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE hash != ?`, keepHash)
	return err
}
