//go:build !windows

package procutil

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// childrenTimeout bounds the ps read behind KillChildren.
const childrenTimeout = 5 * time.Second

// childrenOf parses `ps -axo pid=,ppid=` output into the pids whose parent is
// parent, leaving out skip (the ps that printed the table, itself a child).
func childrenOf(table string, parent, skip int) []int {
	var pids []int
	for line := range strings.SplitSeq(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || ppid != parent || pid == skip {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// KillChildren kills every child process of this one and returns how many it
// signaled. It is the last resort for a poll that outlived its drain bound: a
// child's owner is a goroutine blocked in Wait, which reaps the child as soon as
// it dies. Do not wait4(-1) for them here; that would take the exit status from
// the owner's Wait. A failed ps kills nothing.
func KillChildren() int {
	ctx, cancel := context.WithTimeout(context.Background(), childrenTimeout)
	defer cancel()
	ps := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=")
	out, err := ps.Output() // Output waits, so this ps is reaped before the kills
	if err != nil {
		return 0
	}
	n := 0
	for _, pid := range childrenOf(string(out), os.Getpid(), ps.Process.Pid) {
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			n++
		}
	}
	return n
}
