package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
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

// requireRevertOverrideAuthorization enforces that an agent invoking
// --allow-reverts names the bead recording the mayor ruling that authorized
// it (gt-0wy03 attempt 2): commit-message text can no longer excuse a
// revert, so the one remaining override must be traceable to an explicit
// ruling the same way gt-61x requires for a forced Dolt cleanup.
//
// actor is never a genuine human at a plain terminal: resolveDonePolecatWorktree
// already refuses to let gt done run at all unless BD_ACTOR names a validated
// polecat identity, so by the time this gate runs the caller is proven to be
// an agent. An empty actor here means only that resolveDoneAgentIdentity's own
// role detection failed (gt-0wy03 attempt 3) — GetRoleWithContext errored, or
// returned RoleUnknown — not that a human is running the command. Treating
// that failure as the human exemption let the override through exactly as
// success would; it must fail closed instead.
func requireRevertOverrideAuthorization(actor, authorizedBy string) error {
	if actor == "" {
		return fmt.Errorf(`gt done --allow-reverts cannot verify the requesting agent's identity (role detection failed) (gt-0wy03)

This is the agent path — gt done already refused to run unless BD_ACTOR names a
validated polecat, so a failure here is a detection problem, not a human
running the command, and must refuse rather than proceed as if unattended.
Re-run gt done; if this persists, escalate to the mayor.`)
	}
	if authorizedBy != "" {
		return nil
	}
	return fmt.Errorf(`agent actor %q may not run 'gt done --allow-reverts' without recorded authorization (gt-0wy03)

Reverting merged work requires an authorization bead:
  1. Escalate to the mayor and get an explicit ruling
  2. Reference the bead that records the decision:
       gt done --allow-reverts --allow-reverts-authorized-by <bead-id>

The override will be logged as a comment on that bead`, actor)
}

// recordRevertOverride writes the audit trail --allow-reverts must leave
// (gt-0wy03 attempt 2): the ruling bead gets a permanent comment naming who
// invoked the override and which commits it let through. addComment is
// injected so this is testable without a live beads store; it fails closed —
// if the record cannot be written, the override must not proceed, mirroring
// cleanupAuditor.recordIntent (gt-87a).
func recordRevertOverride(g *git.Git, addComment func(id, text string) error, actor, authorizedBy, target string) error {
	found, detectErr := git.DetectRevertedMerges(g, target, "HEAD")
	var detail string
	switch {
	case detectErr != nil:
		detail = fmt.Sprintf("could not determine (error: %v)", detectErr)
	case len(found) == 0:
		detail = "none detected"
	default:
		commits := make([]string, 0, len(found))
		for _, f := range found {
			commits = append(commits, shortSHA(f.Commit))
		}
		detail = strings.Join(commits, ", ")
	}
	comment := fmt.Sprintf("gt done --allow-reverts by %s: bypassing merged-work revert check against %s; reverted commit(s): %s",
		actor, target, detail)
	if err := addComment(authorizedBy, comment); err != nil {
		return fmt.Errorf("cannot write --allow-reverts audit record to %s — refusing to submit (gt-0wy03): %w", authorizedBy, err)
	}
	return nil
}

// verifyRevertOverrideBead checks that the --allow-reverts-authorized-by bead
// is a genuine mayor ruling for THIS override, not merely some bead ID the
// requesting agent typed (gt-0wy03 attempt 3): requireRevertOverrideAuthorization
// only checks that a bead ID was supplied, so nothing previously stopped a
// polecat pointing it at its own hooked issue and approving itself, the same
// self-authorization hole gt-2oy closed for a forced Dolt cleanup. The bead
// must exist, must have been created by the mayor, and must name both this
// branch and every commit the override is about to let through — so one
// ruling bead cannot be reused to cover a different branch or a different
// revert than the one it was written for.
func verifyRevertOverrideBead(shower beads.IssueShower, g *git.Git, target, id, branch string) error {
	issue, err := shower.Show(id)
	if err != nil {
		return fmt.Errorf("authorization bead %s not found — gt done --allow-reverts requires a real mayor ruling bead (gt-0wy03): %w", id, err)
	}
	if issue == nil {
		return fmt.Errorf("authorization bead %s not found — gt done --allow-reverts requires a real mayor ruling bead (gt-0wy03)", id)
	}
	if normalizeAddress(issue.CreatedBy) != "mayor" {
		return fmt.Errorf("authorization bead %s was created by %q, not the mayor — an agent may not approve its own --allow-reverts override (gt-0wy03)", id, issue.CreatedBy)
	}
	text := revertOverrideBeadText(issue)
	if !strings.Contains(text, branch) {
		return fmt.Errorf("authorization bead %s does not name branch %q — the mayor ruling must name the branch it authorizes (gt-0wy03)", id, branch)
	}
	found, err := git.DetectRevertedMerges(g, target, "HEAD")
	if err != nil {
		return fmt.Errorf("cannot verify authorization bead %s against the branch's reverted commits: %w", id, err)
	}
	for _, f := range found {
		if !strings.Contains(text, f.Commit) && !strings.Contains(text, shortSHA(f.Commit)) {
			return fmt.Errorf("authorization bead %s does not name reverted commit %s — the mayor ruling must name every commit it authorizes reverting (gt-0wy03)", id, shortSHA(f.Commit))
		}
	}
	return nil
}

// revertOverrideBeadText concatenates the fields an authorization bead might
// name its branch and commits in: description and notes cover a bead written
// up front, comments cover one a mayor annotates after the fact.
func revertOverrideBeadText(issue *beads.Issue) string {
	var b strings.Builder
	b.WriteString(issue.Description)
	b.WriteByte('\n')
	b.WriteString(issue.Notes)
	b.WriteByte('\n')
	for _, c := range issue.Comments {
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return b.String()
}
