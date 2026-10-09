package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// landCmd is the operator's handle on the landing queue. It is operator-only
// surface: a polecat never re-queues its own rejected landing, and the
// ready-to-land label is written here rather than by hand (the handbook's
// "never hand-write READY TO LAND").
var landCmd = &cobra.Command{
	Use:     "land",
	GroupID: GroupWork,
	Short:   "Operate on a rig's landing queue",
	Long: `Operate on a rig's landing queue.

Landings are queued by gt done and drained by the daemon's landing worker. A
landing the worker refuses goes back to dispatch labeled rework. These commands
are the operator's sanctioned way to act on that queue.

Subcommands:
  requeue   Put a rejected landing back in the queue unchanged

Examples:
  gt land requeue gt-3e1z4 --reason "the gate failed on a runner fault, not the diff"`,
	RunE: requireSubcommand,
}

// landRequeueReason is the operator's note for gt land requeue.
var landRequeueReason string

var landRequeueCmd = &cobra.Command{
	Use:   "requeue <bead>",
	Short: "Re-queue a rejected landing whose branch head is unchanged",
	Long: `Re-queue a rejected landing unchanged.

A bead the landing worker refused carries the rework label and a MERGE
REJECTION note naming the head it rejected. When the polecat's branch still
sits on that head, nothing needs to change: the rejection was environmental or
otherwise unrelated to the diff. gt land requeue restores the ready-to-land
label and removes rework, so the landing worker merges the same head again.

It refuses when the head changed — that is a normal resubmission and belongs to
gt done, which re-gates and re-pushes it — when the bead is not in a rejected
state, and when the branch is gone from the remote. It only ever re-queues a
head whose original submission already passed presubmit; a new head must go
through gt done.

The operator, the reason and the head are recorded as a comment on the bead.`,
	Args: cobra.ExactArgs(1),
	RunE: runLandRequeue,
}

func init() {
	landRequeueCmd.Flags().StringVar(&landRequeueReason, "reason", "", "why the rejection does not need a code change (recorded on the bead)")
	landCmd.AddCommand(landRequeueCmd)
	rootCmd.AddCommand(landCmd)
}

// landRequeueBeads is the bead surface a re-queue needs. *beads.Beads
// satisfies it; tests pass a fake.
type landRequeueBeads interface {
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	AddCommentAs(id, author, comment string) error
}

// landRequeueRemote reads a branch's tip from a remote. *git.Git satisfies it.
type landRequeueRemote interface {
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
}

func runLandRequeue(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("resolving the town: %w", err)
	}
	rigName := resolveRigForBead(townRoot, beadID)
	if rigName == "" {
		return fmt.Errorf("cannot resolve the rig that owns %s: its ID prefix maps to no rig", beadID)
	}
	remote, err := rig.ResolveLandingRemote(townRoot, rigName)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolving the working directory: %w", err)
	}
	bd := beads.NewWithBeadsDir(cwd, beads.ResolveBeadsDirForID(beads.ResolveBeadsDir(cwd), beadID))
	repo := git.NewGit(filepath.Join(townRoot, rigName, ".repo.git"))

	reason := strings.TrimSpace(landRequeueReason)
	if reason == "" {
		reason = "(no reason given)"
	}
	return requeueRejectedLanding(bd, repo, remote, beadID, reason, requeueActor(), cmd.OutOrStdout())
}

// requeueActor names the operator behind a re-queue for the durable record:
// the polecat if one is driving the command, else the invoking user.
func requeueActor() string {
	if p := strings.TrimSpace(os.Getenv("GT_POLECAT")); p != "" {
		return p
	}
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" {
		return user
	}
	return "operator"
}

// requeueRejectedLanding is gt land requeue's decision, over the interfaces so
// a test drives it without a town, a remote or a live bd. It re-queues only an
// unchanged head: the bead must be in the rework state, its live rejection must
// name a branch and a head, and the branch's tip on the remote must be that
// head. Every other input is refused with a message naming what to do instead.
func requeueRejectedLanding(bd landRequeueBeads, remote landRequeueRemote, remoteName, beadID, reason, actor string, out io.Writer) error {
	issue, err := bd.Show(beadID)
	if err != nil {
		return fmt.Errorf("reading %s: %w", beadID, err)
	}
	if issue == nil {
		return fmt.Errorf("reading %s: no such bead", beadID)
	}
	if beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
		return fmt.Errorf("refusing to requeue %s: it is %s, so it is not waiting to land", beadID, issue.Status)
	}
	if beads.HasLabel(issue, land.LabelReadyToLand) {
		return fmt.Errorf("refusing to requeue %s: it already carries %s and is queued to land", beadID, land.LabelReadyToLand)
	}
	if !beads.HasLabel(issue, land.LabelRework) {
		return fmt.Errorf("refusing to requeue %s: it is not in a rejected state (no %q label)", beadID, land.LabelRework)
	}
	note, ok := land.ParseRejectionNote(issue.Notes)
	if !ok {
		return fmt.Errorf("refusing to requeue %s: it is not in a rejected state (its notes carry no %s block)", beadID, land.MergeRejectionNoteMarker)
	}
	head := strings.TrimSpace(note.Head)
	if head == "" {
		return fmt.Errorf("refusing to requeue %s: the rejection names no head, so there is nothing to compare the branch against", beadID)
	}
	branch := strings.TrimSpace(note.Branch)
	if branch == "" {
		return fmt.Errorf("refusing to requeue %s: the rejection names no branch", beadID)
	}

	ref := "refs/heads/" + branch
	refs, err := remote.ListRemoteRefsWithHashes(remoteName, ref)
	if err != nil {
		return fmt.Errorf("reading %s from %s: %w", ref, remoteName, err)
	}
	tip := ""
	for _, r := range refs {
		if r.Name == ref {
			tip = strings.TrimSpace(r.Hash)
			break
		}
	}
	if tip == "" {
		return fmt.Errorf("refusing to requeue %s: branch %s is not on %s", beadID, branch, remoteName)
	}
	if !land.HeadsEqual(tip, head) {
		return fmt.Errorf("refusing to requeue %s: the head changed since the rejection — %s holds %s but the rejection names %s.\n"+
			"A changed head is a normal resubmission: run gt done on the branch (it re-gates and re-pushes) instead.",
			beadID, branch, shortSHA(tip), shortSHA(head))
	}

	if err := bd.Update(beadID, beads.UpdateOptions{
		AddLabels:    []string{land.LabelReadyToLand},
		RemoveLabels: []string{land.LabelRework},
	}); err != nil {
		return fmt.Errorf("restoring %s on %s: %w", land.LabelReadyToLand, beadID, err)
	}
	// bd reporting success is not proof the write persisted (GH#1945).
	after, err := bd.Show(beadID)
	if err != nil {
		return fmt.Errorf("reading %s back: %w", beadID, err)
	}
	if after == nil || !beads.HasLabel(after, land.LabelReadyToLand) || beads.HasLabel(after, land.LabelRework) {
		return fmt.Errorf("%s does not carry %s after the requeue", beadID, land.LabelReadyToLand)
	}

	comment := land.FormatRequeueComment(actor, branch, head, note.Attempt, reason)
	if err := bd.AddCommentAs(beadID, actor, comment); err != nil {
		return fmt.Errorf("the landing is re-queued but recording the requeue on %s failed: %w", beadID, err)
	}

	fmt.Fprintf(out, "%s Re-queued %s\n", style.Success.Render("✓"), beadID)
	fmt.Fprintf(out, "  Branch: %s @ %s (unchanged since the rejection)\n", branch, shortSHA(head))
	fmt.Fprintf(out, "  Reason: %s\n", reason)
	fmt.Fprintf(out, "  By:     %s\n", actor)
	fmt.Fprintf(out, "  %s\n", style.Dim.Render("The daemon's landing worker will gate and merge it again."))
	return nil
}
