// Package version provides build version information shared by all binaries.
package version

// Version is replaced at build time for release builds.
var Version = "dev"

// String returns the current build version.
func String() string {
	return Version
}
