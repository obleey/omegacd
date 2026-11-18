// Package version reports the OmegaCD version: the Git tag stamped by release builds,
// falling back to what the Go toolchain recorded from Git, or "(devel)".
package version

import (
	"cmp"
	"runtime/debug"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// Version is set at build time: -ldflags "-X github.com/obleey/omegacd/internal/version.Version=v1.2.3".
var Version string

func String() string {
	if Version != "" {
		return Version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok { // coverage: unreachable, every binary and test built with modules carries its build info
		return "(devel)"
	}
	return cmp.Or(bi.Main.Version, "(devel)")
}

// Compatible is whether another part of OmegaCD at version v works with a Control Plane at cp (Q41, Q203): the same
// minor version or one behind. Development builds always are.
func Compatible(cp, v string) bool {
	if !semver.IsValid(cp) || !semver.IsValid(v) {
		return true
	}
	cmaj, cmin := majorMinor(cp)
	maj, min := majorMinor(v)
	return maj == cmaj && (min == cmin || min == cmin-1)
}

func majorMinor(v string) (int, int) {
	major, minor, _ := strings.Cut(strings.TrimPrefix(semver.MajorMinor(v), "v"), ".")
	a, _ := strconv.Atoi(major)
	b, _ := strconv.Atoi(minor)
	return a, b
}
