package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/callercwd"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
)

// gt review-preview reply answers one finding of the last LOCAL ROUND on the
// work bead (gt-16x50l). The answer is one LOCAL ROUND REPLY line, "fixed" or
// "disputed", in the form land.ParseLocalRoundReplies reads back.

var reviewPreviewReplyCmd = &cobra.Command{
	Use:   "reply <finding-id>",
	Short: "Answer one finding of the last local round: fixed or disputed",
	Long: `Record the answer to one finding of the last LOCAL ROUND on this branch's
work bead. Give exactly one of --fixed or --disputed, and the text with -m.

--fixed is refused while the branch head is still the head the round reviewed:
a fix has to be committed first. --disputed needs a reason. An id the last
round does not list is refused. A refused reply writes nothing.

Examples:
  gt review-preview reply f1 --fixed -m "bounded the poll loop"
  gt review-preview reply f2 --disputed -m "the comment matches the code"`,
	Args:         cobra.ExactArgs(1),
	RunE:         runReviewPreviewReply,
	SilenceUsage: true,
}

func init() {
	reviewPreviewCmd.AddCommand(reviewPreviewReplyCmd)
	reviewPreviewReplyCmd.Flags().Bool("fixed", false, "the finding is fixed in a commit after the round's head")
	reviewPreviewReplyCmd.Flags().Bool("disputed", false, "the finding is wrong; the reason goes with -m")
	reviewPreviewReplyCmd.Flags().StringP("message", "m", "", "the reply text")
}

// reviewPreviewReply is one reply run. The bead's notes are read and appended
// through function fields, so the tests drive it with fakes: no git, no bd.
type reviewPreviewReply struct {
	bead string
	// branchHead returns the head the branch is at now.
	branchHead func() (string, error)
	// readNotes returns the work bead's notes as they are now.
	readNotes func() (string, error)
	// appendNote appends one line to the work bead's notes.
	appendNote func(note string) error
}

func runReviewPreviewReply(cmd *cobra.Command, args []string) error {
	fixed, disputed, text, err := reviewPreviewReplyFlags(cmd)
	if err != nil {
		return err
	}
	kind, err := reviewPreviewReplyKind(fixed, disputed)
	if err != nil {
		return err
	}
	return runReviewPreviewReplyFrom(callercwd.Require, args[0], kind, text)
}

// reviewPreviewReplyFlags reads the reply's three flags off cmd. A command that
// does not define them is refused, so the error names the wiring, not the user.
func reviewPreviewReplyFlags(cmd *cobra.Command) (fixed, disputed bool, text string, err error) {
	if fixed, err = cmd.Flags().GetBool("fixed"); err != nil {
		return false, false, "", err
	}
	if disputed, err = cmd.Flags().GetBool("disputed"); err != nil {
		return false, false, "", err
	}
	if text, err = cmd.Flags().GetString("message"); err != nil {
		return false, false, "", err
	}
	return fixed, disputed, text, nil
}

// runReviewPreviewReplyFrom runs the reply in the directory resolve names. The
// command passes callercwd.Require; a test passes a resolver that fails, so the
// refusal runs with no git and no bd.
func runReviewPreviewReplyFrom(resolve func() (string, error), finding, kind, text string) error {
	cwd, err := resolve()
	if err != nil {
		return errors.New("no working directory")
	}
	return runReviewPreviewReplyAt(git.NewGit(cwd), beads.New(cwd), finding, kind, text)
}

// runReviewPreviewReplyAt answers finding on the work bead the branch names. The
// head the round is checked against is the branch's HEAD, and the notes are read
// and the reply appended on that bead only.
func runReviewPreviewReplyAt(repo reviewPreviewRepo, store reviewPreviewStore, finding, kind, text string) error {
	branch, err := repo.CurrentBranch()
	if err != nil {
		return errors.New("could not read the current branch")
	}
	bead, ok := reviewPreviewBeadFromBranch(branch)
	if !ok {
		return fmt.Errorf("branch %s names no work bead", branch)
	}
	r := reviewPreviewReply{
		bead:       bead,
		branchHead: func() (string, error) { return repo.Rev("HEAD") },
		readNotes:  func() (string, error) { return reviewPreviewNotes(bead, store.Show) },
		appendNote: func(note string) error { return store.AppendNotes(bead, note) },
	}
	return r.run(finding, kind, text)
}

// reviewPreviewReplyKind maps the two flags to the kind a reply carries. Exactly
// one of them must be set.
func reviewPreviewReplyKind(fixed, disputed bool) (string, error) {
	switch {
	case fixed && disputed:
		return "", errors.New("give one of --fixed or --disputed, not both")
	case fixed:
		return land.LocalRoundFixed, nil
	case disputed:
		return land.LocalRoundDisputed, nil
	}
	return "", errors.New("give --fixed or --disputed")
}

// run appends the reply to finding of the last LOCAL ROUND on the bead. Every
// refusal returns before the append, so a refused reply writes nothing.
func (r reviewPreviewReply) run(finding, kind, text string) error {
	if strings.TrimSpace(text) == "" {
		if kind == land.LocalRoundDisputed {
			return errors.New("--disputed needs a reason: give it with -m")
		}
		return errors.New("the reply has no text: give it with -m")
	}
	notes, err := r.readNotes()
	if err != nil {
		return fmt.Errorf("could not read the notes of %s", r.bead)
	}
	round, ok := land.ParseLastLocalRound(notes)
	if !ok {
		return fmt.Errorf("no LOCAL ROUND block on %s to answer", r.bead)
	}
	if !reviewPreviewRoundLists(round, finding) {
		return fmt.Errorf("finding %s is not in LOCAL ROUND (%d) on %s", finding, round.Round, r.bead)
	}
	if kind == land.LocalRoundFixed {
		head, err := r.branchHead()
		if err != nil {
			return errors.New("could not read the branch head")
		}
		if head == round.Head {
			return fmt.Errorf("--fixed refused: the branch head is still the head LOCAL ROUND (%d) reviewed, so nothing changed since the round", round.Round)
		}
	}
	if err := r.appendNote(land.FormatLocalRoundReply(finding, kind, text)); err != nil {
		return fmt.Errorf("could not append the reply to %s", r.bead)
	}
	return nil
}

// reviewPreviewRoundLists reports whether round has a finding with id.
func reviewPreviewRoundLists(round land.LocalRound, id string) bool {
	for _, f := range round.Findings {
		if f.ID == id {
			return true
		}
	}
	return false
}
