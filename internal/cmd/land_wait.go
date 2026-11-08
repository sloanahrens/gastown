package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// landWaitSettle is how long a refusal is held after its log line before its
// record is read. The record is the bead write that follows the line, which
// lands well inside this; a newer refusal may not have it yet, so the refusals
// of its bead and kind wait with it (gt-hmxex).
const landWaitSettle = landWaitInterval * landWaitDetailMisses

// landWaitPlainRE is what a printed branch, target or commit may hold: a plain
// token, with no space or control character to reach the agent's terminal.
var landWaitPlainRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)

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
	return landWaitCommand(cmd, landWaitAgent, landWaitTimeout, landWaitProcessHost())
}

// landWaitProcessHost is the host gt land wait reads from this process. The
// bead store the wait polls is the record of a landing's verdict, so its
// directory is the process's own: a caller-supplied variable must not point it
// at another store.
func landWaitProcessHost() landWaitHost {
	return landWaitHost{
		getenv:   os.Getenv,
		getwd:    os.Getwd,
		findTown: workspace.FindFromCwdOrError,
		now:      time.Now,
		sleep:    time.Sleep,
	}
}

// landWaitHost is what the command reads from its host: the environment, the
// working directory, the town and the clock. The command body takes it whole,
// so a test drives each refusal and each wait without a flag, the host's
// environment, its town or a real sleep.
type landWaitHost struct {
	getenv   func(string) string
	getwd    func() (string, error)
	findTown func() (string, error)
	now      func() time.Time
	sleep    func(time.Duration)
}

// landWaitCommand is the command body over the host it is given.
func landWaitCommand(cmd *cobra.Command, agentFlag, timeoutFlag string, host landWaitHost) error {
	timeout, err := landWaitParseTimeout(timeoutFlag)
	if err != nil {
		return err
	}
	agent, err := landWaitAgentFrom(agentFlag, host.getenv)
	if err != nil {
		return err
	}
	townRoot, err := host.findTown()
	if err != nil {
		return fmt.Errorf("resolving the town: %w", err)
	}
	_, err = landWaitPoll(landWaitDeps{
		agent:    agent,
		timeout:  timeout,
		interval: landWaitInterval,
		settle:   landWaitSettle,
		now:      host.now,
		sleep:    host.sleep,
		source:   landWaitLogSource(filepath.Join(townRoot, "daemon")),
		detail: landWaitBeadNotes(func(bead string) (landWaitBeadReader, error) {
			return landWaitBeadStore(host.getwd, bead)
		}),
		out:    cmd.OutOrStdout(),
		errOut: cmd.ErrOrStderr(),
	})
	return landWaitFinish(cmd, err)
}

// landWaitFinish is what the command returns: the run-again line with exit 3
// when nothing arrived, the wait's own error otherwise. A silent exit is the
// answer, so cobra is told not to print it as a failure; a real error still is.
func landWaitFinish(cmd *cobra.Command, err error) error {
	answer := landWaitAnswer(cmd.OutOrStdout(), err)
	if _, ok := IsSilentExit(answer); ok {
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
	}
	return answer
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
	// settle is how old a refusal must be before its record is read.
	settle time.Duration
	now    func() time.Time
	sleep  func(time.Duration)
	source func(since time.Time) ([]landWaitEvent, error)
	detail func(landWaitEvent) (landWaitOutcome, bool, error)
	out    io.Writer
	errOut io.Writer
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
		held := landWaitHeld(events, deps.now(), deps.settle)
		for _, ev := range events {
			if !ev.Landed && held[ev.pair()] {
				continue
			}
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
				seen[key] = true
				// The bead's notes name another crew member's branch, so this
				// outcome is theirs and not reported to this bird.
				if outcome.Branch != "" && !landWaitOurs(deps.agent, outcome.Branch) {
					continue
				}
				fmt.Fprintf(deps.errOut, "skipped the outcome of %s (%s): the bead's notes carry no matching record\n", ev.Bead, ev.outcomeWord())
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

// landWaitHeld names the bead and kind pairs whose newest refusal since the
// start is younger than settle. Each refusal's record is found by counting from
// the newest block of its kind, so while that newest record is unwritten every
// refusal of the pair waits with it: an earlier block would otherwise be read
// as a later refusal's record.
func landWaitHeld(events []landWaitEvent, now time.Time, settle time.Duration) map[string]bool {
	held := make(map[string]bool)
	if settle <= 0 {
		return held
	}
	for _, ev := range events {
		if ev.Landed || ev.Ordinal != ev.Window {
			continue
		}
		if now.Sub(ev.At) < settle {
			held[ev.pair()] = true
		}
	}
	return held
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
	// Ordinal is which refusal of its kind this is for the bead, counted from
	// the start, and Window is how many refusals of that kind the bead has had
	// since the start, this one included (both zero for a landing). A refusal's
	// record is found from the newest block of its kind, so the log's history
	// before the start is never needed to place it.
	Ordinal int
	Window  int
}

// pair names the bead and kind a refusal belongs to: the blocks of that kind
// number its refusals.
func (e landWaitEvent) pair() string {
	return e.Bead + "\x00" + e.Kind
}

// key identifies one outcome across polls, so re-reading the log does not
// repeat a line already printed or skipped. A refusal is named by its ordinal,
// which the window since the start keeps fixed as later refusals arrive; two
// refusals in one second are two keys, where a time stamp would fold them.
func (e landWaitEvent) key() string {
	if e.Landed {
		return "landed\x00" + e.Bead + "\x00" + e.Commit
	}
	return "rejected\x00" + e.pair() + "\x00" + strconv.Itoa(e.Ordinal)
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
// log at dir. Each poll reads the log from since on: daemonlog.Read follows the
// rotation a tail read cannot. Nothing before since is read, so the log's own
// history, pruned by rotation, never changes the numbering of a refusal; its
// record is found in the notes, which keep every block.
func landWaitLogSource(dir string) func(since time.Time) ([]landWaitEvent, error) {
	return func(since time.Time) ([]landWaitEvent, error) {
		if err := landWaitLogPresent(dir, since); err != nil {
			return nil, err
		}
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
		landWaitNumber(events)
		return events, nil
	}
}

// landWaitNumber places each refusal in its window: its ordinal among the
// bead's refusals of its kind since the start, and how many the window holds.
func landWaitNumber(events []landWaitEvent) {
	window := make(map[string]int)
	for _, ev := range events {
		if !ev.Landed {
			window[ev.pair()]++
		}
	}
	seen := make(map[string]int)
	for i := range events {
		ev := &events[i]
		if ev.Landed {
			continue
		}
		seen[ev.pair()]++
		ev.Ordinal, ev.Window = seen[ev.pair()], window[ev.pair()]
	}
}

// landWaitLogPresent refuses a town whose daemon log is missing: a log that
// is not there reads as no outcome, the same as a quiet town, so the wait
// would exit 3 forever. A rotation can briefly leave the live log absent, so
// the log is missing only when no rotated copy from since on exists either.
func landWaitLogPresent(dir string, since time.Time) error {
	_, err := os.Stat(filepath.Join(dir, daemonlog.FileName))
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading the daemon log: %w", err)
	}
	if len(daemonlog.Backups(dir, since)) > 0 {
		return nil
	}
	return fmt.Errorf("the daemon log is missing from %s: no landing outcome can be read from it; is this the town's daemon directory?", dir)
}

// landWaitAttemptOf is the attempt a refusal of kind on its nth refusal carries
// in its record. A review's attempt is capped at the rework ceiling (gt-ern0y),
// and a gate or policy refusal numbers among its own kind, uncapped.
func landWaitAttemptOf(kind string, n int) int {
	if kind == string(land.RejectReview) {
		return min(n, land.MaxReworkAttempts)
	}
	return n
}

// landWaitBeadReader is the one read the waiter makes of a bead: its notes, the
// record gt land verdict reads. *beads.Beads satisfies it; a test passes a fake.
type landWaitBeadReader interface {
	Show(id string) (*beads.Issue, error)
}

// landWaitBeadStore opens the beads store a bead's notes live in, routed by the
// bead's prefix, from the working directory getwd names.
func landWaitBeadStore(getwd func() (string, error), bead string) (landWaitBeadReader, error) {
	cwd, err := getwd()
	if err != nil {
		return nil, fmt.Errorf("resolving the working directory: %w", err)
	}
	return beads.NewWithBeadsDir(cwd, beads.ResolveBeadsDirForID(beads.ResolveBeadsDir(cwd), bead)), nil
}

// landWaitBeadID is the shape of a bead id the worker's log may name. It is
// checked before the id reaches a store path or a store command, so a log
// line cannot steer the read to another path or read as a flag.
var landWaitBeadID = regexp.MustCompile(`^[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*$`)

// landWaitBeadNotes answers an event from the bead's own record of it. A store
// that cannot be opened or read is returned rather than read as no outcome: a
// bird that cannot see its bead's notes has not been told the landing did not
// happen.
func landWaitBeadNotes(open func(bead string) (landWaitBeadReader, error)) func(landWaitEvent) (landWaitOutcome, bool, error) {
	return func(ev landWaitEvent) (landWaitOutcome, bool, error) {
		if !landWaitBeadID.MatchString(ev.Bead) {
			return landWaitOutcome{}, false, fmt.Errorf("refusing to read %q: the daemon log names it, but it is not a bead id", ev.Bead)
		}
		bd, err := open(ev.Bead)
		if err != nil {
			return landWaitOutcome{}, false, err
		}
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
// an earlier landing or refusal of the bead has.
//
// A miss still names the branch the bead's notes record, when they record one,
// so the waiter can tell an outcome that is someone else's from one it cannot
// attribute. A record whose branch, target or commit is not a plain token is a
// miss too: its text is never printed to the agent.
func landWaitNoteOutcome(notes string, ev landWaitEvent) (landWaitOutcome, bool) {
	miss := landWaitOutcome{Bead: ev.Bead, Branch: landWaitNotesBranch(notes)}
	if ev.Landed {
		rec, ok := land.ParseLandingRecord(notes)
		if !ok || !strings.HasPrefix(rec.LandedCommit, ev.Commit) {
			return miss, false
		}
		out := landWaitOutcome{Bead: ev.Bead, Branch: rec.Branch, Landed: true,
			Commit: rec.LandedCommit, Target: rec.Target}
		if !landWaitPrintable(out) {
			return miss, false
		}
		return out, true
	}
	refusal, ok := landWaitRefusalRecord(notes, ev)
	if !ok {
		return miss, false
	}
	out := landWaitOutcome{Bead: ev.Bead, Branch: refusal.Branch, Kind: refusal.Kind,
		Findings: len(refusal.Findings), Attempt: refusal.Attempt}
	if refusal.Receipt != nil {
		out.Score, out.HasScore = refusal.Receipt.Score, true
	}
	if !landWaitPrintable(out) {
		return miss, false
	}
	return out, true
}

// landWaitRefusalRecord is the block that recorded ev's refusal. The blocks of
// the event's kind are the bead's refusals of that kind, in order, and the
// window's last refusal is the newest of them, so the event's block is
// Window-Ordinal blocks older than the newest. The notes must hold Window blocks:
// with fewer, the newest refusal's block is not written yet, and counting from
// the end would name an earlier refusal's block. A block whose number is not its
// place answers nothing; review numbers stop at the rework ceiling, as the
// landing's records do.
func landWaitRefusalRecord(notes string, ev landWaitEvent) (land.RejectionNote, bool) {
	blocks := land.RejectionsOfKind(notes, ev.Kind)
	if ev.Ordinal < 1 || ev.Ordinal > ev.Window || len(blocks) < ev.Window {
		return land.RejectionNote{}, false
	}
	pos := len(blocks) - ev.Window + ev.Ordinal
	rec := blocks[pos-1]
	if rec.Attempt != landWaitAttemptOf(ev.Kind, pos) {
		return land.RejectionNote{}, false
	}
	return rec, true
}

// landWaitPrintable reports whether the outcome's printed fields are plain
// tokens. A refusal prints its branch; a landing prints its branch, target and
// commit.
func landWaitPrintable(o landWaitOutcome) bool {
	if !landWaitPlainRE.MatchString(o.Branch) {
		return false
	}
	if o.Landed {
		return landWaitPlainRE.MatchString(o.Target) && landWaitPlainRE.MatchString(o.Commit)
	}
	return true
}

// landWaitNotesBranch is the branch the bead's notes record, from its landing
// record or its last refusal, or empty when they record none.
func landWaitNotesBranch(notes string) string {
	if rec, ok := land.ParseLandingRecord(notes); ok && rec.Branch != "" {
		return rec.Branch
	}
	if rej, ok := land.ParseRejectionNote(notes); ok {
		return rej.Branch
	}
	return ""
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
