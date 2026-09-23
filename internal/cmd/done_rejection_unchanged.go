package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/style"
)

// This file refuses a resubmission whose content is byte-identical to the
// attempt the refinery already rejected (gt-0jzd5): a polecat redispatched onto
// rejected work can run gt done without addressing a single finding, and the
// queue then re-gates and re-reviews the same diff to reach the same verdict.
//
// What was rejected is read from the rejected MR bead, never from the branch
// ref. The ref cannot answer the question: gt done pushes the branch on every
// submit, so after any later attempt origin/<branch> holds THAT attempt's
// content, and a rework that pushed a fix — the ordinary successful rework —
// would compare equal to itself and be refused as a no-op. The MR bead's
// commit_sha is the tip as submitted, which no later push rewrites.
//
// The comparison itself is patch-ids, not commits: patch-id is base-invariant
// and content-exact, so a rebase onto a newer target, an amended message and a
// re-split of the same hunks all leave it unchanged, while any real change to
// the diff moves it.

// rejectedReworkGit is the subset of *git.Git the check needs.
type rejectedReworkGit interface {
	Rev(ref string) (string, error)
	MergeBase(a, b string) (string, error)
	PatchID(base, head string) (string, error)
}

// rejectedAttempt is one rejection recorded in the source bead's notes: the
// attempt's branch, the MR that holds its tip, and the one-line reason.
type rejectedAttempt struct {
	branch  string
	mrID    string
	summary string
}

// rejectedAttemptsFromNotes returns the rejections recorded in a bead's notes.
// An attempt without an MR id is dropped: the MR is what says which content
// was rejected, so an attempt that does not name one cannot be checked against.
// Only the first Branch:/MR: line in a block is read — the block runs to the
// next marker, so later appended prose belongs to no rejection.
func rejectedAttemptsFromNotes(notes string) []rejectedAttempt {
	if !strings.Contains(notes, refinery.MergeRejectionNoteMarker) {
		return nil
	}
	var out []rejectedAttempt
	seen := make(map[string]bool)
	for _, block := range strings.Split(notes, refinery.MergeRejectionNoteMarker)[1:] {
		var a rejectedAttempt
		lines := strings.Split(block, "\n")
		a.summary = strings.TrimSpace(lines[0])
		for _, line := range lines[1:] {
			key, value, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			switch {
			case a.branch == "" && strings.EqualFold(strings.TrimSpace(key), "branch"):
				a.branch = strings.TrimPrefix(value, "refs/heads/")
			case a.mrID == "" && strings.EqualFold(strings.TrimSpace(key), "mr"):
				a.mrID = value
			}
		}
		// One MR is one attempt, however many times its rejection was
		// recorded: the second record adds a line to the refusal and a Dolt
		// read, not evidence.
		if a.mrID != "" && !seen[a.mrID] {
			seen[a.mrID] = true
			out = append(out, a)
		}
	}
	return out
}

// reportUnchangedSinceRejection refuses the submission when the branch's diff
// is byte-identical to a rejected attempt's, and returns nil otherwise.
//
// tipOf resolves an MR bead id to the tip that MR was submitted with. Every
// way of not knowing passes: a rejection without an MR record, an MR whose
// commit_sha is missing, a tip that is no longer an object in this repository.
// "Cannot tell" is not evidence that the content changed, but reading it as
// "identical" strands a polecat on work it cannot unblock, and the miss costs
// only the wasted gate run this guard exists to prevent — the state we are
// already in, not a new one.
func reportUnchangedSinceRejection(g rejectedReworkGit, notes, issueID, target string, tipOf func(mrID string) (string, bool)) error {
	attempts := rejectedAttemptsFromNotes(notes)
	if len(attempts) == 0 {
		return nil
	}

	localSHA, err := g.Rev("HEAD")
	if err != nil {
		style.PrintWarning("could not resolve HEAD for the rejected-rework check: %v", err)
		return nil
	}
	localPatchID, err := patchIDAgainst(g, target, localSHA)
	if err != nil {
		style.PrintWarning("could not compute the branch's patch-id against %s: %v", target, err)
		return nil
	}

	var unchanged []unchangedRejection
	for _, a := range attempts {
		sha, ok := tipOf(a.mrID)
		if !ok || strings.TrimSpace(sha) == "" {
			continue
		}
		id, err := patchIDAgainst(g, target, sha)
		if err != nil {
			continue
		}
		if id == localPatchID {
			unchanged = append(unchanged, unchangedRejection{attempt: a, sha: sha})
		}
	}
	if len(unchanged) == 0 {
		return nil
	}
	return unchangedSinceRejectionRefusal(issueID, target, localPatchID, unchanged)
}

// patchIDAgainst is the stable patch-id of the diff a tip carries against its
// own merge base with target — the diff the MR would have merged.
func patchIDAgainst(g rejectedReworkGit, target, sha string) (string, error) {
	base, err := g.MergeBase(target, sha)
	if err != nil {
		return "", err
	}
	return g.PatchID(base, sha)
}

// rejectedTipFromMR resolves the tip an MR bead was submitted with, which is
// the content that MR's gate judged (beads.MRFields.CommitSHA).
func rejectedTipFromMR(bd *beads.Beads, mrID string) (string, bool) {
	if bd == nil {
		return "", false
	}
	mr, err := bd.Show(mrID)
	if err != nil {
		style.PrintWarning("could not read rejected MR %s: %v", mrID, err)
		return "", false
	}
	if mr == nil {
		return "", false
	}
	fields := beads.ParseMRFields(mr)
	if fields == nil || strings.TrimSpace(fields.CommitSHA) == "" {
		return "", false
	}
	return fields.CommitSHA, true
}

// unchangedRejection is one rejected attempt whose content the branch still
// carries.
type unchangedRejection struct {
	attempt rejectedAttempt
	sha     string
}

// unchangedSinceRejectionRefusal builds the refusal for a resubmission that
// changes nothing. The message names no flag: agents read refusals and bypass
// them, and there is no flag here to reach for — the diff has to change.
func unchangedSinceRejectionRefusal(issueID, target, patchID string, found []unchangedRejection) error {
	var b strings.Builder
	b.WriteString("refusing to submit: no change since the rejection: address the findings first\n\n")
	fmt.Fprintf(&b, "Your branch carries the change-set the refinery already rejected — same patch-id\n"+
		"%s on both sides. Only the diff decides this: a rebase, an amended\n"+
		"message and re-split commits all leave it unchanged.\n\n", patchID)
	b.WriteString("Rejected attempt(s) holding this identical diff:\n")
	for _, f := range found {
		fmt.Fprintf(&b, "  %s", f.attempt.mrID)
		if f.attempt.branch != "" {
			fmt.Fprintf(&b, " (%s)", f.attempt.branch)
		}
		fmt.Fprintf(&b, " — tip %s\n", shortSHA(f.sha))
		if f.attempt.summary != "" {
			fmt.Fprintf(&b, "      %s\n", f.attempt.summary)
		}
	}
	b.WriteString("\nThe gate re-runs against the same content and returns the same findings, so\n" +
		"submitting this spends a gate run and a review to learn nothing new.\n\n")
	if issueID != "" {
		fmt.Fprintf(&b, "Read the findings in full:\n  bd show %s\n\n", issueID)
	}
	b.WriteString("Change the content so it answers them, then re-run gt done. What you would submit:\n")
	fmt.Fprintf(&b, "  git diff %s...HEAD\n\n", target)
	b.WriteString("If the findings are wrong and this diff is the right answer, escalate to your\n" +
		"witness rather than resubmitting it unchanged.")
	return fmt.Errorf("%s", b.String())
}
