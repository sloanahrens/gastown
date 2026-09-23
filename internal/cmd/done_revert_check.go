package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
)

// This file guards gt done against a branch whose commit REVERTS work already
// merged into the target. That is not a hypothetical: two local-coder polecat
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

// reportRevertedMerges prints the branch's diff against target and refuses the
// submission when the branch undoes merged work.
//
// The stat is printed on every call, refusals included, because it is the one
// view that makes the failure self-evident to the polecat that caused it: a
// correct branch lists the polecat's own files, and the two branches in gt-63sz
// listed 9 and 17 files each, nearly none of them the author's.
func reportRevertedMerges(g *git.Git, target string) error {
	if stat, err := g.DiffStatThreeDot(target, "HEAD"); err != nil {
		style.PrintWarning("could not compute branch diff against %s: %v", target, err)
	} else if strings.TrimSpace(stat) != "" {
		fmt.Printf("  Branch diff vs %s:\n", target)
		for _, line := range strings.Split(strings.TrimSpace(stat), "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}

	found, err := git.DetectRevertedMerges(g, target, "HEAD")
	if err != nil {
		// Refuse rather than submit: this check exists because a branch that
		// reverts merged work is silently accepted by everything downstream,
		// and a check that cannot run must not read as a check that passed.
		return fmt.Errorf("cannot verify branch against %s: %w\n"+
			"Refusing to submit rather than risk reverting merged work. "+
			"Run `git fetch origin && git rebase %s`, then re-run gt done.", target, err, target)
	}
	if len(found) == 0 {
		return nil
	}
	return revertedMergeRefusal(g, target, found)
}

// revertedMergeRefusal builds the refusal error for a branch that undoes merged
// work. The message deliberately does not name the flag that overrides it:
// agents read refusal text and self-bypass, so the text says what to do about
// the branch instead.
func revertedMergeRefusal(g *git.Git, target string, found []git.RevertedMerge) error {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to submit: this branch undoes work already merged to %s\n\n", target)
	fmt.Fprintf(&b, "These commits on %s are undone by your branch:\n", target)
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
	b.WriteString("\nYour working tree is older than the base your commit claims to sit on, " +
		"so the commit records (your tree) - (that base): a revert of every commit merged " +
		"in between, plus your own change. Submitting it would delete other people's merged work.\n\n")
	fmt.Fprintf(&b, "Integrate with:\n"+
		"  git fetch origin && git rebase %s\n\n", target)
	fmt.Fprintf(&b, "Then confirm the branch lists only YOUR files and re-run gt done:\n"+
		"  git diff --stat %s...HEAD\n\n", target)
	b.WriteString("If you are certain this removal is intentional and not a rebase artifact, " +
		"do not try to talk your way past this check: no commit message can authorize it. " +
		"Escalate to the mayor for a ruling; only that can authorize submitting it.")
	return fmt.Errorf("%s", b.String())
}

// requireNonPolecatCloneForRevertOverride refuses --allow-reverts whenever
// cwd resolves under a "*/polecats/*" path component (gt-0wy03 AC1). A bead
// field or an env var (GT_ROLE, BD_ACTOR, CreatedBy) is written by the same
// agent the override would let bypass its own review, so none of them can
// authorize anything — three straight rounds of review found a new way to
// spoof each one. The worktree path is not spoofable the same way: reaching
// a non-polecat clone means actually running the command from one, such as
// mayor/rig, not merely setting a variable. This is a pure filesystem check
// with no identity dependency, so it holds even if gt done's own
// polecat-worktree enforcement (resolveDonePolecatWorktree) ever changes.
//
// The one sanctioned override path is a mayor-side action from a non-polecat
// clone: gt mq submit --allow-reverts --branch <branch> --reason <why>.
//
// cwd is resolved to an absolute, symlink-free path before the check: a
// relative path or a symlink pointing out of a polecat worktree must not
// read as a non-polecat clone. A path that cannot be resolved refuses
// closed rather than falling back to the unresolved string — an override
// gate must not proceed on a path it could not actually verify.
func requireNonPolecatCloneForRevertOverride(cwd string) error {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("--allow-reverts cannot resolve the working directory (refusing rather than risk a polecat worktree going undetected): %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("--allow-reverts cannot resolve the working directory's real path (refusing rather than risk a polecat worktree going undetected): %w", err)
	}
	for _, part := range strings.Split(filepath.ToSlash(resolved), "/") {
		if part == "polecats" {
			return fmt.Errorf(`--allow-reverts is refused from a polecat worktree (gt-0wy03)

No env var or bead can authorize reverting merged work: both are written by
the same agent the override would let bypass its own review. The override
exists only as a mayor-side action, run from a non-polecat clone:

  gt mq submit --allow-reverts --branch <branch> --reason <why>

Escalate to the mayor for a ruling if you believe this branch's removal is
intentional and not a rebase artifact.`)
		}
	}
	return nil
}
