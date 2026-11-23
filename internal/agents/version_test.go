package agents

import (
	"strings"
	"testing"
)

func TestCompatible(t *testing.T) {
	for _, tc := range []struct{ cp, agent, refused string }{
		{"v0.3.0", "v0.3.0", ""},
		{"v0.3.2", "v0.3.0-rc.1", ""},     // same minor
		{"v0.3.0", "v0.2.9", ""},          // minus one
		{"v0.3.0", "v0.1.4", "too old"},   // minus two
		{"v1.0.0", "v0.9.0", "too old"},   // another major
		{"v0.3.0", "v0.4.0", "newer"},     // the Control Plane first
		{"v0.0.0", "v0.0.0-2026-dev", ""}, // minus one can't go below 0
		{"(devel)", "v0.1.0", ""},         // development builds
		{"v0.3.0", "(devel)", ""},
	} {
		err := compatible(tc.cp, tc.agent)
		if (err == nil) != (tc.refused == "") || (err != nil && !strings.Contains(err.Error(), tc.refused)) {
			t.Errorf("compatible(%s, %s) = %v, want %q", tc.cp, tc.agent, err, tc.refused)
		}
	}
	for _, tc := range []struct {
		cp, agent string
		want      bool
	}{
		{"v0.3.1", "v0.3.0", true},
		{"v0.3.0", "v0.2.9", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.3.0", "v0.4.0", false},
		{"v0.3.0", "v0.2.9-rc.1", false}, // development builds and release candidates never
		{"v0.3.0-0.2026-abc", "v0.2.0", false},
		{"v0.3.0", "v0.2.0+dirty", false},
		{"(devel)", "v0.2.0", false},
	} {
		if got := outdated(tc.cp, tc.agent); got != tc.want {
			t.Errorf("outdated(%s, %s) = %v", tc.cp, tc.agent, got)
		}
	}
	if err := compatible("v0.3.0", "v0.1.4"); !strings.Contains(err.Error(), "serves Agents from v0.2 on: upgrade the Agent") {
		t.Errorf("doesn't say what to upgrade to: %v", err)
	}
}

// Q322: from v0.2 on an Agent imports; one older than its Control Plane and than v0.2 is told to upgrade.
func TestCanImport(t *testing.T) {
	for _, tc := range []struct {
		cp, agent string
		ok        bool
	}{
		{"v0.2.0", "v0.1.3", false},
		{"v0.2.0", "v0.2.0", true},
		{"v0.3.0", "v0.2.4", true},
		{"v0.1.0-45-gabc", "v0.1.0-45-gabc", true}, // a build of the tree before v0.2 imports with its own Agent
		{"v0.2.0", "(devel)", true},
		{"(devel)", "v0.1.0", true},
	} {
		if err := canImport(tc.cp, tc.agent); (err == nil) != tc.ok {
			t.Errorf("canImport(%s, %s) = %v", tc.cp, tc.agent, err)
		}
	}
}
