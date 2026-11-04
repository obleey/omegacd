// Package version reports the OmegaCD version: the Git tag stamped by release builds,
// falling back to what the Go toolchain recorded from Git, or "(devel)".
package version

import (
	"cmp"
	"runtime/debug"
)

// Version is set at build time: -ldflags "-X github.com/obleey/omegacd/internal/version.Version=v1.2.3".
var Version string

func String() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		return cmp.Or(bi.Main.Version, "(devel)")
	}
	return "(devel)"
}
