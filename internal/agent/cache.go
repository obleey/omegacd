package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
)

// Cache keeps the Revisions an Agent deployed, so it can restore, self-heal and roll back without the Control
// Plane (Q24, Q25, Q32). Files are blobs shared across Revisions and Applications, indexed in the database. It holds
// only what the Source Server rendered: Encrypted Secrets stay ciphertext, plaintext exists only in the work dir (Q19).
type Cache struct {
	db    *DB
	blobs *blob.Store
	keep  int // Revisions per Application, besides the deployed one
}

func OpenCache(db *DB, dir string, keep int) (*Cache, error) {
	blobs, err := blob.Open(dir)
	if err != nil {
		return nil, err
	}
	return &Cache{db: db, blobs: blobs, keep: keep}, nil
}

// Missing is the hashes of r's files the cache lacks: all the Agent has to ask for.
func (c *Cache) Missing(r Revision) []string {
	var hashes []string
	for _, f := range r.Files {
		if !c.blobs.Has(f.SHA256) && !slices.Contains(hashes, f.SHA256) {
			hashes = append(hashes, f.SHA256)
		}
	}
	return hashes
}

// Add caches r, getting each missing file with fetch, then drops r's Application's older Revisions beyond what it
// keeps. Adding a cached Revision again fetches nothing and makes it the newest.
func (c *Cache) Add(ctx context.Context, r Revision, fetch func(hash string) ([]byte, error)) error {
	for _, h := range c.Missing(r) {
		b, err := fetch(h)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", h, err)
		}
		if blob.Hash(b) != h { // never store what wasn't asked for
			return fmt.Errorf("fetch %s: got content with hash %s", h, blob.Hash(b))
		}
		if _, err := c.blobs.Put(b); err != nil {
			return err
		}
	}
	if err := c.db.AddRevision(ctx, r); err != nil {
		return err
	}
	return c.trim(ctx, r.App)
}

// trim keeps app's newest c.keep Revisions plus the deployed one.
func (c *Cache) trim(ctx context.Context, app string) error {
	revs, err := c.db.Revisions(ctx, app)
	if err != nil {
		return err
	}
	a, err := c.db.GetApp(ctx, app)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	for i, r := range revs {
		if i >= c.keep && r.Revision != a.Revision {
			if err := c.db.DeleteRevision(ctx, app, r.Revision); err != nil {
				return err
			}
		}
	}
	return nil
}

// Revision is a cached Revision of app, or ErrNotFound.
func (c *Cache) Revision(ctx context.Context, app, revision string) (Revision, error) {
	revs, err := c.db.Revisions(ctx, app)
	if err != nil {
		return Revision{}, err
	}
	if i := slices.IndexFunc(revs, func(r Revision) bool { return r.Revision == revision }); i >= 0 {
		return revs[i], nil
	}
	return Revision{}, fmt.Errorf("revision %s of %s is not cached: %w", revision, app, domain.ErrNotFound)
}

// Blob is a cached file's content, checked against its hash.
func (c *Cache) Blob(hash string) ([]byte, error) { return c.blobs.Get(hash) }

// Collect removes the files no cached Revision uses that are older than grace (a grace keeps an Add in progress safe).
func (c *Cache) Collect(ctx context.Context, grace time.Duration) (int, error) {
	used, err := c.db.files(ctx)
	if err != nil {
		return 0, err
	}
	return c.blobs.GC(func(h string) bool { return used[h] }, grace)
}
