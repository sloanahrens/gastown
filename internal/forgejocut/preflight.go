package forgejocut

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
)

// Runner is the exec seam the run's child processes go through. argv[0] is the
// program. A test injects a fake; the command injects the real exec.
type Runner interface {
	// Output runs argv and returns its stdout: the answer a read needs.
	Output(argv []string) ([]byte, error)
	// Run runs argv with the run's own stdout and stderr, so the probe's log
	// reaches the operator.
	Run(argv []string) error
}

// countPattern is what the bd count prints: digits and nothing else.
var countPattern = regexp.MustCompile(`^[0-9]+$`)

// LandingCount returns how many open beads in the rig at rigRoot wear the
// ready-to-land label: the queue the landing worker drains. A failing read, or an
// answer that is not a plain count, is an error that names the command. It is
// never zero, so "I could not tell" cannot read as "nothing is in flight".
func LandingCount(run Runner, rigRoot string) (int, error) {
	argv := []string{"bd", "-C", rigRoot, "count", "--status", "open", "--label", land.LabelReadyToLand}
	out, err := run.Output(argv)
	if err != nil {
		return 0, fmt.Errorf("%s failed: %w", commandText(argv), err)
	}
	// Command substitution strips trailing newlines and nothing else, so a count
	// with a space or a sign around it is refused.
	text := strings.TrimRight(string(out), "\n")
	if !countPattern.MatchString(text) {
		return 0, fmt.Errorf("%s printed no plain count", commandText(argv))
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%s printed a count that does not fit: %w", commandText(argv), err)
	}
	return n, nil
}

// RefuseIfLandingInFlight is step 1's first half. It refuses the cutover while
// the rig has an open ready-to-land bead, since that bead is a landing queued or
// in flight; an empty queue is logged and passes.
func RefuseIfLandingInFlight(run Runner, o *Options, log func(string)) error {
	n, err := LandingCount(run, o.RigRoot)
	if err != nil {
		return fmt.Errorf("could not read the landing queue of %s: %w", o.Rig, err)
	}
	if n != 0 {
		return fmt.Errorf("%s has %d open %s bead(s): land them (or wait) before cutting the rig over",
			o.Rig, n, land.LabelReadyToLand)
	}
	log(o.Rig + "'s landing queue is empty")
	return nil
}

// ProbeGreen is step 1's second half. The probe pushes the rig's main through its
// real runner and waits for the gate, so a green run is the evidence the landing
// path works. Under a dry run the probe is printed and not run.
//
// The probe carries --town-root so it pushes from a worktree of the rig's bare
// repository and the rig's hooks run. Its push rides the admin git URL, because it
// happens before provisioning, when the bots have no access yet. The admin token
// is handed over as a file path only, never as an argv word.
func ProbeGreen(run Runner, o *Options, log func(string)) error {
	argv := []string{"bash", o.Probe, o.Rig,
		"--repo", o.Repo,
		"--main-branch", o.MainBranch,
		"--town-root", o.TownRoot,
		"--api-url", o.APIURL,
		"--web-url", o.WebURL,
	}
	if o.AdminTokenFile != "" {
		argv = append(argv, "--admin-token-file", o.AdminTokenFile)
	}
	argv = append(argv,
		"--github-url", o.GitHubURL,
		"--git-url", o.AdminGitURL,
	)
	if o.DryRun {
		log("+ " + commandText(argv))
		log("(dry run: the probe was not run)")
		return nil
	}
	log("probing " + o.Rig + " through its real runner before the cutover")
	if err := run.Run(argv); err != nil {
		// The probe's own output already reached the operator, and err is only the
		// exit status, so the message does not repeat either.
		return fmt.Errorf("the probe of %s is not green; fix the runner or the gate workflow and re-run", o.Rig)
	}
	return nil
}

// commandText is argv as one line, for an error or a log line.
func commandText(argv []string) string {
	return strings.Join(argv, " ")
}
