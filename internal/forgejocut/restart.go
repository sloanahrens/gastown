package forgejocut

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/land"
)

// errRestartRunnerUnwired is step 7's refusal of a cutover whose exec seam is not
// wired: with no runner the step could only skip the restart, which reads to the
// operator as a daemon that already read the cut-over block.
var errRestartRunnerUnwired = errors.New("the exec seam is not wired: no runner for the daemon restart")

// RestartOutcome is what step 7 did with the daemon restart. A skipped restart
// and one that ran both leave the cutover whole, so both return a nil error; this
// is how a caller tells them apart.
type RestartOutcome int

const (
	// RestartNone is the zero value and what an error returns: no restart ran. A
	// zero or unknown value never reads as a restart that happened.
	RestartNone RestartOutcome = iota
	// RestartRan: the landing queue was empty and `gt daemon restart` ran.
	RestartRan
	// RestartSkippedQueue: the queue held at least one landing, so the restart
	// is left to the operator.
	RestartSkippedQueue
	// RestartSkippedUnreadable: the queue could not be read, so the restart is
	// left to the operator.
	RestartSkippedUnreadable
	// RestartSkippedDryRun: a dry run printed the command and ran nothing.
	RestartSkippedDryRun
)

// String says what the outcome means in one phrase, for the run's summary line.
func (o RestartOutcome) String() string {
	switch o {
	case RestartRan:
		return "the daemon was restarted"
	case RestartSkippedQueue:
		return "the daemon was not restarted: a landing is queued"
	case RestartSkippedUnreadable:
		return "the daemon was not restarted: the landing queue could not be read"
	case RestartSkippedDryRun:
		return "the daemon was not restarted: dry run"
	default:
		return "the daemon was not restarted"
	}
}

// RestartDaemonIfQuiet is step 7. The daemon reads a rig's merge_queue.forgejo
// block at start, so the cutover restarts it — but never over a landing in
// flight: it re-reads the landing queue and runs `gt daemon restart` only when the
// queue is empty. A queue that holds a bead, or that cannot be read, skips the
// restart and logs the reason and the command to run once the queue is empty; the
// rig is cut over by now, so either costs the restart, not the cutover. A dry run
// prints the command and runs nothing. The outcome says which of these happened;
// on an error it is RestartNone, since the error says what failed.
//
// The queue read and the restart are not one atomic step: a landing worker can
// claim a bead between them, and the restart then ends that landing where it
// stands — its bead stays open and ready-to-land, so the next daemon pass lands
// it. Closing the gap needs a lock the cutover and the landing worker both take,
// which the cutover has no seam for, so the gap is one exec of `gt daemon
// restart` wide and is stated here rather than closed.
func (c *Cutover) RestartDaemonIfQuiet() (RestartOutcome, error) {
	o := c.Opts
	if c.Runner == nil {
		return RestartNone, errRestartRunnerUnwired
	}
	n, err := LandingCount(c.Runner, o.RigRoot)
	switch {
	case err != nil:
		c.logf("could not read %s's landing queue (%v); NOT restarting the daemon. When the queue is empty run: %s daemon restart",
			o.Rig, err, o.GT)
		return RestartSkippedUnreadable, nil
	case n != 0:
		c.logf("%s has %d open %s bead(s); NOT restarting the daemon. When the queue is empty run: %s daemon restart",
			o.Rig, n, land.LabelReadyToLand, o.GT)
		return RestartSkippedQueue, nil
	}
	argv := restartArgv(o)
	c.logf("+ %s", commandText(argv))
	if o.DryRun {
		c.logf("(dry run: the daemon was not restarted)")
		return RestartSkippedDryRun, nil
	}
	if err := c.Runner.Run(argv); err != nil {
		return RestartNone, fmt.Errorf("the daemon restart of %s failed: %w", o.Rig, err)
	}
	return RestartRan, nil
}

// OperatorSteps is step 8: what no part of the cutover can do for the operator,
// in the script's words — the text the script sent to the operator's stdout. The
// mirror branch's line went with the dropped push mirror, so only the promote
// branch's text remains here.
func (c *Cutover) OperatorSteps() []string {
	slug := c.Opts.GitHubSlug
	return []string{
		"Finish the cutover by hand:",
		"  - add the promote deploy key above, then confirm the rig's next green main verdict advances GitHub main",
		"  - disable GitHub Actions on " + slug + ":",
		"      gh api --method PUT repos/" + slug + "/actions/permissions -F enabled=false",
		"  - stop (not remove) any self-hosted GitHub runner for " + slug,
	}
}

// restartArgv is step 7's command line, word for word the script's: the town root
// twice in the environment, so the restarted daemon reads the same town, and the
// gt binary --gt names.
func restartArgv(o *Options) []string {
	return []string{"env", "GT_TOWN_ROOT=" + o.TownRoot, "GT_ROOT=" + o.TownRoot, o.GT, "daemon", "restart"}
}
