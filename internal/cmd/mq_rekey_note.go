package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	mqRekeyNoteLanded       string
	mqRekeyNoteSecondParent bool
	mqRekeyNoteReason       string
	mqRekeyNoteRequestedBy  string
	mqRekeyNoteTarget       string
)

var mqRekeyNoteCmd = &cobra.Command{
	Use:   "rekey-note <mr-id>",
	Short: "Backfill an om note onto a commit that already landed, by patch-id",
	Long: `Copy an MR's om verdict note onto the commit that actually landed.

When a merge lands before its note is reachable — the note keyed to a
rehearsal head the merge queue discarded, a non-fast-forward merge whose
note stayed on the branch tip — the landed commit is left with no proof and
the editorial-coverage check flags it. This command is the auditable
backfill for that: it finds the note the MR's own review wrote, proves by
patch-id that what landed is what was reviewed, and copies the note onto the
landed commit.

It refuses rather than guesses. The landed commit must be reachable from
origin/<target>, a note for the MR must exist under refs/notes/om, and its
patch-id must equal each target's own diff patch-id. Nothing is written when
any check fails — including when a target already carries another MR's
approve note for the same diff, since a notes ref holds one note per commit
and stamping there would replace that verdict's proof. A successful copy is
stamped rekeyed_from (the commit the verdict was originally written on),
backfill, backfill_reason, backfilled_by, backfilled_at and
patch_id_verified, then pushed to refs/notes/om.

The source note is found by the MR id inside the note, not in beads: MR beads
are wisps and are routinely reaped, while the note is the durable record.

A push failure is reported as a failure — the note exists locally but no
other clone (including the coverage check) can see it until the push lands.

Examples:
  gt mq rekey-note gt-wisp-c304 --landed 097ab8a --reason "non-ff merge landed before its note was copied"
  gt mq rekey-note gt-wisp-qb0y --landed 510bfa5 --second-parent --reason "second-parent copy did not run"
  gt mq rekey-note gt-wisp-24l --landed dfc8c98 --reason "reversed-parent merge: first-parent walk passes through the polecat side"`,
	Args: cobra.ExactArgs(1),
	RunE: runMQRekeyNote,
}

func init() {
	mqRekeyNoteCmd.Flags().StringVar(&mqRekeyNoteLanded, "landed", "", "Commit that landed on the target branch (required)")
	mqRekeyNoteCmd.Flags().BoolVar(&mqRekeyNoteSecondParent, "second-parent", false, "Also stamp the note on the landed merge commit's second parent")
	mqRekeyNoteCmd.Flags().StringVar(&mqRekeyNoteReason, "reason", "", "Why this backfill is legitimate (required; recorded on the note)")
	mqRekeyNoteCmd.Flags().StringVar(&mqRekeyNoteRequestedBy, "requested-by", "", "Who asked for the backfill (optional, recorded on the note)")
	mqRekeyNoteCmd.Flags().StringVar(&mqRekeyNoteTarget, "target", "", "Branch the commit landed on (default: the rig's remote default branch)")
	mqCmd.AddCommand(mqRekeyNoteCmd)
}

func runMQRekeyNote(_ *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, r, err := findCurrentRig(townRoot)
	if err != nil {
		return err
	}

	// Same clone gt mq review writes and pushes notes from, so the note
	// ref this reads is the one the reviews and the refinery actually use.
	repoDir := filepath.Join(r.Path, "refinery", "rig")
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		repoDir = filepath.Join(r.Path, "mayor", "rig")
	}
	g := git.NewGit(repoDir)

	target := strings.TrimSpace(mqRekeyNoteTarget)
	if target == "" {
		target = g.RemoteDefaultBranch()
	}

	result, err := editorial.RekeyNote(g, editorial.RekeyRequest{
		MR:           args[0],
		Landed:       mqRekeyNoteLanded,
		Target:       target,
		SecondParent: mqRekeyNoteSecondParent,
		Reason:       mqRekeyNoteReason,
		RequestedBy:  mqRekeyNoteRequestedBy,
	})
	if err != nil {
		// A push failure leaves the proof local-only: loud, because the
		// landed commit still reads as uncovered everywhere else.
		if result != nil && len(result.Written) > 0 && !result.Pushed {
			fmt.Fprintf(os.Stderr, "%s note written to refs/notes/%s in %s but NOT published\n",
				style.Bold.Render("✗"), editorial.NotesRef, repoDir)
		}
		return err
	}

	if len(result.Written) == 0 {
		fmt.Printf("%s Note already on every target for %s — republished refs/notes/%s\n",
			style.Dim.Render("○"), args[0], editorial.NotesRef)
	} else {
		fmt.Printf("%s Re-keyed note for %s\n", style.Bold.Render("✓"), args[0])
	}

	fmt.Printf("  Rig:     %s\n", rigName)
	fmt.Printf("  Source:  %s %s\n", shortSHA(result.SourceCommit), style.Dim.Render("(where the verdict was written)"))
	fmt.Printf("  PatchID: %s %s\n", result.PatchID, style.Dim.Render("(verified equal to every target's own diff)"))
	for _, t := range result.Targets {
		state := style.Dim.Render("already covered")
		for _, w := range result.Written {
			if w == t {
				state = style.Success.Render("stamped")
			}
		}
		fmt.Printf("  Target:  %s %s\n", shortSHA(t), state)
	}
	if result.Pushed {
		fmt.Printf("  %s Pushed refs/notes/%s to origin\n", style.Success.Render("✓"), editorial.NotesRef)
	}
	return nil
}
