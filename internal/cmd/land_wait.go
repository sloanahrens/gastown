package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/daemonlog"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/role"
	"github.com/steveyegge/gastown/internal/workspace"
)

// landWaitInterval is how often the waiter re-reads the landing log. A bird
// waits with nothing else to build, so a slow poll costs the host nothing and
// still answers promptly after a landing (gt-hmxex).
const landWaitInterval = 5 * time.Second

// landWaitDetailMisses bounds the polls an outcome line waits for the bead's
// own record of it. The landing worker logs the landing before it writes the
// record, so a first read can arrive early; a record that never appears is
// reported rather than waited on until the timeout.
const landWaitDetailMisses = 3

// errLandWaitNoOutcome is the answer to a wait that saw nothing: the bird runs
// the command again, which is what its next turn is for.
var errLandWaitNoOutcome = errors.New("no outcome yet, run again")

var (
	landWaitAgent   string
	landWaitTimeout string
)

var landWaitCmd = &cobra.Command{
	Use:   "wait",
	Short: "Wait for the outcome of your own crew-branch landings",
	Long: `Wait for a landing of your own crew branch and print its outcome.

Reads the landing worker's daemon log and prints one line per outcome of a bead
whose branch is the caller's own (crew/<agent>/...), with the bead's own record
of it: a landing names the commit it landed as, a refusal names the gate or
review that refused it, om's score, the finding count and which refusal of that
kind it is.

A refusal that is the third of its kind ends the rework loop: the line says the
overseer holds the bead, and a push of that branch will not be landed.

An outcome counts only when it happened after the command started. With none in
sight the wait exits 3 after its timeout, saying to run again; that is the loop a
bird with nothing else to build runs until its work has a verdict.

Examples:
  gt land wait
  gt land wait --agent dunlin --timeout 20m`,
	Args: cobra.NoArgs,
	RunE: runLandWait,
}

func init() {
	landWaitCmd.Flags().StringVar(&landWaitAgent, "agent", "", "the crew agent whose branches to watch (default: the crew member GT_ROLE names)")
	landWaitCmd.Flags().StringVar(&landWaitTimeout, "timeout", "9m", "how long to wait for an outcome before exiting 3")
	landCmd.AddCommand(landWaitCmd)
}

func runLandWait(cmd *cobra.Command, _ []string) error {
	timeout, err := landWaitParseTimeout(landWaitTimeout)
	if err != nil {
		return err
	}
	agent, err := landWaitAgentFrom(landWaitAgent, os.Getenv)
	if err != nil {
		return err
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("resolving the town: %w", err)
	}
	_, err = landWaitPoll(landWaitDeps{
		agent:    agent,
		timeout:  timeout,
		interval: landWaitInterval,
		now:      time.Now,
		sleep:    time.Sleep,
		source:   landWaitLogSource(filepath.Join(townRoot, "daemon")),
		detail:   landWaitBeadNotes(os.Getwd),
		out:      cmd.OutOrStdout(),
		errOut:   cmd.ErrOrStderr(),
	})
	return landWaitAnswer(cmd.OutOrStdout(), err)
}

// landWaitAnswer turns the wait's end into what the command reports: the
// run-again line and exit 3 when nothing arrived, the wait's own error
// otherwise.
func landWaitAnswer(out io.Writer, err error) error {
	if !errors.Is(err, errLandWaitNoOutcome) {
		return err
	}
	fmt.Fprintln(out, errLandWaitNoOutcome)
	return NewSilentExit(3)
}

// landWaitParseTimeout reads --timeout as a duration to wait.
func landWaitParseTimeout(value string) (time.Duration, error) {
	waited, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("--timeout %q: want a duration such as 9m or 90s", value)
	}
	if waited <= 0 {
		return 0, fmt.Errorf("--timeout %q: want a duration longer than none", value)
	}
	return waited, nil
}

// landWaitAgentFrom names the crew agent whose branches the waiter watches: the
// --agent flag, else the crew member GT_ROLE names. A polecat, a daemon job or
// no role names no crew branch, so it is refused rather than waiting for
// outcomes that could never be its own.
func landWaitAgentFrom(flag string, getenv func(string) string) (string, error) {
	if agent := strings.TrimSpace(flag); agent != "" {
		return agent, nil
	}
	gtRole := strings.TrimSpace(getenv("GT_ROLE"))
	if gtRole == "" {
		return "", fmt.Errorf("gt land wait watches a crew member's branches: pass --agent <name> (GT_ROLE is unset)")
	}
	parsed, _, name := role.Parse(gtRole)
	if parsed != role.Crew || name == "" {
		return "", fmt.Errorf("gt land wait watches a crew member's branches: pass --agent <name> (GT_ROLE is %q)", gtRole)
	}
	return name, nil
}

// landWaitDeps is one wait over injected inputs: its clock, sleep, log read,
// bead read and writers, so a test drives every branch without a real sleep,
// the host's daemon log or a beads store.
type landWaitDeps struct {
	agent    string
	timeout  time.Duration
	interval time.Duration
	now      func() time.Time
	sleep    func(time.Duration)
	source   func(since time.Time) ([]landWaitEvent, error)
	detail   func(landWaitEvent) (landWaitOutcome, bool, error)
	out      io.Writer
	errOut   io.Writer
}

// landWaitPoll prints one line per outcome of the agent's own branches and
// stops at the first poll that printed one, so a bird hears of its first
// landing rather than waiting out the others. It returns errLandWaitNoOutcome
// when the timeout passes with nothing printed.
func landWaitPoll(deps landWaitDeps) (reported int, err error) {
	if deps.interval <= 0 {
		return 0, fmt.Errorf("the poll interval must be positive, not %s", deps.interval)
	}
	if deps.timeout <= 0 {
		return 0, fmt.Errorf("the wait must be longer than none, not %s", deps.timeout)
	}
	start := deps.now()
	deadline := start.Add(deps.timeout)
	seen := make(map[string]bool)
	misses := make(map[string]int)
	for {
		events, err := deps.source(start)
		if err != nil {
			return reported, err
		}
		for _, ev := range events {
			key := ev.key()
			if seen[key] {
				continue
			}
			outcome, found, err := deps.detail(ev)
			if err != nil {
				return reported, err
			}
			if !found {
				// The record is written after the line; a later poll reads it.
				if misses[key]++; misses[key] < landWaitDetailMisses {
					continue
				}
				fmt.Fprintf(deps.errOut, "skipped the outcome of %s (%s): the bead's notes carry no matching record\n", ev.Bead, ev.outcomeWord())
				seen[key] = true
				continue
			}
			seen[key] = true
			if !landWaitOurs(deps.agent, outcome.Branch) {
				continue
			}
			fmt.Fprintln(deps.out, landWaitLine(outcome))
			reported++
		}
		if reported > 0 {
			return reported, nil
		}
		if !deps.now().Before(deadline) {
			return 0, errLandWaitNoOutcome
		}
		deps.sleep(deps.interval)
	}
}

// landWaitOurs reports whether branch is the agent's own crew branch: the crew
// member's checkout branch, or a submission made from it.
func landWaitOurs(agent, branch string) bool {
	prefix := authorkind.CrewPrefix + agent
	return branch == prefix || strings.HasPrefix(branch, prefix+"/")
}

// landWaitEvent is one landing outcome the landing worker's log names: the bead,
// whether it landed, and the refusal's kind when it did not. The bead's record
// says where; the line says when.
type landWaitEvent struct {
	At     time.Time
	Bead   string
	Landed bool
	// Commit is the commit a landed line names, short or full.
	Commit string
	// Kind is the failure class a refused line names.
	Kind string
}

// key identifies one outcome across polls, so re-reading the log does not
// repeat a line already printed or skipped.
func (e landWaitEvent) key() string {
	return e.Bead + "\x00" + e.outcomeWord() + "\x00" + e.At.UTC().Format(time.RFC3339Nano)
}

// outcomeWord names the outcome in a message a bird reads.
func (e landWaitEvent) outcomeWord() string {
	if e.Landed {
		return "landed"
	}
	return "rejected (" + e.Kind + ")"
}

var (
	// landWaitLineRE finds the outcome line the landing worker tags with [land].
	// No other component writes the tag, so the worker's duplicate "landing
	// rejected (...)" line does not read as a second outcome.
	landWaitLineRE = regexp.MustCompile(`\[land\] (\S+): (.+)$`)
	// landWaitLandedRE reads "landed <commit> on <remote>/<target> (...)"; the
	// "merged" line before it is not an outcome, because a merge can still be
	// reviewed or gated away.
	landWaitLandedRE = regexp.MustCompile(`^landed ([0-9a-f]+) on `)
	// landWaitRejectedRE reads "rejected (<kind>): <reason>".
	landWaitRejectedRE = regexp.MustCompile(`^rejected \(([a-z_]+)\): `)
)

// parseLandWaitLine reads a daemon-log line as the outcome it records, without
// its stamp.
func parseLandWaitLine(text string) (landWaitEvent, bool) {
	m := landWaitLineRE.FindStringSubmatch(text)
	if m == nil {
		return landWaitEvent{}, false
	}
	ev := landWaitEvent{Bead: m[1]}
	if lm := landWaitLandedRE.FindStringSubmatch(m[2]); lm != nil {
		ev.Landed, ev.Commit = true, lm[1]
		return ev, true
	}
	if rm := landWaitRejectedRE.FindStringSubmatch(m[2]); rm != nil {
		ev.Kind = rm[1]
		return ev, true
	}
	return landWaitEvent{}, false
}

// landWaitLogSource reads the landing outcomes at or after since from the daemon
// log at dir. Each poll reads the log whole: the prune patrol keeps it small,
// and daemonlog.Read follows the rotation a tail read cannot.
func landWaitLogSource(dir string) func(since time.Time) ([]landWaitEvent, error) {
	return func(since time.Time) ([]landWaitEvent, error) {
		lines, err := daemonlog.Read(dir, since, time.Local)
		if err != nil {
			return nil, fmt.Errorf("reading the daemon log: %w", err)
		}
		var events []landWaitEvent
		for _, line := range lines {
			ev, ok := parseLandWaitLine(line.Text)
			if !ok {
				continue
			}
			ev.At = line.At
			events = append(events, ev)
		}
		return events, nil
	}
}

// landWaitBeadNotes reads the outcome's branch and figures from the bead's own
// record of it, the notes gt land verdict reads. A read failure is returned
// rather than read as no outcome: a bird that cannot see its bead's notes has
// not been told the landing did not happen.
func landWaitBeadNotes(getwd func() (string, error)) func(landWaitEvent) (landWaitOutcome, bool, error) {
	return func(ev landWaitEvent) (landWaitOutcome, bool, error) {
		cwd, err := getwd()
		if err != nil {
			return landWaitOutcome{}, false, fmt.Errorf("resolving the working directory: %w", err)
		}
		bd := beads.NewWithBeadsDir(cwd, beads.ResolveBeadsDirForID(beads.ResolveBeadsDir(cwd), ev.Bead))
		issue, err := bd.Show(ev.Bead)
		if err != nil {
			return landWaitOutcome{}, false, fmt.Errorf("reading %s: %w", ev.Bead, err)
		}
		if issue == nil {
			return landWaitOutcome{}, false, fmt.Errorf("reading %s: no such bead", ev.Bead)
		}
		outcome, ok := landWaitNoteOutcome(issue.Notes, ev)
		return outcome, ok, nil
	}
}

// landWaitNoteOutcome reads an outcome's branch and figures from the bead's
// record of it: the LANDING RECORD block for a landing, the MERGE REJECTION
// block for a refusal. A note that does not carry the block the event names is
// no answer, because the landing that wrote it has not recorded itself yet or
// an earlier landing of the bead has.
func landWaitNoteOutcome(notes string, ev landWaitEvent) (landWaitOutcome, bool) {
	if ev.Landed {
		rec, ok := land.ParseLandingRecord(notes)
		if !ok || !strings.HasPrefix(rec.LandedCommit, ev.Commit) {
			return landWaitOutcome{}, false
		}
		return landWaitOutcome{Bead: ev.Bead, Branch: rec.Branch, Landed: true,
			Commit: rec.LandedCommit, Target: rec.Target}, true
	}
	refusal, ok := land.ParseRejectionNote(notes)
	if !ok || refusal.Kind != ev.Kind {
		return landWaitOutcome{}, false
	}
	out := landWaitOutcome{Bead: ev.Bead, Branch: refusal.Branch, Kind: refusal.Kind,
		Findings: len(refusal.Findings), Attempt: refusal.Attempt}
	if refusal.Receipt != nil {
		out.Score, out.HasScore = refusal.Receipt.Score, true
	}
	return out, true
}

// landWaitOutcome is one outcome as the waiter prints it.
type landWaitOutcome struct {
	Bead   string
	Branch string
	Landed bool
	// Commit and Target are the landed commit and the branch it landed on.
	Commit string
	Target string
	// Kind is the refusal's failure class, with the om score the refusal
	// carried when a review reached one.
	Kind     string
	Score    float64
	HasScore bool
	Findings int
	// Attempt is which refusal of its kind this was, counted by the landing
	// worker against the rework ceiling.
	Attempt int
}

// landWaitLine renders one outcome as the single line a bird reads. The third
// refusal of a kind is the ceiling: the landing worker stops reworking it, so
// the line tells the bird to stop pushing instead of paying for a fourth gate.
func landWaitLine(o landWaitOutcome) string {
	if o.Landed {
		return fmt.Sprintf("landed %s as %s on %s (%s)", o.Bead, o.Commit, o.Target, o.Branch)
	}
	score := "no om score"
	if o.HasScore {
		score = fmt.Sprintf("score %.2f", o.Score)
	}
	line := fmt.Sprintf("rejected %s (%s) on %s: %s, %d finding(s), refusal %d of %d",
		o.Bead, o.Kind, o.Branch, score, o.Findings, o.Attempt, land.MaxReworkAttempts)
	if o.Attempt >= land.MaxReworkAttempts {
		line += "; the overseer has this bead now, so do not push it again: a new push will not be landed"
	}
	return line + fmt.Sprintf("; run `gt land verdict %s` for the findings", o.Bead)
}
