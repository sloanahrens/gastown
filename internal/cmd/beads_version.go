// Package cmd provides CLI commands for the gt tool.
package cmd

import (
	"sync"

	"github.com/steveyegge/gastown/internal/deps"
)

var (
	cachedVersionCheckResult error
	versionCheckOnce         sync.Once
)

// CheckBeadsVersion verifies that bd is on PATH and reports a version.
// Returns nil if so, or an error with the install hint if not.
// The check is performed only once per process execution.
func CheckBeadsVersion() error {
	versionCheckOnce.Do(func() {
		// Presence and a readable version only: a cheap probe that must not
		// block read-only commands. The schema/contract handshake gates the
		// town-running commands (requireBDHandshake).
		cachedVersionCheckResult = deps.EnsureBeads()
	})
	return cachedVersionCheckResult
}
