package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/obleey/omegacd/internal/domain"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// migrations are applied in order; PRAGMA user_version records how many ran. Only ever append.
var migrations = []string{`
CREATE TABLE apps (
	name TEXT PRIMARY KEY,
	parent TEXT NOT NULL,          -- its App of Apps, or ''
	revision TEXT NOT NULL,        -- deployed; '' before the first Sync
	self_heal INTEGER NOT NULL,
	prune INTEGER NOT NULL,
	prune_volumes INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
-- the cache index: the Desired State of each Revision kept for an Application (its files are blobs in the cache)
CREATE TABLE revisions (
	app TEXT NOT NULL,
	revision TEXT NOT NULL,
	checksum TEXT NOT NULL,
	files TEXT NOT NULL,           -- JSON []domain.File
	compose_files TEXT NOT NULL,   -- JSON []string
	profiles TEXT NOT NULL,        -- JSON []string
	added_at INTEGER NOT NULL,
	PRIMARY KEY (app, revision)
);
-- what happened while nobody was listening, reported in seq order on reconnect and deleted when acknowledged
CREATE TABLE outbox (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	recorded_at INTEGER NOT NULL,
	entry BLOB NOT NULL            -- an agentv1.OutboxEntry
);
`, `
-- recency without clock ties: each add (or re-add, a Rollback) takes the next number
ALTER TABLE revisions ADD COLUMN used INTEGER NOT NULL DEFAULT 0;
UPDATE revisions SET used = rowid;
`, `
-- the folder the Agent writes the Application's files to: its own, whatever is in it (workdir.Claim)
ALTER TABLE apps ADD COLUMN dir TEXT NOT NULL DEFAULT '';
`}

// DB is the Agent's own small database in its state dir (Q48).
type DB struct {
	db   *sql.DB
	path string
}

// App is an Application on this Host, with what the Agent needs to keep it running offline.
type App struct {
	Name, Parent string
	Revision     string // deployed
	Dir          string // its folder in the work dir, once written
	SelfHeal     bool
	Prune        bool
	PruneVolumes bool
}

// Revision is an Application's Desired State at one Revision, as cached.
type Revision struct {
	App, Revision, Checksum string
	Files                   []domain.File
	ComposeFiles, Profiles  []string
	AddedAt                 time.Time
}

func OpenDB(path string) (*DB, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d := &DB{db: db, path: path}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) migrate() error {
	var v int
	if err := d.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("agent database is version %d, newer than this Agent knows (%d): upgrade the Agent", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := d.db.Begin()
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

func (d *DB) Close() error { return d.db.Close() }

// ---- Applications ----

func (d *DB) SaveApp(ctx context.Context, a App) error {
	_, err := d.db.ExecContext(ctx, `INSERT INTO apps (name, parent, revision, dir, self_heal, prune, prune_volumes, updated_at)
		VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET parent=excluded.parent, revision=excluded.revision,
		dir=excluded.dir, self_heal=excluded.self_heal, prune=excluded.prune, prune_volumes=excluded.prune_volumes,
		updated_at=excluded.updated_at`,
		a.Name, a.Parent, a.Revision, a.Dir, a.SelfHeal, a.Prune, a.PruneVolumes, time.Now().UnixMilli())
	return err
}

func (d *DB) GetApp(ctx context.Context, name string) (App, error) {
	var a App
	err := d.db.QueryRowContext(ctx, `SELECT name, parent, revision, dir, self_heal, prune, prune_volumes FROM apps WHERE name = ?`, name).
		Scan(&a.Name, &a.Parent, &a.Revision, &a.Dir, &a.SelfHeal, &a.Prune, &a.PruneVolumes)
	if errors.Is(err, sql.ErrNoRows) {
		return a, domain.ErrNotFound
	}
	return a, err
}

func (d *DB) ListApps(ctx context.Context) ([]App, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT name, parent, revision, dir, self_heal, prune, prune_volumes FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apps []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.Name, &a.Parent, &a.Revision, &a.Dir, &a.SelfHeal, &a.Prune, &a.PruneVolumes); err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// DeleteApp forgets an Application and its cached Revisions.
func (d *DB) DeleteApp(ctx context.Context, name string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM revisions WHERE app = ?`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE name = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- the cache index ----

// AddRevision records r as cached; adding it again only refreshes it.
func (d *DB) AddRevision(ctx context.Context, r Revision) error {
	files, err := json.Marshal(r.Files)
	if err != nil {
		return err
	}
	cf, _ := json.Marshal(r.ComposeFiles)
	pr, _ := json.Marshal(r.Profiles)
	_, err = d.db.ExecContext(ctx, `INSERT INTO revisions (app, revision, checksum, files, compose_files, profiles, added_at, used)
		VALUES (?,?,?,?,?,?,?, (SELECT COALESCE(MAX(used), 0) + 1 FROM revisions))
		ON CONFLICT(app, revision) DO UPDATE SET checksum=excluded.checksum, files=excluded.files,
		compose_files=excluded.compose_files, profiles=excluded.profiles, added_at=excluded.added_at, used=excluded.used`,
		r.App, r.Revision, r.Checksum, files, cf, pr, time.Now().UnixMilli())
	return err
}

// Revisions is app's cached Revisions, most recently added first.
func (d *DB) Revisions(ctx context.Context, app string) ([]Revision, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT app, revision, checksum, files, compose_files, profiles, added_at
		FROM revisions WHERE app = ? ORDER BY used DESC`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var r Revision
		var files, cf, pr string
		var added int64
		if err := rows.Scan(&r.App, &r.Revision, &r.Checksum, &files, &cf, &pr, &added); err != nil {
			return nil, err
		}
		if err := errors.Join(json.Unmarshal([]byte(files), &r.Files), json.Unmarshal([]byte(cf), &r.ComposeFiles),
			json.Unmarshal([]byte(pr), &r.Profiles)); err != nil {
			return nil, fmt.Errorf("revision %s of %s: %w", r.Revision, r.App, err)
		}
		r.AddedAt = time.UnixMilli(added)
		out = append(out, r)
	}
	return out, rows.Err()
}

// files is the hash of every file a cached Revision uses.
func (d *DB) files(ctx context.Context) (map[string]bool, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT files FROM revisions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[string]bool{}
	for rows.Next() {
		var js string
		var files []domain.File
		if err := rows.Scan(&js); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(js), &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			used[f.SHA256] = true
		}
	}
	return used, rows.Err()
}

func (d *DB) DeleteRevision(ctx context.Context, app, revision string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM revisions WHERE app = ? AND revision = ?`, app, revision)
	return err
}

// ---- the outbox ----

// Record appends e to the outbox; it gets the next seq and the current time.
func (d *DB) Record(ctx context.Context, e *agentv1.OutboxEntry) (uint64, error) {
	now := time.Now()
	e = proto.CloneOf(e)
	e.Seq, e.TimeUnix = 0, now.Unix()
	b, err := proto.Marshal(e)
	if err != nil {
		return 0, err
	}
	res, err := d.db.ExecContext(ctx, `INSERT INTO outbox (recorded_at, entry) VALUES (?, ?)`, now.UnixMilli(), b)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	return uint64(seq), err
}

// Outbox is every unacknowledged entry, oldest first, each with its seq.
func (d *DB) Outbox(ctx context.Context) ([]*agentv1.OutboxEntry, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT seq, entry FROM outbox ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*agentv1.OutboxEntry
	for rows.Next() {
		var seq uint64
		var b []byte
		if err := rows.Scan(&seq, &b); err != nil {
			return nil, err
		}
		e := &agentv1.OutboxEntry{}
		if err := proto.Unmarshal(b, e); err != nil {
			return nil, fmt.Errorf("outbox entry %d: %w", seq, err)
		}
		e.Seq = seq
		out = append(out, e)
	}
	return out, rows.Err()
}

// Ack deletes the entries up to and including seq: the Control Plane has them.
func (d *DB) Ack(ctx context.Context, seq uint64) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM outbox WHERE seq <= ?`, seq)
	return err
}
