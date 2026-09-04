// Package version holds the build version string, injected at link time by
// the Makefile (-X cs2a/internal/version.Version=...).
package version

// Version is overridden at build time with -ldflags "-X ...". Falls back to
// "dev" for untagged local builds.
var Version = "dev"

// UserAgent is used by the agent when talking to external services.
func UserAgent() string {
	return "cs2a/" + Version
}
