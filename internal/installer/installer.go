// Package installer is scripts/install-gt.sh in Go (gt-tpg0p, decision Q3).
//
// The script's sections move here one bead at a time; what is here now is the
// exit contract they all report through and the install lock the swap takes
// (gt-tpg0p.13). A run names one result in its last line (Result) and ends with
// one of the four statuses its callers map (Exit). A run that ends without
// naming a result is reported as failed/unexpected by OnExit, the counterpart
// of the script's EXIT trap.
package installer

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Exit is a run's status. plugins/rebuild-gt maps these, so a change here is a
// change to that caller too.
type Exit int

const (
	// ExitOK is installed, or nothing to do: the binary already holds the commit.
	ExitOK Exit = 0
	// ExitFailed is a failed install: build, install or smoke failed, or the run
	// ended on a path that named no result.
	ExitFailed Exit = 1
	// ExitRefused is a refused target: the checkout is dirty, off main, diverged
	// or not forward, or the commit is unknown.
	ExitRefused Exit = 2
	// ExitBusy is a wait that ran out: the install lock or the gate slot was not
	// free in time.
	ExitBusy Exit = 3
)

// Result is the one machine-readable line a run ends with,
// `install-gt: RESULT <event> <commit> <prev> <reason>`.
type Result struct {
	// Event is what became of the run: installed, noop, failed, refused, ...
	Event string
	// Commit is the commit the event is about.
	Commit string
	// Prev is the commit it replaced.
	Prev string
	// Reason is the code for why: build-failed, lock-busy, unexpected, ...
	Reason string
}

// Line renders r as the script's igt_result does: four fields, "-" for one it
// has nothing to say about, which is how the daemon reads it as absent.
func (r Result) Line() string {
	return fmt.Sprintf("install-gt: RESULT %s %s %s %s",
		orDash(r.Event), orDash(r.Commit), orDash(r.Prev), orDash(r.Reason))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Log writes one [install-gt] line, the script's log().
func (c *Contract) Log(parts ...string) {
	fmt.Fprintln(c.stdout(), "[install-gt] "+strings.Join(parts, " "))
}

// Report writes r as the run's result line and marks the contract met.
func (c *Contract) Report(r Result) {
	fmt.Fprintln(c.stdout(), r.Line())
	c.reported = true
}

// Finish reports r and returns code: the script's `result_line ...; exit N`.
func (c *Contract) Finish(r Result, code Exit) Exit {
	c.Report(r)
	return code
}

func (c *Contract) stdout() io.Writer {
	if c.Stdout == nil {
		return os.Stdout
	}
	return c.Stdout
}

func (c *Contract) stderr() io.Writer {
	if c.Stderr == nil {
		return os.Stderr
	}
	return c.Stderr
}
