// Package buildinfo reports the version of the running cishift binary.
package buildinfo

import "runtime/debug"

// version is set by release builds:
//
//	-ldflags "-X github.com/viniciusdc/ci-shift/internal/buildinfo.version=v1.2.3"
var version = ""

// Version returns the release version, the module version recorded by
// `go install ...@vX.Y.Z`, or "dev" for local builds.
func Version() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
