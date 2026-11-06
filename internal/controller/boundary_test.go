package controller

import (
	"go/build"
	"strings"
	"testing"
)

// The Control Plane gets Desired State only through domain.Source (M1-03): it reads no Git, renders nothing and
// parses no App of Apps definitions itself.
func TestDesiredStateOnlyThroughSource(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		for _, bad := range []string{"github.com/go-git/", "github.com/obleey/omegacd/internal/source", "github.com/obleey/omegacd/internal/appdef"} {
			if strings.HasPrefix(imp, bad) {
				t.Errorf("the controller imports %s", imp)
			}
		}
	}
	if len(pkg.Imports) == 0 {
		t.Fatal("found no imports")
	}
}
