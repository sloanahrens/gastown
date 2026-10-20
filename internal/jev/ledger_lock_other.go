//go:build !unix

package jev

import "os"

// lockLedger takes no lock where flock is missing, so concurrent writers rely
// on O_APPEND alone.
func lockLedger(*os.File) (func(), error) { return func() {}, nil }
