package installer

import (
	"io"
	"os/exec"
	"time"
)

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
	run := c.Exec
	if run == nil {
		run = func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		}
	}
	// gt's own words about a refused escalation go nowhere, as the script
	// discards them: the WARNING line is the notice.
	_, err := run(c.GT, "escalate", "install-gt: "+message, "-s", severity,
		"--source", "script:install-gt", "--fingerprint", fingerprint)
	if err != nil {
		c.Log("WARNING: escalation " + fingerprint + " did not reach the town")
	}
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
