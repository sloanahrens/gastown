//go:build !windows

package land

import (
	"fmt"
	"os"
	"syscall"
)

// oNoFollow refuses to open the landings file through a symlink.
const oNoFollow = syscall.O_NOFOLLOW

// checkOwner refuses a landings directory another account owns.
func checkOwner(dir string, info os.FileInfo) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing landings dir %s: owned by uid %d, not %d", dir, st.Uid, os.Getuid())
	}
	return nil
}
