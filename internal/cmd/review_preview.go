package cmd

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

// gt review-preview runs the daemon's local om round on this worktree's head
// before gt done submits it (gt-jx66mp). It puts the head on origin when origin
// does not have it, appends "LOCAL ROUND REQUEST head=<sha>" to the work bead's
// notes, and polls the notes for the LOCAL ROUND block the daemon writes for
// that head. It prints the findings with their ids, so the reply command can
// name them.
//
// The preview never blocks a submit: a timeout, an unavailable block, or any
// failure prints "preview unavailable, submit anyway" on stderr and exits 0.

var reviewPreviewCmd = &cobra.Command{
	Use:     "review-preview",
	GroupID: GroupWork,
	Short:   "Run the local om round on this branch head and print its findings",
	Long: `Ask the daemon for a local om round on the branch head this worktree
holds, wait for its LOCAL ROUND block and print the findings: one line each,
"<id> <severity> <file>:<line> <title>", or "no findings" for an approving round.

The branch head is pushed to origin first when origin does not have it. The
request is one note on the work bead, "LOCAL ROUND REQUEST head=<sha>", and it
is not appended again while one is open for the same head.

The wait is bounded. When no block arrives in time, when the daemon answers
with an unavailable block, or when any step fails, the command prints
"preview unavailable, submit anyway" on stderr and exits 0: the preview never
blocks a submit.

Examples:
  gt review-preview                    # From a polecat worktree on its work bead`,
	Args:         cobra.NoArgs,
	RunE:         runReviewPreview,
	SilenceUsage: true,
}

const (
	// reviewPreviewRequestPrefix opens the note the daemon serves. The head
	// follows it with no separator.
	reviewPreviewRequestPrefix = "LOCAL ROUND REQUEST head="

	// reviewPreviewUnavailable opens every stderr line the preview prints when
	// it gives up. The submit goes ahead.
	reviewPreviewUnavailable = "preview unavailable, submit anyway"

	// reviewPreviewWait bounds the wall-clock wait for a round to be written.
	reviewPreviewWait = 15 * time.Minute

	// reviewPreviewPollEvery is the pause between two reads of the bead's notes.
	reviewPreviewPollEvery = 15 * time.Second
)

// unavailableBlockRE matches the block the daemon writes when a round could not
// run: "LOCAL ROUND (n): unavailable - <reason>" (gt-ryp1ff). The block names no
// head, so it answers the request that precedes it on the bead.
var unavailableBlockRE = regexp.MustCompile(`^LOCAL ROUND \(\d+\): unavailable - (.*)$`)

func init() {
	rootCmd.AddCommand(reviewPreviewCmd)
}

// reviewPreview is one preview run. Each read and write the run makes is a
// field, so the tests drive the wait with fakes: no git, no bd and no real
// sleep.
type reviewPreview struct {
	out, errOut io.Writer
	bead, head  string
	// pushHead puts head on origin when origin does not have it.
	pushHead func() error
	// readNotes returns the work bead's notes as they are now.
	readNotes func() (string, error)
	// appendNote appends one line to the work bead's notes.
	appendNote func(note string) error
	// poll pauses for one interval between two reads.
	poll func(time.Duration)
	// now is the clock the wall bound is measured on.
	now      func() time.Time
	wait     time.Duration
	interval time.Duration
}

// reviewPreviewAnswer is what the work bead's notes say about the last request
// for one head.
type reviewPreviewAnswer struct {
	// requested is true when the notes hold a request for the head.
	requested bool
	// answered is true when a LOCAL ROUND block for the head, or an unavailable
	// block, follows the last request.
	answered bool
	// round is the block for the head. Set when answered with findings.
	round land.LocalRound
	// reason is the daemon's reason when the answer is an unavailable block.
	reason string
}

// run is the whole preview: push, request when none is open, wait, report.
func (p *reviewPreview) run() error {
	// The errors of the git and bd calls are not printed: their text can carry
	// the command's output, so each refusal names the step that failed.
	if err := p.pushHead(); err != nil {
		return p.unavailable("could not put the branch head on origin")
	}
	notes, err := p.readNotes()
	if err != nil {
		return p.unavailable(fmt.Sprintf("could not read the notes of %s", p.bead))
	}
	ans := readReviewPreviewAnswer(notes, p.head)
	if ans.answered && ans.reason == "" {
		return p.report(ans.round)
	}
	// A request is open while the notes hold one for the head with no answer
	// after it. An earlier unavailable answer closes its request, so a new run
	// asks again.
	if !ans.requested || ans.answered {
		if err := p.appendNote(reviewPreviewRequestPrefix + p.head); err != nil {
			return p.unavailable(fmt.Sprintf("could not append the request to %s", p.bead))
		}
	}
	deadline := p.now().Add(p.wait)
	for p.now().Before(deadline) {
		p.poll(p.interval)
		notes, err = p.readNotes()
		if err != nil {
			return p.unavailable(fmt.Sprintf("could not read the notes of %s", p.bead))
		}
		ans = readReviewPreviewAnswer(notes, p.head)
		if !ans.answered {
			continue
		}
		if ans.reason != "" {
			return p.unavailable("the daemon could not run the round: " + ans.reason)
		}
		return p.report(ans.round)
	}
	return p.unavailable(fmt.Sprintf("no LOCAL ROUND block for %s within %s", p.head, p.wait))
}

// report prints a round: its header, then one line per finding with the id the
// reply command names.
func (p *reviewPreview) report(round land.LocalRound) error {
	fmt.Fprintf(p.out, "LOCAL ROUND (%d): head=%s\n", round.Round, round.Head)
	if len(round.Findings) == 0 {
		fmt.Fprintln(p.out, "no findings")
		return nil
	}
	for _, f := range round.Findings {
		fmt.Fprintf(p.out, "%s %s %s:%d %s\n", f.ID, f.Severity, f.Path, f.Line, f.Title)
	}
	return nil
}

// unavailable prints why the preview gave up. It returns nil: the submit goes
// ahead whatever the reason.
func (p *reviewPreview) unavailable(reason string) error {
	fmt.Fprintf(p.errOut, "%s: %s\n", reviewPreviewUnavailable, reason)
	return nil
}

// readReviewPreviewAnswer reads notes for the answer to the last request for
// head. A block or an unavailable block counts only when it follows that
// request, so an answer to an earlier request is not read as this one. A block
// for another head is not an answer and the scan keeps going.
func readReviewPreviewAnswer(notes, head string) reviewPreviewAnswer {
	lines := strings.Split(notes, "\n")
	last := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == reviewPreviewRequestPrefix+head {
			last = i
		}
	}
	if last < 0 {
		return reviewPreviewAnswer{}
	}
	ans := reviewPreviewAnswer{requested: true}
	tail := lines[last+1:]
	if round, ok := land.ParseLastLocalRound(strings.Join(tail, "\n")); ok && round.Head == head {
		ans.answered = true
		ans.round = round
		return ans
	}
	for _, line := range tail {
		if m := unavailableBlockRE.FindStringSubmatch(line); m != nil {
			ans.answered = true
			ans.reason = m[1]
			return ans
		}
	}
	return ans
}

// ensureOriginHead puts head on origin under the branch when origin's tip is
// not already head. The daemon fetches the head from origin, so a head origin
// lacks is no round.
func ensureOriginHead(head string, remoteTip func() (string, error), push func() error) error {
	tip, err := remoteTip()
	if err != nil {
		return err
	}
	if tip == head {
		return nil
	}
	return push()
}

// reviewPreviewNotes reads the notes of bead through show, the bead store's
// Show. A failed read is an error, and so is a Show that returns no issue: an
// empty notes list would look like a bead no round has answered.
func reviewPreviewNotes(bead string, show func(id string) (*beads.Issue, error)) (string, error) {
	iss, err := show(bead)
	if err != nil {
		return "", err
	}
	if iss == nil {
		return "", fmt.Errorf("bead %s not found", bead)
	}
	return iss.Notes, nil
}

// reviewPreviewBeadFromBranch is the work bead a polecat branch names. ok is
// false for a branch that names none.
func reviewPreviewBeadFromBranch(branch string) (string, bool) {
	meta, ok := polecat.ParseBranchName(branch)
	if !ok || meta.Issue == "" {
		return "", false
	}
	return meta.Issue, true
}

func runReviewPreview(cmd *cobra.Command, _ []string) error {
	p := &reviewPreview{
		out:      cmd.OutOrStdout(),
		errOut:   cmd.ErrOrStderr(),
		now:      time.Now,
		poll:     time.Sleep,
		wait:     reviewPreviewWait,
		interval: reviewPreviewPollEvery,
	}
	cwd, err := os.Getwd()
	if err != nil {
		return p.unavailable(fmt.Sprintf("no working directory: %v", err))
	}
	g := git.NewGit(cwd)
	branch, err := g.CurrentBranch()
	if err != nil {
		return p.unavailable("could not read the current branch")
	}
	bead, ok := reviewPreviewBeadFromBranch(branch)
	if !ok {
		return p.unavailable(fmt.Sprintf("branch %s names no work bead", branch))
	}
	head, err := g.Rev("HEAD")
	if err != nil {
		return p.unavailable("could not read HEAD")
	}
	client := beads.New(cwd)
	p.bead = bead
	p.head = head
	p.pushHead = func() error {
		return ensureOriginHead(head,
			func() (string, error) { return g.PushRemoteBranchTip("origin", branch) },
			func() error { return g.Push("origin", branch, false) })
	}
	p.readNotes = func() (string, error) { return reviewPreviewNotes(bead, client.Show) }
	p.appendNote = func(note string) error { return client.AppendNotes(bead, note) }
	return p.run()
}
