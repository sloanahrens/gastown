package tiersweep

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// shellPkg is the shell-tier runner the sweep runs as a child process. It runs
// every shell test in the manifest (gt-u560q.4).
const shellPkg = "./internal/shelltier/cmd/shelltier"

// shellNoSummary is the one failed item a shell tier counts when its runner
// printed output but no summary line. It has no space, so the daemon reads it
// as one name.
const shellNoSummary = "shelltier-command"

// shellSummaryRE matches the runner's summary line. Its per-script announce
// lines start the same way but end in " ..." and carry no verdict.
var shellSummaryRE = regexp.MustCompile(`^tier-sweep: shell (GREEN|RED) passed=`)

// ShellTailHeader starts each failed script's log tail, which the runner prints
// after its summary. A log holds arbitrary script output, so a summary-shaped
// line inside a tail is that script's text: the run's summary is always before
// the first tail header. It is exported for internal/shelltier, which prints
// the header, so the printer and this parser cannot drift apart. The constant
// lives here because internal/shelltier imports this package for the summary
// line; the reverse import would be a cycle.
const ShellTailHeader = "tier-sweep: shell log tail "

// Shell runs the shell tier through the shell-tier runner and returns the
// summary line the runner prints, unchanged, the lines the runner printed after
// that summary (a RED run's log tails), and whether the tier is RED. A RED
// summary line or a non-zero exit is RED, and a run that prints no summary line
// is RED too, never an empty line.
//
// The lines before the summary go to the sweep's stderr here, in the runner's
// order. The caller prints the summary before the returned tails, so the job
// log keeps the runner's order: announces, summary, tails.
//
// The shell tier has no skip list: the runner's command takes no skip option
// (its flags are --all, --root, --logdir and --budget, in
// internal/shelltier/cmd/shelltier). The sweep's timeout (TIER_SWEEP_TIMEOUT,
// 10m by default) is passed as the runner's --budget, the time limit for each
// script, so the shell scripts are held to the same limit as the Go packages.
func (s *Sweep) Shell() (line string, tails []string, red bool) {
	start := s.Now()
	elapsed := func() int { return int(s.Now().Sub(start) / time.Second) }
	var out bytes.Buffer
	code := s.Exec(".", s.shellEnv(), shellArgv(s.Opts.LogDir, s.Opts.Timeout), &out)
	summary, before, after, red := ShellVerdict(out.String(), code)
	for _, l := range before {
		fmt.Fprintln(s.Stderr, l)
	}
	switch {
	case summary != "":
		return summary, after, red
	case len(before) == 0 && len(after) == 0:
		fmt.Fprintf(s.Stderr, "%s: %s: the shell-tier runner exited %d and printed nothing\n", Name, TierShell, code)
		line, red = Summary(TierShell, s.Opts.LogDir, 0, 0, 0, "", elapsed())
		return line, nil, red
	}
	fmt.Fprintf(s.Stderr, "%s: %s: the shell-tier runner exited %d with no summary line\n", Name, TierShell, code)
	line, red = Summary(TierShell, s.Opts.LogDir, 0, 1, 0, shellNoSummary, elapsed())
	return line, after, red
}

// ShellVerdict reads the shell-tier runner's combined output and exit code. It
// returns the summary line the runner printed, the non-empty lines before it,
// the non-empty lines after it, and whether the tier is RED. The last summary
// line before the first log tail decides; a tail is script output, never the
// run's summary. A non-zero exit is RED even beside a GREEN line, since the
// runner exits 2 on a usage or log-dir failure. Output with no summary line is
// RED, and summary is "" with every line before.
func ShellVerdict(output string, code int) (summary string, before, after []string, red bool) {
	summary, before, after = splitShellOutput(output)
	if summary == "" {
		return "", before, after, true
	}
	return summary, before, after, code != 0 || shellSummaryRE.FindStringSubmatch(summary)[1] == "RED"
}

// shellArgv is the runner's command: every manifest entry, with its logs in the
// log directory the summary line names and each script held to budget.
func shellArgv(logDir, budget string) []string {
	return []string{"go", "run", shellPkg, "--all", "--logdir", logDir, "--budget", budget}
}

// shellEnv is the runner's environment: the sweep's exported environment, with
// the container tests off, as the script's shell branch runs its scripts. The
// entry is appended last, so it is the value the child sees.
func (s *Sweep) shellEnv() []string {
	return append(s.exportEnv(s.Opts.Tested, s.Opts.SkipList), "GT_TEST_DOCKER=0")
}

// splitShellOutput returns the runner's summary line and its other non-empty
// lines, keeping the lines before the summary apart from those after it, so a
// caller prints them in the runner's order. When the output holds more than one
// summary line, the last one before the first log tail is the summary and the
// earlier ones, with anything between, are before it.
func splitShellOutput(text string) (summary string, before, after []string) {
	inTail := false
	for _, l := range strings.Split(text, "\n") {
		switch {
		case l == "":
		case strings.HasPrefix(l, ShellTailHeader):
			inTail = true
			after = append(after, l)
		case inTail:
			after = append(after, l)
		case shellSummaryRE.MatchString(l):
			if summary != "" {
				// The line found earlier was a summary-shaped script line, not
				// the winner: it and the lines between it and this one are
				// before the summary this run reports.
				before = append(append(before, summary), after...)
				after = nil
			}
			summary = l
		case summary == "":
			before = append(before, l)
		default:
			after = append(after, l)
		}
	}
	return summary, before, after
}
