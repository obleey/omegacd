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
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/obleey/dockercd/internal/crypto"
	"github.com/obleey/dockercd/internal/domain"
)

const historyLimit = 20

// migrations are applied in order; PRAGMA user_version records how many ran.
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
	adopt INTEGER NOT NULL,
	rolled_back INTEGER NOT NULL,
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
CREATE TABLE repo_credentials (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	url_prefix TEXT NOT NULL UNIQUE,
	username TEXT NOT NULL,
	password TEXT NOT NULL,
	ssh_private_key TEXT NOT NULL
);
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
`}

type Store struct {
	db     *sql.DB
	sealer *crypto.Sealer
}

var _ domain.Store = (*Store)(nil)

func Open(path string, sealer *crypto.Sealer) (*Store, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, sealer: sealer}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
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
	if n, err := res.RowsAffected(); err != nil {
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

const appCols = `name, repo_url, path, target_revision, compose_files, profiles, host, auto_sync, self_heal, prune, adopt, rolled_back, created_at`

func scanApp(r scanner) (domain.Application, error) {
	var a domain.Application
	var files, profiles string
	var created int64
	if err := r.Scan(&a.Name, &a.RepoURL, &a.Path, &a.TargetRevision, &files, &profiles, &a.Host,
		&a.AutoSync, &a.SelfHeal, &a.Prune, &a.Adopt, &a.RolledBack, &created); err != nil {
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
		a.AutoSync, a.SelfHeal, a.Prune, a.Adopt, a.RolledBack}
}

func (s *Store) ListApps(ctx context.Context) ([]domain.Application, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appCols+` FROM apps ORDER BY name`)
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
	return scanApp(s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE name = ?`, name))
}

func (s *Store) CreateApp(ctx context.Context, a domain.Application) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO apps (`+appCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		append(appArgs(a), toUnix(a.CreatedAt))...)
	return mapErr(err)
}

// UpdateApp keeps CreatedAt; the app is identified by Name.
func (s *Store) UpdateApp(ctx context.Context, a domain.Application) error {
	args := appArgs(a)
	return affected(s.db.ExecContext(ctx, `UPDATE apps SET repo_url=?, path=?, target_revision=?, compose_files=?,
		profiles=?, host=?, auto_sync=?, self_heal=?, prune=?, adopt=?, rolled_back=? WHERE name=?`, append(args[1:], a.Name)...))
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

func (s *Store) FinishOp(ctx context.Context, id int64, phase domain.OpPhase, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var app string
	if err := tx.QueryRowContext(ctx, `UPDATE ops SET phase=?, message=?, finished_at=? WHERE id=? RETURNING app`,
		phase, message, time.Now().UnixNano(), id).Scan(&app); err != nil {
		return mapErr(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ops WHERE app = ?1 AND id NOT IN
		(SELECT id FROM ops WHERE app = ?1 ORDER BY id DESC LIMIT ?2)`, app, historyLimit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetOp(ctx context.Context, id int64) (domain.SyncOperation, error) {
	return scanOp(s.db.QueryRowContext(ctx, `SELECT `+opCols+` FROM ops WHERE id = ?`, id))
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
		if err != nil {
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

func (s *Store) ListRepoCredentials(ctx context.Context) ([]domain.RepoCredential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, url_prefix, username, password, ssh_private_key FROM repo_credentials ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	creds := []domain.RepoCredential{}
	for rows.Next() {
		var c domain.RepoCredential
		var pw, key string
		if err := rows.Scan(&c.ID, &c.URLPrefix, &c.Username, &pw, &key); err != nil {
			return nil, err
		}
		if c.Password, err = s.sealer.Open(pw); err != nil {
			return nil, fmt.Errorf("open repo credential %d: %w", c.ID, err)
		}
		if c.SSHPrivateKey, err = s.sealer.Open(key); err != nil {
			return nil, fmt.Errorf("open repo credential %d: %w", c.ID, err)
		}
		c.HasPassword, c.HasSSHKey = c.Password != "", c.SSHPrivateKey != ""
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

func (s *Store) SaveRepoCredential(ctx context.Context, c domain.RepoCredential) (int64, error) {
	pw, err := s.sealer.Seal(c.Password)
	if err != nil {
		return 0, err
	}
	key, err := s.sealer.Seal(c.SSHPrivateKey)
	if err != nil {
		return 0, err
	}
	if c.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO repo_credentials (url_prefix, username, password, ssh_private_key)
			VALUES (?,?,?,?)`, c.URLPrefix, c.Username, pw, key)
		if err != nil {
			return 0, mapErr(err)
		}
		return res.LastInsertId()
	}
	return c.ID, affected(s.db.ExecContext(ctx, `UPDATE repo_credentials SET url_prefix=?, username=?,
		password=COALESCE(NULLIF(?, ''), password), ssh_private_key=COALESCE(NULLIF(?, ''), ssh_private_key) WHERE id=?`,
		c.URLPrefix, c.Username, pw, key, c.ID))
}

func (s *Store) DeleteRepoCredential(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM repo_credentials WHERE id = ?`, id))
}

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
		if err := rows.Scan(&c.ID, &c.Registry, &c.Username, &pw); err != nil {
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
	if err != nil {
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
		if err := rows.Scan(&t.ID, &t.Name, &t.Hash, &created, &used); err != nil {
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
	plaintext := "dcd_" + base64.RawURLEncoding.EncodeToString(raw)
	t := domain.APIToken{Name: name, Hash: hashToken(plaintext), CreatedAt: time.Now().UTC()}
	res, err := s.db.ExecContext(ctx, `INSERT INTO tokens (name, hash, created_at, last_used) VALUES (?,?,?,0)`,
		t.Name, t.Hash, toUnix(t.CreatedAt))
	if err != nil {
		return domain.APIToken{}, "", mapErr(err)
	}
	if t.ID, err = res.LastInsertId(); err != nil {
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
