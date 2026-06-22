// Package version carries build metadata, overridable at build time with
// -ldflags "-X .../version.Version=... -X .../version.Commit=...".
package version

var (
	// Version is the release or "dev" for local builds.
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "none"
)

// String renders the version and commit together.
func String() string { return Version + " (" + Commit + ")" }
