package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// landTakeoverFlags are the flags gt land takeover reads.
var (
	landTakeoverBranch string
	landTakeoverTarget string
	landTakeoverForce  string
)

var landTakeoverCmd = &cobra.Command{
	Use:   "takeover <bead>",
	Short: "Land an escalated branch on the overseer's review of its head",
	Long: `Land an escalated branch on the overseer's review of its head.

The overseer takes over a bead the landing worker left for a person. Rather
than hand-writing the landing request, this one command does the whole
takeover for the head the overseer reviewed:

  1. writes the READY TO LAND block naming the branch's tip on origin,
     with the caller as its Worker and the submission time;
  2. writes the OVERSEER REVIEWED note for that tip;
  3. adds gt:overseer-reviewed and gt:ready-to-land, and removes
     gt:needs-overseer (and the other needs-person spellings) and rework.

It then prints the head it named, so the overseer can check it against the
head it reviewed.

It refuses, writing nothing, when the branch is not on origin, when the bead
is closed, when the bead is not escalated (unless --force names why), and when
the caller is the human's own identity. Any read it cannot make fails closed.

It never runs gt done, and it does not change the rig's landing rules.

Examples:
  gt land takeover gt-3e1z4 --branch polecat/mica/gt-3e1z4+abc
  gt land takeover gt-3e1z4 --branch polecat/mica/gt-3e1z4+abc --force "escalated by hand before the label existed"`,
	Args: cobra.ExactArgs(1),
	RunE: runLandTakeover,
}

func init() {
	landTakeoverCmd.Flags().StringVar(&landTakeoverBranch, "branch", "", "the polecat branch on origin whose tip the overseer reviewed (required)")
	landTakeoverCmd.Flags().StringVar(&landTakeoverTarget, "target", "", "the branch to land on (default: the rig's default branch)")
	landTakeoverCmd.Flags().StringVar(&landTakeoverForce, "force", "", "take over a bead that is not escalated; the reason is required and printed")
	landCmd.AddCommand(landTakeoverCmd)
}

// landTakeoverBeads is the bead surface a takeover needs. *beads.Beads
// satisfies it; tests pass a fake.
type landTakeoverBeads interface {
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
}

// landTakeoverRemote reads a branch's tip from a remote. *git.Git satisfies it.
type landTakeoverRemote interface {
	ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error)
}

// landTakeoverHost is what gt land takeover reads from its process: the town
// it runs in, its working directory and environment, the rig's landing remote,
// and the rig's repository and bead store. Tests pass fakes for each.
type landTakeoverHost struct {
	findTown      func() (string, error)
	getwd         func() (string, error)
	getenv        func(string) string
	landingRemote func(townRoot, rigName string) (string, error)
	repo          func(townRoot, rigName string) landTakeoverRemote
	beads         func(cwd, beadID string) landTakeoverBeads
}

// landTakeoverProcessHost is the host a real gt land takeover runs against.
func landTakeoverProcessHost() landTakeoverHost {
	return landTakeoverHost{
		findTown:      workspace.FindFromCwdOrError,
		getwd:         os.Getwd,
		getenv:        os.Getenv,
		landingRemote: rig.ResolveLandingRemote,
		repo: func(townRoot, rigName string) landTakeoverRemote {
			return git.NewGit(filepath.Join(townRoot, rigName, ".repo.git"))
		},
		beads: func(cwd, beadID string) landTakeoverBeads {
			return beads.NewWithBeadsDir(cwd, beads.ResolveBeadsDirForID(beads.ResolveBeadsDir(cwd), beadID))
		},
	}
}

func runLandTakeover(cmd *cobra.Command, args []string) error {
	return landTakeoverCommand(cmd.OutOrStdout(), args[0], landTakeoverBranch, landTakeoverTarget, landTakeoverForce, landTakeoverProcessHost())
}

// landTakeoverCommand resolves what the takeover needs from host and runs it.
// Each resolution is a read, so every failure here comes before the first write.
func landTakeoverCommand(out io.Writer, beadID, branch, targetFlag, force string, host landTakeoverHost) error {
	beadID = strings.TrimSpace(beadID)
	townRoot, err := host.findTown()
	if err != nil {
		return fmt.Errorf("resolving the town: %w", err)
	}
	rigName := resolveRigForBead(townRoot, beadID)
	if rigName == "" {
		return fmt.Errorf("cannot resolve the rig that owns %s: its ID prefix maps to no rig", beadID)
	}
	remoteName, err := host.landingRemote(townRoot, rigName)
	if err != nil {
		return err
	}
	cwd, err := host.getwd()
	if err != nil {
		return fmt.Errorf("resolving the working directory: %w", err)
	}
	human, err := loadTakeoverHuman(townRoot)
	if err != nil {
		return err
	}
	target, err := takeoverTarget(townRoot, rigName, targetFlag)
	if err != nil {
		return err
	}
	actor, err := takeoverActor(host.getenv)
	if err != nil {
		return err
	}
	return takeOverLanding(host.beads(cwd, beadID), host.repo(townRoot, rigName), remoteName, takeoverRequest{
		BeadID: beadID, Branch: branch, Target: target,
		Actor: actor, Human: human, Force: force,
	}, out)
}

// takeoverRequest is one gt land takeover: the bead, the branch on origin, the
// target to land on, the caller, the human's identity and the --force reason.
type takeoverRequest struct {
	BeadID string
	Branch string
	Target string
	Actor  string
	Human  string
	Force  string
}

// loadTakeoverHuman reads the town's human identity. A town whose identity
// cannot be read is an error, never an empty name: with no name the command
// could not tell the human's submission from the caller's.
func loadTakeoverHuman(townRoot string) (string, error) {
	path := config.OverseerConfigPath(townRoot)
	cfg, err := config.LoadOverseerConfig(path)
	if err != nil {
		return "", fmt.Errorf("refusing takeover: the town's human identity cannot be read from %s, so the command cannot tell the human from the caller: %w", path, err)
	}
	return cfg.Name, nil
}

// takeoverTarget is the branch the takeover lands on: --target when given,
// else the rig's default branch, which gt done also falls back to. A rig
// config that cannot be read is an error, since it names the target.
func takeoverTarget(townRoot, rigName, flag string) (string, error) {
	if flag = strings.TrimSpace(flag); flag != "" {
		return flag, nil
	}
	cfg, err := rig.LoadRigConfigIfPresent(filepath.Join(townRoot, rigName))
	if err != nil {
		return "", fmt.Errorf("refusing takeover: the rig's default branch cannot be read (pass --target to name it): %w", err)
	}
	if cfg != nil && cfg.DefaultBranch != "" {
		return cfg.DefaultBranch, nil
	}
	return "main", nil
}

// takeoverActor names the caller for the READY TO LAND block's Worker line: the
// crew identity the session runs as. A takeover is the overseer's act, so a
// caller with no crew identity is refused rather than given a placeholder.
func takeoverActor(getenv func(string) string) (string, error) {
	if actor := done.CrewActorFromEnv(getenv); actor != "" {
		return actor, nil
	}
	return "", errors.New("refusing takeover: cannot name the caller; run it from a crew session (BEADS_ACTOR or GT_ROLE)")
}

// takeOverLanding is gt land takeover's decision, over the interfaces so a test
// drives it without a town, a remote or a live bd. Every refusal comes before
// the first write. The writes then go in order: the READY TO LAND block, the
// OVERSEER REVIEWED note, and last the labels, so the landing worker never sees
// the ready label without the block and the waiver it reads beside it.
func takeOverLanding(bd landTakeoverBeads, remote landTakeoverRemote, remoteName string, req takeoverRequest, out io.Writer) error {
	branch := strings.TrimSpace(req.Branch)
	target := strings.TrimSpace(req.Target)
	actor := strings.TrimSpace(req.Actor)
	human := strings.TrimSpace(req.Human)
	force := strings.TrimSpace(req.Force)
	switch {
	case branch == "":
		return errors.New("refusing takeover: --branch names no branch")
	case target == "":
		return fmt.Errorf("refusing takeover of %s: the target branch is empty", req.BeadID)
	case human == "":
		return fmt.Errorf("refusing takeover of %s: the town's human identity is empty, so the command cannot tell the human from the caller", req.BeadID)
	case actor == "":
		return fmt.Errorf("refusing takeover of %s: the caller names no identity", req.BeadID)
	case land.IsHumanActor(actor, human):
		return fmt.Errorf("refusing takeover of %s: the caller %q is the human's own identity; an agent takes the bead over, and the human lands it by hand", req.BeadID, actor)
	}

	issue, err := bd.Show(req.BeadID)
	if err != nil {
		return fmt.Errorf("reading %s: %w", req.BeadID, err)
	}
	if issue == nil {
		return fmt.Errorf("reading %s: no such bead", req.BeadID)
	}
	if beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
		return fmt.Errorf("refusing takeover of %s: it is %s, so it is not waiting to land", req.BeadID, issue.Status)
	}
	forced := !land.HasNeedsPerson(issue)
	if forced && force == "" {
		return fmt.Errorf("refusing takeover of %s: it is not escalated (no %s label); pass --force <reason> to take it over anyway", req.BeadID, land.LabelNeedsOverseer)
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
		return fmt.Errorf("refusing takeover of %s: branch %s is not on %s", req.BeadID, branch, remoteName)
	}

	w := land.Work{BeadID: req.BeadID, Branch: branch, Head: tip, Target: target, Worker: actor, Submitted: time.Now().UTC()}
	if err := bd.Update(req.BeadID, beads.UpdateOptions{AppendNotes: land.FormatReadyNote(w)}); err != nil {
		return fmt.Errorf("writing the %s block on %s: %w", land.ReadyNoteMarker, req.BeadID, err)
	}
	if err := bd.Update(req.BeadID, beads.UpdateOptions{AppendNotes: land.FormatOverseerReviewedNote(tip)}); err != nil {
		return fmt.Errorf("writing the %s note on %s: %w", land.OverseerReviewedMarker, req.BeadID, err)
	}
	if err := bd.Update(req.BeadID, beads.UpdateOptions{
		AddLabels:    []string{land.LabelOverseerReviewed, land.LabelReadyToLand},
		RemoveLabels: []string{land.LabelNeedsOverseer, land.LabelNeedsHuman, land.LabelNeedsOperator, land.LabelRework},
	}); err != nil {
		return fmt.Errorf("setting the takeover labels on %s; its notes already carry the block and the waiver, so re-run the takeover: %w", req.BeadID, err)
	}
	// bd reporting success is not proof the write persisted (GH#1945).
	after, err := bd.Show(req.BeadID)
	if err != nil {
		return fmt.Errorf("reading %s back: %w", req.BeadID, err)
	}
	if after == nil || !beads.HasLabel(after, land.LabelReadyToLand) || !land.OverseerReviewed(after, tip) ||
		beads.HasLabel(after, land.LabelRework) || land.HasNeedsPerson(after) {
		return fmt.Errorf("%s does not read back as taken over after the writes (%s, %s, the takeover labels); re-run the takeover", req.BeadID, land.ReadyNoteMarker, land.OverseerReviewedMarker)
	}

	if forced {
		style.FprintWarning(out, "%s was not escalated; taking it over anyway: %s", req.BeadID, force)
	}
	fmt.Fprintf(out, "%s Took over %s\n", style.Success.Render("✓"), req.BeadID)
	fmt.Fprintf(out, "  Branch: %s\n", branch)
	fmt.Fprintf(out, "  Head:   %s\n", tip)
	fmt.Fprintf(out, "  Target: %s\n", target)
	fmt.Fprintf(out, "  By:     %s\n", actor)
	fmt.Fprintf(out, "  %s\n", style.Dim.Render("The daemon's landing worker will review and merge this head."))
	return nil
}
