//go:build !windows

package tmux

import (
	"context"
	"strings"
	"syscall"
	"time"
)

func killProcessGroup(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// getParentPID returns the parent process ID (PPID) for a given PID.
// Returns empty string if the process doesn't exist or PPID can't be determined.
func getParentPID(ex execFunc, pid string) string {
	out, _, err := ex(context.Background(), "ps", "-o", "ppid=", "-p", pid)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getProcessGroupID returns the process group ID (PGID) for a given PID.
// Returns empty string if the process doesn't exist or PGID can't be determined.
func getProcessGroupID(ex execFunc, pid string) string {
	out, _, err := ex(context.Background(), "ps", "-o", "pgid=", "-p", pid)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getProcessGroupMembers returns all PIDs in a process group.
// This finds processes that share the same PGID, including those that reparented to init.
func getProcessGroupMembers(ex execFunc, pgid string) []string {
	// Use ps to find all processes with this PGID
	// On macOS: ps -axo pid,pgid
	// On Linux: ps -eo pid,pgid
	out, _, err := ex(context.Background(), "ps", "-axo", "pid,pgid")
	if err != nil {
		return nil
	}

	var members []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimSpace(fields[1]) == pgid {
			members = append(members, strings.TrimSpace(fields[0]))
		}
	}
	return members
}
