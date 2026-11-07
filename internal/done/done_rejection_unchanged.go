package done

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
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
// The comparison itself is patch-ids, not commits: patch-id is base-invariant,
// so a rebase onto a newer target, an amended message and a re-split of the
// same hunks all leave it unchanged, while any change to the diff moves it.
// That last clause needs the verbatim form. Plain patch-id strips whitespace
// before hashing, so it reads a whitespace-only fix — the natural answer to a
// formatting finding — as no change at all and refuses it (gt-2colr).

// rejectedReworkGit is the subset of *git.Git the check needs.
type rejectedReworkGit interface {
	Rev(ref string) (string, error)
	MergeBase(a, b string) (string, error)
	PatchIDVerbatim(base, head string) (string, error)
}

// rejectedAttempt is one rejection recorded in the source bead's notes: the
// attempt's branch, the MR that holds its tip, and the one-line reason.
type rejectedAttempt struct {
	branch  string
	mrID    string
	summary string
	// kind is the failure class the rejection header names ("gate",
	// "conflict", "not_pushed", ...); empty when the rejection carries none.
	kind string
	// head is the rejected tip when the rejection names it (Land's Head:
	// line, ADR 0004); it is the content to compare, with no MR to look up.
	head string
	// targetTip is the target tip the rejected merged tree was built on, from
	// the note's Target tip: line. Empty when the note carries none, which
	// every rejection written before the line existed does.
	targetTip string
}

// rejectionKindsNotCausedByDiff are the rejection classes that say nothing
// about the content judged: the diff is fine, and the same diff can be the
// right resubmission. A conflict is against a target that has since moved (gt
// done has just rebased onto it), a not_pushed head is a push that did not
// land, an empty merge means the target already holds the content, and a
// policy refusal is about the bead's state (unchecked acceptance criteria).
// Refusing the identical diff for these strands a polecat on work it cannot
// unblock by editing. An unclassified or unknown class still blocks: only a
// class known to be off the diff is let through.
var rejectionKindsNotCausedByDiff = map[string]bool{
	"conflict":   true,
	"not_pushed": true,
	"empty":      true,
	"policy":     true,
	// A test stage over its timeout says nothing the author can fix by
	// editing (gt-b5ugw review).
	"timeout": true,
}

// rejectionKindFromSummary reads the class out of a rejection's first line,
// "(attempt N): <class> - <reason>" (land.FormatRejectionNote). A header with
// no class reads as "": its reason is free text, and the first word of free
// text is not a class.
func rejectionKindFromSummary(summary string) string {
	_, rest, found := strings.Cut(summary, "):")
	if !found {
		return ""
	}
	class, _, found := strings.Cut(rest, " - ")
	class = strings.ToLower(strings.TrimSpace(class))
	if !found || class == "" || strings.ContainsAny(class, " \t") {
		return ""
	}
	return class
}

// rejectionBlocksFromNotes returns every rejection block in a bead's notes, in
// the order they were written, including ones that name no content. The last
// block is the most recent rejection, whatever its content, so the caller
// needs the blocks before any are dropped. Only the first Branch:/MR:/Head:/
// Target tip: line in a block is read — the block runs to the next marker, so
// later appended prose belongs to no rejection.
func rejectionBlocksFromNotes(notes string) []rejectedAttempt {
	if !strings.Contains(notes, dispatch.MergeRejectionNoteMarker) {
		return nil
	}
	var out []rejectedAttempt
	for _, block := range strings.Split(notes, dispatch.MergeRejectionNoteMarker)[1:] {
		var a rejectedAttempt
		lines := strings.Split(block, "\n")
		a.summary = strings.TrimSpace(lines[0])
		a.kind = rejectionKindFromSummary(a.summary)
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
			case a.head == "" && strings.EqualFold(strings.TrimSpace(key), "head"):
				a.head = value
			case a.targetTip == "" && strings.EqualFold(strings.TrimSpace(key), "target tip"):
				a.targetTip = value
			}
		}
		out = append(out, a)
	}
	return out
}

// attemptKey names the content a rejection is about: its rejected Head: when
// the note carries one, else its MR id. A block naming neither has no key.
func attemptKey(a rejectedAttempt) string {
	if a.head != "" {
		return a.head
	}
	return a.mrID
}

// distinctAttempts keeps one attempt per rejected content: an MR (or a rejected
// head) is one attempt, however many times its rejection was recorded. An
// attempt that names neither a rejected Head: nor an MR id is dropped: one of
// them is what says which content was rejected.
func distinctAttempts(blocks []rejectedAttempt) []rejectedAttempt {
	var out []rejectedAttempt
	seen := make(map[string]bool)
	for _, a := range blocks {
		// The second record of an MR adds a line to the refusal and a Dolt
		// read, not evidence.
		key := attemptKey(a)
		if key != "" && !seen[key] {
			seen[key] = true
			out = append(out, a)
		}
	}
	return out
}

// reportUnchangedSinceRejection refuses the submission when the branch's diff
// is byte-identical to a rejected attempt's, and returns nil otherwise. An
// attempt rejected for a reason the diff did not cause
// (rejectionKindsNotCausedByDiff) is not compared at all.
//
// The one lift is a gate rejection the target has since moved past: the gate
// runs on the merged tree, so a new target tip is new evidence
// (gateRejectionOfTheseAttempts).
//
// tipOf resolves an MR bead id to the tip that MR was submitted with. Every
// way of not knowing passes: a rejection without an MR record, an MR whose
// commit_sha is missing, a tip that is no longer an object in this repository.
// "Cannot tell" is not evidence that the content changed, but reading it as
// "identical" strands a polecat on work it cannot unblock, and the miss costs
// only the wasted gate run this guard exists to prevent — the state we are
// already in, not a new one.
func reportUnchangedSinceRejection(g rejectedReworkGit, notes, issueID, target string, tipOf func(mrID string) (string, bool)) error {
	blocks := rejectionBlocksFromNotes(notes)
	if len(blocks) == 0 {
		return nil
	}
	attempts := distinctAttempts(blocks)

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
		if rejectionKindsNotCausedByDiff[a.kind] {
			continue
		}
		sha, ok := a.head, a.head != ""
		if !ok {
			sha, ok = tipOf(a.mrID)
		}
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
	if last := latestBlockNaming(blocks, unchanged); gateRejectionOfTheseAttempts(last, unchanged) {
		// The target is read only here, when the lift could apply: every other
		// refusal is decided without a git call.
		tip, err := g.Rev(target)
		if err != nil {
			return fmt.Errorf("refusing to submit: could not read %s to check whether it moved since the gate rejection: %w", target, err)
		}
		if tip != last.targetTip {
			return nil
		}
	}
	return unchangedSinceRejectionRefusal(issueID, target, localPatchID, unchanged)
}

// latestBlockNaming returns the most recent rejection block that names the
// content of one of the identical attempts. A block about other content, or
// one that names none, says nothing about this diff, so it does not decide the
// lift. The zero value is returned when no block matches; identical is never
// empty here, since each of its attempts came from one of these blocks.
func latestBlockNaming(blocks []rejectedAttempt, identical []unchangedRejection) rejectedAttempt {
	named := make(map[string]bool, len(identical))
	for _, u := range identical {
		named[attemptKey(u.attempt)] = true
	}
	var last rejectedAttempt
	for _, b := range blocks {
		if key := attemptKey(b); key != "" && named[key] {
			last = b
		}
	}
	return last
}

// gateRejectionOfTheseAttempts reports whether the most recent rejection of
// this content is a gate rejection that recorded the target tip it was built
// on, and every identical attempt is a gate rejection too. A review rejection
// of the same diff stays a refusal whatever the target did: a reviewer judged
// that content, and the target moving does not change the verdict. A record
// with no target tip stays a refusal, since there is nothing to show the
// target moved.
func gateRejectionOfTheseAttempts(last rejectedAttempt, identical []unchangedRejection) bool {
	if last.kind != "gate" || last.targetTip == "" {
		return false
	}
	for _, u := range identical {
		if u.attempt.kind != "gate" {
			return false
		}
	}
	return true
}

// patchIDAgainst is the patch-id of the diff a tip carries against its own
// merge base with target — the diff the MR would have merged, whitespace
// included, so a whitespace-only rework is not read as the rejected attempt.
func patchIDAgainst(g rejectedReworkGit, target, sha string) (string, error) {
	base, err := g.MergeBase(target, sha)
	if err != nil {
		return "", err
	}
	return g.PatchIDVerbatim(base, sha)
}

// rejectedTipFromMR resolves the tip an MR bead was submitted with, which is
// the content that MR's gate judged (beads.MRFields.CommitSHA).
func rejectedTipFromMR(bd beads.Client, mrID string) (string, bool) {
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

// unchangedSinceRejectionHeadline names what the identical rejections ask for.
// A gate rejection has no findings to address: the gate refused the merged
// diff, so the diff has to pass it.
func unchangedSinceRejectionHeadline(found []unchangedRejection) string {
	for _, f := range found {
		if f.attempt.kind != "gate" {
			return "refusing to submit: no change since the rejection: address the findings first"
		}
	}
	return "refusing to submit: no change since the rejection: make the gate pass first"
}

// unchangedSinceRejectionRefusal builds the refusal for a resubmission that
// changes nothing. The message names no flag: agents read refusals and bypass
// them, and there is no flag here to reach for — the diff has to change.
func unchangedSinceRejectionRefusal(issueID, target, patchID string, found []unchangedRejection) error {
	var b strings.Builder
	b.WriteString(unchangedSinceRejectionHeadline(found) + "\n\n")
	fmt.Fprintf(&b, "Your branch carries the change-set the refinery already rejected — same patch-id\n"+
		"%s on both sides. Only the diff decides this: a rebase, an amended\n"+
		"message and re-split commits all leave it unchanged.\n\n", patchID)
	b.WriteString("Rejected attempt(s) holding this identical diff:\n")
	for _, f := range found {
		fmt.Fprintf(&b, "  %s", f.attempt.mrID)
		if f.attempt.branch != "" {
			fmt.Fprintf(&b, " (%s)", f.attempt.branch)
		}
		fmt.Fprintf(&b, " — tip %s\n", ShortSHA(f.sha))
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
