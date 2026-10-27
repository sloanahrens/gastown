//go:build !linux

package polecatimage

import (
	"fmt"
	"os/exec"
)

// seedAs refuses a child that starts as another user: dropping a child's credentials needs linux,
// and the seed Job's image is linux, so this platform never runs the seed pass.
func seedAs(cmd *exec.Cmd, uid, gid int) error {
	return fmt.Errorf("running a seed child as uid %d needs linux", uid)
}
