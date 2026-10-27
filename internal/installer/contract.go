package installer

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// escalateDeadline is how long one escalation may take: a town that does not
// answer must not hold the install open (gt-314j0). 60 s matches the daemon's
// own escalation budget, which allows the child to acquire a slot under load.
const escalateDeadline = 60 * time.Second

// escalateGrace is how long a killed escalation's output pipes get to close: a
// grandchild that inherited them can otherwise keep the wait alive (gt-314j0).
const escalateGrace = time.Second

// Contract is the exit contract of one run: the result line it names, the
// receipt it leaves, and the escalation it raises. The zero value writes to
// os.Stdout and os.Stderr and leaves no receipt until Receipts is set.
type Contract struct {
	// Stdout receives result lines and log lines.
	Stdout io.Writer
	// Stderr receives the receipt warning a run cannot append one.
	Stderr io.Writer
	// Receipts is the file receipts are appended to: <daemon>/install-receipts.jsonl.
	// Empty means the run does not know its daemon directory yet and writes
	// none, as the script's igt_receipt cannot on the paths before DAEMON_DIR
	// resolves.
	Receipts string
	// Source is the --source value, written to every receipt.
	Source string
	// Expected is the commit results and receipts name: EXPECTED once the target
	// is resolved, otherwise the SHA argument.
	Expected string
	// Prev is the commit being replaced.
	Prev string
	// MergedAt is the target commit's merge time, for receipts; zero is null.
	MergedAt time.Time
	// Start is when the run started, for the receipt's duration; zero is null.
	Start time.Time
	// GT is the gt binary escalations go through; the script passes its resolved
	// $GT.
	GT string
	// Exec runs the installer's external commands; nil runs them for real.
	Exec Exec
	// Now is the clock receipts use; nil means time.Now.
	Now func() time.Time

	// escalateDeadline bounds this contract's escalations; zero means
	// escalateDeadline.
	escalateDeadline time.Duration

	reported bool
}

// Exec runs one external command and returns its combined output; a caller
// that does not need the output drops it. It is the seam that keeps a unit test
// from running a process (gt-tpg0p.9).
type Exec func(name string, args ...string) ([]byte, error)

// Escalate reports a failure through GT under fingerprint, prefixed
// "install-gt: " as the script does. An escalation that cannot be delivered is
// one WARNING line, never a failed install: the run's outcome does not depend
// on the town hearing about it.
func (c *Contract) Escalate(severity, fingerprint, message string) {
	// gt's own words about a refused escalation go nowhere, as the script
	// discards them: the WARNING line is the notice.
	_, err := c.escalate("escalate", "install-gt: "+message, "-s", severity,
		"--source", "script:install-gt", "--fingerprint", fingerprint)
	if err != nil {
		c.Log("WARNING: escalation " + fingerprint + " did not reach the town")
	}
}

// escalate runs gt with args and returns its output, or fails once the
// deadline passes. The wait is abandoned at the deadline rather than trusting
// the child to exit: the same deadline kills gt, and the grace closes the
// output pipe a grandchild could otherwise hold (gt-314j0).
func (c *Contract) escalate(args ...string) ([]byte, error) {
	deadline := c.escalateTimeout()
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		defer close(done)
		if c.Exec != nil {
			out, err = c.Exec(c.GT, args...)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, c.GT, args...)
		cmd.WaitDelay = escalateGrace
		out, err = cmd.CombinedOutput()
	}()

	select {
	case <-done:
		return out, err
	case <-time.After(deadline):
		return nil, context.DeadlineExceeded
	}
}

// escalateTimeout is the deadline this contract gives one escalation.
func (c *Contract) escalateTimeout() time.Duration {
	if c.escalateDeadline > 0 {
		return c.escalateDeadline
	}
	return escalateDeadline
}

// exec runs one external command and returns its combined output, through the
// seam when one is set and for real when none is.
func (c *Contract) exec(name string, args ...string) ([]byte, error) {
	if c.Exec != nil {
		return c.Exec(name, args...)
	}
	return exec.Command(name, args...).CombinedOutput()
}

// Warn writes one [install-gt] WARNING line to stderr, where the script's
// explicit >&2 lines go; it never changes the run's status, so a caller uses it
// only for a failure the run outlives.
func (c *Contract) Warn(message string) {
	fmt.Fprintln(c.stderr(), "[install-gt] WARNING: "+message)
}

// OnExit is the script's EXIT trap: it returns code when the run named a
// result, and otherwise leaves the failed receipt (when the receipts path is
// known), reports failed/<Expected>/<Prev>/unexpected, and returns ExitFailed.
func (c *Contract) OnExit(code Exit) Exit {
	if c.reported {
		return code
	}
	c.Receipt("failed", c.Expected, c.Prev, "unexpected")
	c.Report(Result{Event: "failed", Commit: c.Expected, Prev: c.Prev, Reason: "unexpected"})
	return ExitFailed
}
