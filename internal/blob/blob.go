// Package blob is a content-addressed store on disk: files kept by their sha256 (Q27, Q31). The Control Plane keeps
// every payload it delivered here; an Agent keeps its cache of recent Revisions.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrCorrupt is returned by Get when a blob's content no longer matches its hash.
var ErrCorrupt = errors.New("blob corrupt")

// Store keeps blobs under dir as <first two hex digits>/<hash>, and writes in progress under dir/tmp.
type Store struct {
	dir string
	// GC holds it exclusively, Puts shared: a Put that touches an old blob must not lose it to a GC already running
	mu sync.RWMutex
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Hash is the hex sha256 that names data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Put stores data and returns its hash. Concurrent Puts of the same data are safe.
func (s *Store) Put(data []byte) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := Hash(data)
	p := s.path(h)
	// already there: touch it, so a blob about to be referenced again outlives GC's grace period
	if err := os.Chtimes(p, time.Now(), time.Now()); err == nil {
		return h, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	// write elsewhere, sync, then rename: a killed write never leaves a half file under its hash
	f, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), h+".*")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil { // coverage: unreachable, writing, syncing and renaming a new file in the store's own folders fails only with a broken disk
		os.Remove(f.Name())
		return "", err
	}
	return h, nil
}

// Get returns the blob named hash, fs.ErrNotExist if there is none, or ErrCorrupt if its content changed.
func (s *Store) Get(hash string) ([]byte, error) {
	if !valid(hash) {
		return nil, fmt.Errorf("blob %q: not a sha256", hash)
	}
	data, err := os.ReadFile(s.path(hash))
	if err != nil {
		return nil, err
	}
	if Hash(data) != hash {
		return nil, fmt.Errorf("blob %s: %w", hash, ErrCorrupt)
	}
	return data, nil
}

func (s *Store) Has(hash string) bool {
	if !valid(hash) {
		return false
	}
	_, err := os.Stat(s.path(hash))
	return err == nil
}

// GC removes every blob that referenced says no to and that is older than grace, so a blob just Put but not yet
// recorded as referenced survives; leftovers of killed writes older than grace go too. It returns how many blobs
// it removed.
func (s *Store) GC(referenced func(hash string) bool, grace time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-grace)
	removed := 0
	err := filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) { // coverage: unreachable, only an entry deleted between listing and stat, and GC holds the store's lock
			return nil
		}
		if err != nil { // coverage: unreachable, stat of an entry just listed in the store's own folder
			return err
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		inTmp := filepath.Base(filepath.Dir(p)) == "tmp"
		if !inTmp && (!valid(d.Name()) || referenced(d.Name())) {
			return nil
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if !inTmp {
			removed++
		}
		return nil
	})
	return removed, err
}

func (s *Store) path(hash string) string { return filepath.Join(s.dir, hash[:2], hash) }

// valid guards paths built from hashes that come over the network.
func valid(hash string) bool {
	b, err := hex.DecodeString(hash)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == hash // lowercase only
}
