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

// Shell runs the shell tier through the shell-tier runner and returns the
// summary line the runner prints, unchanged, and whether the tier is RED. The
// runner's exit code decides RED, and a run that prints no summary line is RED
// too, never an empty line.
func (s *Sweep) Shell() (line string, red bool) {
	start := s.Now()
	elapsed := func() int { return int(s.Now().Sub(start) / time.Second) }
	var out bytes.Buffer
	code := s.Exec(".", s.Environ, shellArgv(s.Opts.LogDir), &out)
	summary, others := splitShellOutput(out.String())
	for _, l := range others {
		fmt.Fprintln(s.Stderr, l)
	}
	switch {
	case summary != "":
		return summary, code != 0
	case len(others) == 0:
		fmt.Fprintf(s.Stderr, "%s: %s: the shell-tier runner exited %d and printed nothing\n", Name, TierShell, code)
		return Summary(TierShell, s.Opts.LogDir, 0, 0, 0, "", elapsed())
	}
	fmt.Fprintf(s.Stderr, "%s: %s: the shell-tier runner exited %d with no summary line\n", Name, TierShell, code)
	return Summary(TierShell, s.Opts.LogDir, 0, 1, 0, shellNoSummary, elapsed())
}

// shellArgv is the runner's command: every manifest entry, with its logs in the
// log directory the summary line names.
func shellArgv(logDir string) []string {
	return []string{"go", "run", shellPkg, "--all", "--logdir", logDir}
}

// splitShellOutput returns the runner's summary line and every other non-empty
// line in order. When the output holds more than one summary line, the last
// one is the summary and the earlier ones are among the others.
func splitShellOutput(text string) (summary string, others []string) {
	for _, l := range strings.Split(text, "\n") {
		switch {
		case l == "":
		case shellSummaryRE.MatchString(l):
			if summary != "" {
				others = append(others, summary)
			}
			summary = l
		default:
			others = append(others, l)
		}
	}
	return summary, others
}
