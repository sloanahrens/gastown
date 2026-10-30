//go:build !windows

package sweeprunner

import "syscall"

// oNoFollow refuses to open a result through a symlink, and oNonBlock keeps a
// FIFO swapped in at the name from blocking the open until a writer appears.
// Both are open(2) flags; a regular file ignores O_NONBLOCK, so they cost a
// result read nothing (gt-u00wo).
const (
	oNoFollow = syscall.O_NOFOLLOW
	oNonBlock = syscall.O_NONBLOCK
)
