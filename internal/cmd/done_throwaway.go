package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// This file guards gt done against a branch that would ADD throwaway files to
// the target: scratch and diagnostic files a polecat wrote to poke at a live
// system, /tmp copies, editor backups, patch leftovers (gt-ozo4).
//
// The polecat usually never committed them. checkpoint_dog and gt done's own
// gt-pvx safety net both run `git add -A` on live worktrees, and gt done
// collapses the branch into one commit carrying HEAD's tree, so a scratch file
// snapshotted between two dog cycles is submitted even after the polecat
// deletes it. Squashing rewrites commit messages, not content: gt-3wf and its
// siblings fixed the messages, this fixes the content. Both staging sites now
// skip these paths, so this gate is the backstop for a polecat adding them
// itself, a branch carrying a checkpoint from before that policy, and a path
// shape the policy does not recognize.

// reportThrowawayPaths refuses the submission when the branch adds a throwaway
// path to the target, and fails closed when it cannot tell. It runs on both
// submission shapes — squashed and pushed-as-is — because the branch reaches
// the merge queue either way.
func reportThrowawayPaths(g *git.Git, baseRef string) error {
	found, err := checkpoint.AddedThrowawayPaths(g.WorkDir(), baseRef, "HEAD")
	if err != nil {
		// Refuse rather than submit: the files this check exists to keep off
		// main arrive silently, so a check that could not run must not read as
		// a check that passed.
		return fmt.Errorf("cannot check branch for throwaway files: %w\n"+
			"Refusing to submit rather than risk landing scratch files on %s. "+
			"Run `git fetch origin && git rebase %s`, then re-run gt done.", err, baseRef, baseRef)
	}
	if len(found) == 0 {
		return nil
	}
	return throwawayRefusalError(baseRef, found)
}

// throwawayRefusalError builds the refusal for a branch that would add throwaway
// files. Like revertedMergeRefusal it does not name the flag that overrides it:
// agents read refusal text and self-bypass, so the text says what to do about
// the branch instead.
func throwawayRefusalError(baseRef string, found []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to submit: this branch would add %d throwaway file(s) to %s\n\n", len(found), baseRef)
	for _, path := range found {
		fmt.Fprintf(&b, "  %s\n", path)
	}
	b.WriteString("\nScratch/diagnostic files, /tmp copies, editor backups and patch leftovers are " +
		"kept off the target whatever put them on the branch. The usual way in is a " +
		"\"WIP: checkpoint (auto)\" commit: the checkpoint dog runs `git add -A` on live " +
		"worktrees, so a file that existed between two dog cycles was snapshotted, and " +
		"deleting it afterwards does not remove it. gt done collapses the branch into one " +
		"commit carrying HEAD's tree, so the file has to leave the branch itself, not just " +
		"the working tree.\n\n")
	quoted := make([]string, len(found))
	for i, path := range found {
		quoted[i] = config.ShellQuote(path)
	}
	fmt.Fprintf(&b, "Remove them and re-run gt done:\n"+
		"  git rm --cached -- %s && git commit -m \"remove throwaway files\"\n\n", strings.Join(quoted, " "))
	fmt.Fprintf(&b, "Then confirm the branch adds only files you meant to submit:\n"+
		"  git diff --name-only %s...HEAD", baseRef)
	return fmt.Errorf("%s", b.String())
}
