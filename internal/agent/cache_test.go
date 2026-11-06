package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/workdir"
)

// source plays the Control Plane: rendered files by hash, counting what it was asked for.
type source struct {
	blobs   map[string][]byte
	fetched int
}

func (s *source) revision(app, rev string, files map[string]string) Revision {
	if s.blobs == nil {
		s.blobs = map[string][]byte{}
	}
	r := Revision{App: app, Revision: rev, Checksum: "c-" + rev, ComposeFiles: []string{"compose.yaml"}}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		h := blob.Hash([]byte(files[p]))
		s.blobs[h] = []byte(files[p])
		r.Files = append(r.Files, domain.File{Path: p, Mode: 0o644, SHA256: h})
	}
	return r
}

func (s *source) fetch(h string) ([]byte, error) {
	s.fetched++
	if b, ok := s.blobs[h]; ok {
		return b, nil
	}
	return nil, domain.ErrNotFound
}

func openCache(t *testing.T, keep int) (*Cache, *DB, string) {
	state := t.TempDir()
	db := openDB(t, filepath.Join(state, "agent.db"))
	t.Cleanup(func() { db.Close() })
	c, err := OpenCache(db, filepath.Join(state, "cache"), keep)
	if err != nil {
		t.Fatal(err)
	}
	return c, db, state
}

func cached(t *testing.T, db *DB, app string) (revs []string) {
	rs, err := db.Revisions(t.Context(), app)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		revs = append(revs, r.Revision)
	}
	return revs
}

func TestCacheRetention(t *testing.T) {
	c, db, _ := openCache(t, 3)
	ctx := t.Context()
	src := &source{}
	db.SaveApp(ctx, App{Name: "web", Revision: "r1"}) // r1 stays deployed while newer ones arrive
	for i := 1; i <= 6; i++ {
		rev := fmt.Sprintf("r%d", i)
		if err := c.Add(ctx, src.revision("web", rev, map[string]string{"compose.yaml": "v" + rev}), src.fetch); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := cached(t, db, "web"), []string{"r6", "r5", "r4", "r1"}; !slices.Equal(got, want) {
		t.Fatalf("cached %v, want the 3 newest plus the deployed one: %v", got, want)
	}
	if n, err := c.Collect(ctx, 0); err != nil || n != 2 { // r2's and r3's compose.yaml
		t.Fatalf("Collect = %d, %v; want 2", n, err)
	}
}

func TestCacheRollbackNeedsNoDownload(t *testing.T) {
	c, db, _ := openCache(t, 10)
	ctx := t.Context()
	src := &source{}
	shared := "services: {web: {image: nginx}}\n"
	r1 := src.revision("web", "r1", map[string]string{"compose.yaml": shared, "conf/nginx.conf": "v1"})
	r2 := src.revision("web", "r2", map[string]string{"compose.yaml": shared, "conf/nginx.conf": "v2"})
	c.Add(ctx, r1, src.fetch)
	if err := c.Add(ctx, r2, src.fetch); err != nil || src.fetched != 3 {
		t.Fatalf("fetched %d (%v); want 3: compose.yaml once, shared between Revisions", src.fetched, err)
	}
	// another Application with the same file shares the blob too
	if err := c.Add(ctx, src.revision("api", "a1", map[string]string{"compose.yaml": shared}), src.fetch); err != nil || src.fetched != 3 {
		t.Fatalf("fetched %d (%v) for an already cached file", src.fetched, err)
	}

	before := src.fetched
	back, err := c.Revision(ctx, "web", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Missing(back)) != 0 || c.Add(ctx, back, func(string) ([]byte, error) { return nil, errors.New("offline") }) != nil {
		t.Fatal("a Rollback to a cached Revision wanted a download")
	}
	if src.fetched != before || cached(t, db, "web")[0] != "r1" {
		t.Fatalf("fetched %d; newest is %v, want r1", src.fetched-before, cached(t, db, "web"))
	}
	dir := t.TempDir()
	if err := workdir.Write(dir, back.Files, c.Blob); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "conf/nginx.conf")); string(b) != "v1" {
		t.Fatalf("rolled back file: %q", b)
	}
	if _, err := c.Revision(ctx, "web", "never"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("uncached Revision: %v", err)
	}
}

func TestCacheRefusesWrongContent(t *testing.T) {
	c, _, _ := openCache(t, 10)
	src := &source{}
	r := src.revision("web", "r1", map[string]string{"compose.yaml": "real"})
	err := c.Add(t.Context(), r, func(string) ([]byte, error) { return []byte("tampered"), nil })
	if err == nil || len(c.Missing(r)) != 1 || c.blobs.Has(blob.Hash([]byte("tampered"))) {
		t.Fatalf("Add with the wrong content: %v", err)
	}
}

// The cache holds ciphertext only: the plaintext a Sync writes into the work dir never reaches the state dir.
func TestCacheHoldsNoPlaintext(t *testing.T) {
	c, _, state := openCache(t, 10)
	ctx := t.Context()
	src := &source{}
	const ciphertext = "DB_PASSWORD=ENC[AES256_GCM,data:Zm9vYmFy,iv:abc,tag:def,type:str]\n"
	const plaintext = "plaintext-db-password-in-the-work-dir-only"
	r := src.revision("web", "r1", map[string]string{"compose.yaml": "services: {}\n", ".sops.env": ciphertext})
	if err := c.Add(ctx, r, src.fetch); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "web")
	if err := workdir.Write(work, r.Files, c.Blob); err != nil {
		t.Fatal(err)
	}
	// what decrypting does (M2-05): the plaintext next to the ciphertext, in the work dir
	os.WriteFile(filepath.Join(work, ".env"), []byte("DB_PASSWORD="+plaintext+"\n"), 0o600)
	if err := workdir.Write(work, r.Files, c.Blob); err != nil { // a later Sync of the same Revision
		t.Fatal(err)
	}
	c.Add(ctx, r, src.fetch)

	found := false
	filepath.WalkDir(state, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), plaintext) {
			t.Errorf("plaintext in %s", p)
		}
		found = found || strings.Contains(string(b), "ENC[AES256_GCM")
		return nil
	})
	if !found {
		t.Fatal("the ciphertext isn't in the cache either: the scan saw nothing")
	}
}
