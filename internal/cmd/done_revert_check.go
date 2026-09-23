package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
)

// This file warns gt done's caller about a branch whose commit REVERTS work
// already merged into the target — advisory only (gt-0wy03 REDESIGN); the
// refinery's pre-merge gate (internal/refinery/revert_gate.go) is what
// actually refuses one. That is not a hypothetical: two local-coder polecat
// MRs in one night (gt-wisp-hrau, gt-wisp-p7nl) each submitted an unrelated fix
// wrapped around a full revert of everything merged since their worktree was
// cut — 17 files +972/-1711 and 9 files +106/-472 (gt-63sz).
//
// The mechanism is a squash-onto-fresh-base habit, not staleness of the
// COMMIT graph: the polecat runs
//
//	git add -A; git reset --soft origin/main; git commit
//
// over a checkout that is hours old. `reset --soft` moves HEAD to the current
// remote tip while leaving the index and working tree exactly as the old
// checkout had them, so the following commit records (old tree) - (new tip) —
// a revert of every commit merged in between, hidden inside one commit on a
// perfectly fresh base.
//
// Nothing about ancestry can see this. `git merge-base origin/main HEAD` is
// origin/main itself, the branch is exactly one commit ahead, and the commit
// message describes the intended work. Only per-file CONTENT shows it: the
// branch carries the pre-merge blob of paths it never meant to touch. So the
// check below reconstructs, for each path, the blobs on all four sides of the
// question (the target commit's parent, the target commit, the target tip, and
// the branch tip) and asks whether the branch undoes a live change.

// revertReportLimit caps how many reverted changes are listed before the message
// summarizes the rest. The count is always reported in full.
const revertReportLimit = 8

// revertPathsPerCommit caps the paths listed under a single reverted commit.
const revertPathsPerCommit = 4

// reportRevertedMerges prints the branch's diff against target and warns when
// the branch undoes merged work. This is advisory only (gt-0wy03 REDESIGN):
// the authoritative, fail-closed check is the refinery's pre-merge gate
// (internal/refinery/revert_gate.go), the single choke point covering gt
// done's own push, gt mq submit, and both the single-MR and batch merge
// paths. A client-side refusal here could only ever block the polecat that
// tripped it, never the branch reaching main by another route, so gt done no
// longer treats this as a submission blocker.
//
// The stat is printed on every call because it is the one view that makes a
// real incident self-evident to the polecat that caused it: a correct branch
// lists the polecat's own files, and the two branches in gt-63sz listed 9 and
// 17 files each, nearly none of them the author's.
func reportRevertedMerges(g *git.Git, target string) {
	if stat, err := g.DiffStatThreeDot(target, "HEAD"); err != nil {
		style.PrintWarning("could not compute branch diff against %s: %v", target, err)
	} else if strings.TrimSpace(stat) != "" {
		fmt.Printf("  Branch diff vs %s:\n", target)
		for _, line := range strings.Split(strings.TrimSpace(stat), "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}

	found, err := git.DetectRevertedMerges(g, target, "HEAD", "HEAD")
	if err != nil {
		style.PrintWarning("could not verify branch against %s for reverted merged work: %v", target, err)
		return
	}
	if len(found) == 0 {
		return
	}
	style.PrintWarning("%s", revertedMergeWarning(g, target, found))
}

// revertedMergeWarning builds the advisory text for a branch that appears to
// undo merged work. It names the mayor escalation path rather than any flag
// or env var: the only override is a mayor-authored file the refinery's gate
// reads, and nothing on the polecat's side can grant one.
func revertedMergeWarning(g *git.Git, target string, found []git.RevertedMerge) string {
	var b strings.Builder
	fmt.Fprintf(&b, "this branch appears to undo work already merged to %s\n\n", target)
	fmt.Fprintf(&b, "These commits on %s look undone by your branch:\n", target)
	for i, f := range found {
		if i == revertReportLimit {
			fmt.Fprintf(&b, "  ... and %d more\n", len(found)-revertReportLimit)
			break
		}
		subject, err := g.CommitSubject(f.Commit)
		if err != nil || subject == "" {
			subject = "(subject unavailable)"
		}
		fmt.Fprintf(&b, "  %s %s\n", shortSHA(f.Commit), subject)
		for j, path := range f.Paths {
			if j == revertPathsPerCommit {
				fmt.Fprintf(&b, "      ... and %d more paths\n", len(f.Paths)-revertPathsPerCommit)
				break
			}
			fmt.Fprintf(&b, "      undoes: %s\n", path)
		}
	}
	b.WriteString("\nIf your working tree is older than the base your commit claims to sit on, " +
		"the commit records (your tree) - (that base): a revert of every commit merged " +
		"in between, plus your own change. Rebase before resubmitting:\n" +
		fmt.Sprintf("  git fetch origin && git rebase %s\n\n", target) +
		fmt.Sprintf("Then confirm the branch lists only YOUR files: git diff --stat %s...HEAD\n\n", target) +
		"The refinery's merge gate makes the authoritative call before landing this branch. " +
		"If this is a genuine relocation rather than a real revert, escalate to the mayor — " +
		"only a mayor-authored override can let it through.")
	return b.String()
}
