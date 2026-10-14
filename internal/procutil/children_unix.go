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

// zombieChildrenOf parses `ps -axo pid=,ppid=,stat=` output into the pids of
// parent's children in state Z, leaving out skip (the ps that printed the table).
func zombieChildrenOf(table string, parent, skip int) []int {
	var pids []int
	for line := range strings.SplitSeq(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || ppid != parent || pid == skip || !strings.HasPrefix(f[2], "Z") {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// ReapZombieChildren waits on every child of this process that is already
// defunct and returns how many it reaped. A dashboard restarted in place
// inherits the children of the image it replaced; one whose owner is gone (a
// poll stranded past the kill bound) stays defunct until the process exits.
// It waits on each zombie's own pid with WNOHANG and never on -1: a live child
// belongs to the os/exec Wait that started it, and a zombie listed here but
// claimed by such a Wait first just fails the wait4 and is not counted. A
// failed ps reaps nothing.
func ReapZombieChildren() int {
	ctx, cancel := context.WithTimeout(context.Background(), childrenTimeout)
	defer cancel()
	ps := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,stat=")
	out, err := ps.Output() // Output waits, so this ps is reaped before the loop
	if err != nil {
		return 0
	}
	n := 0
	for _, pid := range zombieChildrenOf(string(out), os.Getpid(), ps.Process.Pid) {
		var ws syscall.WaitStatus
		if got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); err == nil && got == pid {
			n++
		}
	}
	return n
}
