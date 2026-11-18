package version

import "testing"

// Q41, Q203: another part of OmegaCD fits a Control Plane of the same minor version or one behind; development builds
// always do.
func TestCompatible(t *testing.T) {
	for _, tc := range []struct {
		cp, v string
		want  bool
	}{
		{"v0.3.1", "v0.3.0", true}, {"v0.3.0", "v0.2.9", true}, {"v0.3.0", "v0.1.0", false}, {"v0.3.0", "v0.4.0", false},
		{"v1.0.0", "v0.9.0", false}, {"(devel)", "v0.1.0", true}, {"v0.3.0", "(devel)", true},
	} {
		if got := Compatible(tc.cp, tc.v); got != tc.want {
			t.Errorf("Compatible(%s, %s) = %v", tc.cp, tc.v, got)
		}
	}
}
