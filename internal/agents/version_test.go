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
	if err := compatible("v0.3.0", "v0.1.4"); !strings.Contains(err.Error(), "serves Agents from v0.2 on: upgrade the Agent") {
		t.Errorf("doesn't say what to upgrade to: %v", err)
	}
}
