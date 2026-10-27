//go:build linux

package polecatimage

import (
	"os/exec"
	"syscall"
)

// seedAs runs cmd as uid and gid. The seed Job's container is root so it can hand the claim over,
// and the pinned rigs' builds run as the cache user, so a pin's code never compiles as root
// (gt-dv04y).
func seedAs(cmd *exec.Cmd, uid, gid int) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	return nil
}
