// Package version is the single source of truth for the release version
// shown by `xboard-node -v` and `xbnode version`.
//
// Bump Version on each maintenance release (v1.0.1 -> v1.0.2 -> ...).
// It is deliberately a plain constant, not a commit hash injected at
// build time, so the displayed version stays short and stable across
// dev builds until the next release bump.
package version

// Version is the current release version.
const Version = "v1.0.1"
