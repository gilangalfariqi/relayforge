// Package version carries build identity injected at build time.
package version

// These values are overridden with -ldflags at build time.
var (
	// Version is the release version, for example "0.1.0".
	Version = "dev"
	// Commit is the git commit SHA the binary was built from.
	Commit = "unknown"
	// BuildTime is the RFC 3339 build timestamp.
	BuildTime = "unknown"
)
