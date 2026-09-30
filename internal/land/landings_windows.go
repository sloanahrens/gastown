//go:build windows

package land

import "os"

// oNoFollow has no open(2) counterpart on Windows; the landing worker does
// not run there.
const oNoFollow = 0

// checkOwner has no uid to compare on Windows.
func checkOwner(string, os.FileInfo) error { return nil }
