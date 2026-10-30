//go:build windows

package sweeprunner

// oNoFollow and oNonBlock have no open(2) counterpart on Windows. Read still
// refuses a result that is not a regular file, through the Lstat before the
// open; the runner itself runs in a Linux container, so the Windows build is a
// compile target only (gt-u00wo).
const (
	oNoFollow = 0
	oNonBlock = 0
)
