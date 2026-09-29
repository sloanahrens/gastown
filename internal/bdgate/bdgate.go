// Package bdgate lets the gt binary refuse to start agent sessions unless the
// bd startup handshake passed (gt-7iwy0.1). The session-starting code calls
// Require; only the gt entry point installs the check, so library callers
// and package tests run with no gate.
package bdgate

import "sync/atomic"

var check atomic.Pointer[func() error]

// Set installs the check Require runs; nil removes it.
func Set(fn func() error) {
	if fn == nil {
		check.Store(nil)
		return
	}
	check.Store(&fn)
}

// Require returns the installed check's verdict, or nil when none is set.
func Require() error {
	if fn := check.Load(); fn != nil {
		return (*fn)()
	}
	return nil
}
