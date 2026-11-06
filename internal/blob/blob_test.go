package blob

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPutGet(t *testing.T) {
	s := open(t)
	h, err := s.Put([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if h != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("hash %s", h)
	}
	got, err := s.Get(h)
	if err != nil || string(got) != "hello" || !s.Has(h) {
		t.Fatalf("Get = %q, %v; Has = %v", got, err, s.Has(h))
	}
	missing := Hash([]byte("other"))
	if _, err := s.Get(missing); !errors.Is(err, fs.ErrNotExist) || s.Has(missing) {
		t.Fatalf("missing blob: %v", err)
	}
	for _, bad := range []string{"../../etc/passwd", h[:63], "2CF24DBA5FB0A30E26E83B2AC5B9E29E1B161E5C1FA7425E73043362938B9824"} {
		if _, err := s.Get(bad); err == nil || s.Has(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConcurrentPuts(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			if _, err := s.Put([]byte("same")); err != nil {
				t.Error(err)
			}
			if _, err := s.Put(fmt.Appendf(nil, "blob %d", i)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i := range 50 {
		if _, err := s.Get(Hash(fmt.Appendf(nil, "blob %d", i))); err != nil {
			t.Error(err)
		}
	}
	if got, err := s.Get(Hash([]byte("same"))); err != nil || string(got) != "same" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if tmp, _ := os.ReadDir(filepath.Join(s.dir, "tmp")); len(tmp) != 0 {
		t.Fatalf("%d files left in tmp", len(tmp))
	}
}

func TestCorruption(t *testing.T) {
	s := open(t)
	h, _ := s.Put([]byte("original"))
	if err := os.WriteFile(s.path(h), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(h); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get = %v, want ErrCorrupt", err)
	}
}

func TestKilledWrite(t *testing.T) {
	s := open(t)
	h := Hash([]byte("complete content"))
	// what a write killed before its rename leaves behind
	half := filepath.Join(s.dir, "tmp", h+".123")
	if err := os.WriteFile(half, []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Has(h) {
		t.Fatal("a half-written blob is visible under its hash")
	}
	if _, err := s.Put([]byte("complete content")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(h); err != nil || string(got) != "complete content" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	age(t, half)
	if _, err := s.GC(func(string) bool { return true }, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(half); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("GC left the half-written file")
	}
}

func TestGC(t *testing.T) {
	s := open(t)
	kept, _ := s.Put([]byte("referenced"))
	gone, _ := s.Put([]byte("unreferenced"))
	fresh, _ := s.Put([]byte("just put"))
	retouched, _ := s.Put([]byte("put again"))
	for _, h := range []string{kept, gone, retouched} {
		age(t, s.path(h))
	}
	if _, err := s.Put([]byte("put again")); err != nil { // renews its grace period
		t.Fatal(err)
	}
	n, err := s.GC(func(h string) bool { return h == kept }, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("GC = %d, %v; want 1 removed", n, err)
	}
	for h, want := range map[string]bool{kept: true, gone: false, fresh: true, retouched: true} {
		if s.Has(h) != want {
			t.Errorf("Has(%s) = %v, want %v", h[:8], !want, want)
		}
	}
}

func age(t *testing.T, path string) {
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
