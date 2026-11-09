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
`, `
CREATE TABLE sessions (
	hash TEXT PRIMARY KEY,
	expires_at INTEGER NOT NULL
);
`, `
CREATE TABLE hosts (
	name TEXT PRIMARY KEY,
	url TEXT NOT NULL,
	work_dir TEXT NOT NULL,
	ssh_private_key TEXT NOT NULL,
	tls_ca TEXT NOT NULL,
	tls_cert TEXT NOT NULL,
	tls_key TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`, `
ALTER TABLE apps ADD COLUMN kind TEXT NOT NULL DEFAULT 'compose';
ALTER TABLE apps ADD COLUMN parent TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN not_in_git INTEGER NOT NULL DEFAULT 0;
`, `
ALTER TABLE apps ADD COLUMN prune_volumes INTEGER NOT NULL DEFAULT 0;
`, `
CREATE TABLE host_keys (
	host TEXT NOT NULL REFERENCES hosts(name) ON DELETE CASCADE,
	recipient TEXT NOT NULL,
	private_key TEXT NOT NULL,
	current INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (host, recipient)
);
`, `
ALTER TABLE apps ADD COLUMN dirs TEXT NOT NULL DEFAULT '[]';
`, `
ALTER TABLE apps ADD COLUMN work_dir TEXT NOT NULL DEFAULT '';
`, `
ALTER TABLE hosts ADD COLUMN signing_key BLOB;
ALTER TABLE hosts ADD COLUMN age_recipient TEXT NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN approved INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX hosts_signing_key ON hosts(signing_key);
CREATE TABLE invites (
	host TEXT PRIMARY KEY REFERENCES hosts(name) ON DELETE CASCADE,
	hash TEXT NOT NULL UNIQUE,
	expires_at INTEGER NOT NULL
);
`, `
ALTER TABLE hosts ADD COLUMN previous_signing_key BLOB;
ALTER TABLE hosts ADD COLUMN locked TEXT NOT NULL DEFAULT '';
`, `
ALTER TABLE hosts ADD COLUMN agent TEXT NOT NULL DEFAULT '';
`, `
ALTER TABLE hosts ADD COLUMN allow_ips TEXT NOT NULL DEFAULT '[]';
`}

type Store struct {
	db     *sql.DB
	sealer *crypto.Sealer
}

var _ domain.Store = (*Store)(nil)

func Open(path string, sealer *crypto.Sealer) (*Store, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)&_txlock=immediate"
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

const appCols = `name, repo_url, path, target_revision, compose_files, profiles, host, auto_sync, self_heal, prune, adopt, rolled_back, kind, parent, not_in_git, prune_volumes, work_dir, created_at`

// appRead is appCols plus what only SetAppDirs writes.
const appRead = appCols + `, dirs`

func scanApp(r scanner) (domain.Application, error) {
	var a domain.Application
	var files, profiles, dirs string
	var created int64
	if err := r.Scan(&a.Name, &a.RepoURL, &a.Path, &a.TargetRevision, &files, &profiles, &a.Host,
		&a.AutoSync, &a.SelfHeal, &a.Prune, &a.Adopt, &a.RolledBack, &a.Kind, &a.Parent, &a.NotInGit, &a.PruneVolumes, &a.WorkDir, &created, &dirs); err != nil {
		return a, mapErr(err)
	}
	a.CreatedAt = fromUnix(created)
	if err := json.Unmarshal([]byte(files), &a.ComposeFiles); err != nil {
		return a, err
	}
	if err := json.Unmarshal([]byte(dirs), &a.Dirs); err != nil {
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

func (s *Store) SetAppDirs(ctx context.Context, name string, dirs []string) error {
	b, _ := json.Marshal(dirs)
	return affected(s.db.ExecContext(ctx, `UPDATE apps SET dirs=? WHERE name=?`, string(b), name))
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

// ---- hosts ----

// persistent, so bind-mounted files survive a reboot of the Host
const defaultWorkDir = "/var/lib/omegacd"

const hostCols = `name, url, work_dir, ssh_private_key, tls_ca, tls_cert, tls_key, created_at`

// hostSelect reads a Host with its Agent's enrollment, which SaveHost never changes
const hostSelect = `SELECT ` + hostCols + `, signing_key, age_recipient, approved, locked, agent, allow_ips FROM hosts`

func (s *Store) scanHost(r scanner) (domain.Host, error) {
	var h domain.Host
	var sealed [4]string
	var created int64
	var locked, agent, allow string
	if err := r.Scan(&h.Name, &h.URL, &h.WorkDir, &sealed[0], &sealed[1], &sealed[2], &sealed[3], &created,
		&h.SigningKey, &h.AgeRecipient, &h.Approved, &locked, &agent, &allow); err != nil {
		return h, mapErr(err)
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
	for i, dst := range []*string{&h.SSHPrivateKey, &h.TLSCA, &h.TLSCert, &h.TLSKey} {
		var err error
		if *dst, err = s.sealer.Open(sealed[i]); err != nil {
			return h, fmt.Errorf("open host %s: %w", h.Name, err)
		}
	}
	h.HasSSHKey, h.HasTLS = h.SSHPrivateKey != "", h.TLSCert != "" && h.TLSKey != ""
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
	if err != nil {
		return err
	}
	return affected(s.db.ExecContext(ctx, `UPDATE hosts SET locked = ? WHERE name = ?`, string(b), host))
}

func (s *Store) SetAllowIPs(ctx context.Context, host string, cidrs []string) error {
	b, err := json.Marshal(cidrs)
	if err != nil {
		return err
	}
	if len(cidrs) == 0 {
		b = []byte("[]")
	}
	return affected(s.db.ExecContext(ctx, `UPDATE hosts SET allow_ips = ? WHERE name = ?`, string(b), host))
}

func (s *Store) RecordAgent(ctx context.Context, host string, info domain.AgentInfo) error {
	b, err := json.Marshal(info)
	if err != nil {
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
	var url string
	var key []byte
	err = tx.QueryRowContext(ctx, `SELECT url, signing_key FROM hosts WHERE name = ?`, host).Scan(&url, &key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO hosts (`+hostCols+`) VALUES (?, '', ?, '', '', '', '', ?)`,
			host, defaultWorkDir, toUnix(time.Now())); err != nil {
			return "", mapErr(err)
		}
	case err != nil:
		return "", err
	case url != "":
		return "", fmt.Errorf("%w: Host %s is reached over SSH or TLS (%s), not through an Agent", domain.ErrInvalid, host, url)
	case len(key) > 0:
		return "", fmt.Errorf("%w: an Agent is enrolled for Host %s already", domain.ErrAlreadyExists, host)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO invites (host, hash, expires_at) VALUES (?, ?, ?)
		ON CONFLICT (host) DO UPDATE SET hash = excluded.hash, expires_at = excluded.expires_at`,
		host, hashToken(invite), toUnix(expires)); err != nil {
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
	if err != nil {
		return domain.Host{}, err
	}
	if !now.Before(fromUnix(expires)) {
		tx.Commit() // an expired invite is used up too
		return domain.Host{}, errInvite
	}
	if _, err := tx.ExecContext(ctx, `UPDATE hosts SET signing_key = ?, age_recipient = ?, approved = 0 WHERE name = ?`,
		signingKey, ageRecipient, host); err != nil {
		return domain.Host{}, mapErr(err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Host{}, err
	}
	return s.GetHost(ctx, host)
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

func (s *Store) SaveHost(ctx context.Context, h domain.Host) error {
	if h.WorkDir == "" {
		h.WorkDir = defaultWorkDir
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now()
	}
	args := []any{h.Name, h.URL, h.WorkDir}
	for _, v := range []string{h.SSHPrivateKey, h.TLSCA, h.TLSCert, h.TLSKey} {
		sealed, err := s.sealer.Seal(v)
		if err != nil {
			return err
		}
		args = append(args, sealed)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO hosts (`+hostCols+`) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (name) DO UPDATE SET url = excluded.url, work_dir = excluded.work_dir,
		ssh_private_key = COALESCE(NULLIF(excluded.ssh_private_key, ''), ssh_private_key),
		tls_ca = COALESCE(NULLIF(excluded.tls_ca, ''), tls_ca),
		tls_cert = COALESCE(NULLIF(excluded.tls_cert, ''), tls_cert),
		tls_key = COALESCE(NULLIF(excluded.tls_key, ''), tls_key)`, append(args, toUnix(h.CreatedAt))...)
	return mapErr(err)
}

func (s *Store) DeleteHost(ctx context.Context, name string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM hosts WHERE name = ?`, name))
}

// ---- host keys ----

func (s *Store) ListHostKeys(ctx context.Context, host string) ([]domain.HostKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT recipient, private_key, current, created_at FROM host_keys
		WHERE host = ? ORDER BY current DESC, rowid DESC`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []domain.HostKey{}
	for rows.Next() {
		k := domain.HostKey{Host: host}
		var sealed string
		var created int64
		if err := rows.Scan(&k.Recipient, &sealed, &k.Current, &created); err != nil {
			return nil, err
		}
		k.CreatedAt = fromUnix(created)
		if k.PrivateKey, err = s.sealer.Open(sealed); err != nil {
			return nil, fmt.Errorf("open host key %s of %s: %w", k.Recipient, host, err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Store) AddHostKey(ctx context.Context, k domain.HostKey) error {
	sealed, err := s.sealer.Seal(k.PrivateKey)
	if err != nil {
		return err
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE host_keys SET current = 0 WHERE host = ?`, k.Host); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO host_keys (host, recipient, private_key, current, created_at) VALUES (?,?,?,1,?)`,
		k.Host, k.Recipient, sealed, toUnix(k.CreatedAt)); err != nil {
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
			return domain.ErrNotFound
		}
		return mapErr(err)
	}
	return tx.Commit()
}

func (s *Store) DeleteHostKey(ctx context.Context, host, recipient string) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM host_keys WHERE host = ? AND recipient = ?`, host, recipient))
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
	plaintext := "ocd_" + base64.RawURLEncoding.EncodeToString(raw)
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
