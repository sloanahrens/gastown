// Package procid names one process for good: a pid plus the kernel's start
// token for it. A pid alone is not an identity. Once its process dies the
// number is handed to the next process, so a pid file read later can make a
// dead process look alive, or point a SIGTERM at a stranger. The start token
// does not repeat for a reused pid, so the pair does not have that problem.
//
// Anything that records a process to find or signal it later (a poller's pid
// file, a session's tracked pane pid, a container's owner label) records an
// ID, and acts on the process only when ID.Running says the same process is
// still there.
package procid

import (
	"fmt"
	"strconv"
	"strings"
)

// ID is one process: its pid and the start token StartToken read for it.
type ID struct {
	PID   int
	Start string
}

// Of returns the ID of the live process pid; ok is false when the pid has no
// process or its start token cannot be read.
func Of(pid int) (ID, bool) {
	return of(pid, StartToken)
}

func of(pid int, start func(int) (string, bool)) (ID, bool) {
	token, ok := start(pid)
	if !ok || token == "" {
		return ID{}, false
	}
	return ID{PID: pid, Start: token}, true
}

// Running reports whether the process id names is still running, read
// through start (StartToken outside tests). It is true only when the pid's
// current start token equals the recorded one: a dead pid, a reused pid, a
// token the kernel will not give, and an ID with no recorded token all read
// as not running, so a caller never signals a process it cannot prove is its
// own.
func (id ID) Running(start func(int) (string, bool)) bool {
	if id.PID <= 0 || id.Start == "" {
		return false
	}
	token, ok := start(id.PID)
	return ok && token == id.Start
}

// String is the on-disk form, "<pid>|<start>", which Parse reads back.
func (id ID) String() string {
	return strconv.Itoa(id.PID) + "|" + id.Start
}

// Parse reads the form String writes. A bare pid with no start token (the
// form older pid files hold) parses with an empty Start, which Running never
// accepts: such a record can be cleaned up but never acted on.
func Parse(s string) (ID, error) {
	s = strings.TrimSpace(s)
	pidPart, start, _ := strings.Cut(s, "|")
	pid, err := strconv.Atoi(pidPart)
	if err != nil {
		return ID{}, fmt.Errorf("parsing process id %q: %w", s, err)
	}
	if pid <= 0 {
		return ID{}, fmt.Errorf("parsing process id %q: pid must be positive", s)
	}
	return ID{PID: pid, Start: start}, nil
}
