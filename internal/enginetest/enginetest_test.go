package enginetest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fatalTB records Fatal instead of failing the test, ending its goroutine as testing's does.
type fatalTB struct {
	testing.TB
	msg string
}

func (f *fatalTB) Fatal(args ...any) { f.msg = fmt.Sprint(args...); runtime.Goexit() }

// TempDir: t.TempDir without OMEGACD_IT_ROOT; a traversable folder under it with; a root that can't hold one fails the
// test, saying why.
func TestTempDir(t *testing.T) {
	t.Setenv("OMEGACD_IT_ROOT", "")
	if dir := TempDir(t); !strings.HasPrefix(dir, os.TempDir()) {
		t.Errorf("without OMEGACD_IT_ROOT: %s", dir)
	}

	root := t.TempDir()
	t.Setenv("OMEGACD_IT_ROOT", root)
	dir := TempDir(t)
	if fi, err := os.Stat(dir); err != nil || filepath.Dir(dir) != root || fi.Mode().Perm() != 0o711 {
		t.Errorf("under OMEGACD_IT_ROOT: %s %v %v", dir, fi, err)
	}

	t.Setenv("OMEGACD_IT_ROOT", filepath.Join(root, "missing"))
	f := &fatalTB{TB: t}
	done := make(chan struct{})
	go func() { defer close(done); TempDir(f) }()
	<-done
	if !strings.Contains(f.msg, "missing") {
		t.Errorf("a root that isn't there: %q", f.msg)
	}
}
