// Package version carries build metadata set with -ldflags.
package version

// Set at build time: -ldflags "-X .../internal/version.Version=v0.1.0 -X .../internal/version.Commit=abc".
var (
	Version = "dev"
	Commit  = "unknown"
)

// UserAgent is sent to LXD.
func UserAgent() string { return "type3/" + Version }
