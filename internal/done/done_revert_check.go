package done

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/attention"
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

// deletesBySpecLabel marks a bead whose own description names the files its
// branch is meant to delete: a directed deletion, not a stale tree. The label
// alone waives nothing, because the author of a bead can label it (gt-b8f9z).
const deletesBySpecLabel = "deletes-by-spec"

// reportRevertedMerges prints the branch's diff against target and refuses the
// submission when the branch undoes merged work.
//
// The stat is printed on every call, refusals included, because it is the one
// view that makes the failure self-evident to the polecat that caused it: a
// correct branch lists the polecat's own files, and the two branches in gt-63sz
// listed 9 and 17 files each, nearly none of them the author's.
//
// Changes the branch merely relocated are printed and accepted: their code is
// still in the tree, so there is nothing to refuse (gt-x748o).
func reportRevertedMerges(g *git.Git, target string) error {
	return reportRevertedMergesWith(g, func() (git.RevertReport, error) { return git.DetectRevertedMerges(g, target, "HEAD") }, target)
}

// revertReportGit is what the revert report reads from the worktree.
type revertReportGit interface {
	DiffStatThreeDot(base, head string) (string, error)
	CommitSubject(rev string) (string, error)
	Rev(ref string) (string, error)
}

// reportRevertedMergesWith is reportRevertedMerges with the detection given,
// for a caller with no source bead to claim anything.
func reportRevertedMergesWith(g revertReportGit, detect func() (git.RevertReport, error), target string) error {
	_, err := revertedMergesReport(g, detect, target, revertWaiver{})
	return err
}

// revertedMergesReport is reportRevertedMergesWith returning the report it
// acted on, so the submit path can record what it refused. The waiver is the
// source bead's deletes-by-spec claim, if any.
func revertedMergesReport(g revertReportGit, detect func() (git.RevertReport, error), target string, waive revertWaiver) (git.RevertReport, error) {
	if stat, err := g.DiffStatThreeDot(target, "HEAD"); err != nil {
		style.PrintWarning("could not compute branch diff against %s: %v", target, err)
	} else if strings.TrimSpace(stat) != "" {
		fmt.Printf("  Branch diff vs %s:\n", target)
		for _, line := range strings.Split(strings.TrimSpace(stat), "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}

	report, err := detect()
	if err != nil {
		// Refuse rather than submit: this check exists because a branch that
		// reverts merged work is silently accepted by everything downstream,
		// and a check that cannot run must not read as a check that passed.
		return report, fmt.Errorf("cannot verify branch against %s: %w\n"+
			"Refusing to submit rather than risk reverting merged work. "+
			"Run `git fetch origin && git rebase %s`, then re-run gt done.", target, err, target)
	}
	if note := relocatedMergesNote(g, report.Relocated); note != "" {
		fmt.Print(note)
	}
	if len(report.Reverted) == 0 {
		return report, nil
	}
	if waive.claimed {
		_, unnamed := revertedPaths(report.Reverted, waive.description)
		if len(unnamed) == 0 {
			fmt.Printf("  %s\n\n", deletesBySpecNote(report.Reverted, waive.description))
			return report, nil
		}
		return report, revertedMergeRefusal(g, target, report.Reverted, unnamed)
	}
	return report, revertedMergeRefusal(g, target, report.Reverted, nil)
}

// revertWaiver is the source bead's deletes-by-spec claim. The label alone
// waives nothing: the description must also name every path the branch deletes,
// because the author of a bead can label it (gt-b8f9z).
type revertWaiver struct {
	claimed     bool
	description string
}

// waiverFromIssue reads the claim off the source bead. No bead, or no label,
// claims nothing.
func waiverFromIssue(issue *beads.Issue) revertWaiver {
	if issue == nil || !beads.HasLabel(issue, deletesBySpecLabel) {
		return revertWaiver{}
	}
	return revertWaiver{claimed: true, description: issue.Description}
}

// revertedPaths is the paths the branch undoes, once each in report order, and
// the subset the description does not name (namedInDescription). The count is
// of files being deleted, not of report lines, so a path undone by two commits
// counts once.
func revertedPaths(reverted []git.RevertedMerge, description string) (all, unnamed []string) {
	seen := make(map[string]bool)
	for _, f := range reverted {
		for _, path := range f.Paths {
			if seen[path] {
				continue
			}
			seen[path] = true
			all = append(all, path)
			if !namedInDescription(description, path) {
				unnamed = append(unnamed, path)
			}
		}
	}
	return all, unnamed
}

// namedInDescription reports whether description names path as a whole path
// rather than as a run of characters inside a longer one (gt-j5q4i). A
// substring match only counts when path characters do not touch it, so
// "docs/README.md" does not name "README.md" and "internal/x.go.bak" does not
// name "internal/x.go" — a short top-level path is not named by the longer path
// that ends with it. A sentence's trailing period or comma closes the name, so
// "internal/x.go." and "internal/x.go, which we replaced" both name it.
func namedInDescription(description, path string) bool {
	if path == "" {
		return false
	}
	for from := 0; from+len(path) <= len(description); {
		at := strings.Index(description[from:], path)
		if at < 0 {
			return false
		}
		at += from
		if nameBoundary(description, at, at+len(path)) {
			return true
		}
		from = at + 1
	}
	return false
}

// nameBoundary reports whether the match at description[start:end] stands as a
// whole path: non-path characters (or the text's edges) bound it, except that a
// period or comma followed by whitespace or the end closes a sentence and so
// closes the name too.
func nameBoundary(description string, start, end int) bool {
	if start > 0 && pathChar(description[start-1]) {
		return false
	}
	if end == len(description) || !pathChar(description[end]) {
		return true
	}
	if c := description[end]; c == '.' || c == ',' {
		return whitespaceOrEmpty(description[end+1:])
	}
	return false
}

// pathChar reports whether c can appear inside a repo-relative, slash-separated
// path: a letter, digit, underscore, dot, hyphen or slash. Multi-byte rune
// bytes are all >= 0x80, so a non-ASCII neighbor never counts as a path
// character and never hides or widens a name.
func pathChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	case c == '_', c == '.', c == '-', c == '/':
		return true
	}
	return false
}

// whitespaceOrEmpty reports whether s is empty or begins with whitespace.
func whitespaceOrEmpty(s string) bool {
	if s == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsSpace(r)
}

// deletesBySpecNote is the one notes line recording the guard standing down: a
// later reader of the bead can see that its deletions were directed, not stale.
func deletesBySpecNote(reverted []git.RevertedMerge, description string) string {
	all, _ := revertedPaths(reverted, description)
	return fmt.Sprintf("revert guard waived by deletes-by-spec for %d path(s): %s", len(all), strings.Join(all, ", "))
}

// reportRevertedMergesRecording is reportRevertedMerges for the submit path: a
// refusal that names reverted commits is recorded in the town's attention
// ledger before the same refusal is returned. A refusal is the strongest
// evidence a polecat is destroying other people's merged work (gt-63sz), and
// without the record it lives only in the refused pane.
//
// The record is best-effort and never touches the refusal: the refusal is what
// stops the submission, so a ledger write that fails warns and is forgotten.
func reportRevertedMergesRecording(r *doneRun, target string) error {
	return reportRevertedMergesRecordingTo(os.Stderr, r, r.g, func() (git.RevertReport, error) {
		return git.DetectRevertedMerges(r.g, target, "HEAD")
	}, target)
}

// reportRevertedMergesRecordingTo is reportRevertedMergesRecording with the
// detection given and the warning writer taken, so a test reads both.
func reportRevertedMergesRecordingTo(warn io.Writer, r *doneRun, g revertReportGit, detect func() (git.RevertReport, error), target string) error {
	waive := waiverFromIssue(r.sourceIssue)
	report, err := revertedMergesReport(g, detect, target, waive)
	if err == nil && waive.claimed && len(report.Reverted) > 0 {
		recordDeletesBySpecWaiver(warn, r, report.Reverted)
	}
	if err != nil && len(report.Reverted) > 0 {
		recordRevertRefusal(warn, r, g, report.Reverted)
	}
	return err
}

// recordDeletesBySpecWaiver appends the one notes line that records the guard
// standing down. Like the refusal record it is best-effort: the submission is
// already allowed, and a note that fails must not refuse it.
func recordDeletesBySpecWaiver(warn io.Writer, r *doneRun, reverted []git.RevertedMerge) {
	if r.sourceBD == nil {
		style.FprintWarning(warn, "could not record the deletes-by-spec waiver on %s: no beads client for the source bead", r.issueID)
		return
	}
	note := deletesBySpecNote(reverted, r.sourceIssue.Description)
	if err := r.sourceBD.AppendNotes(r.issueID, note); err != nil {
		style.FprintWarning(warn, "could not record the deletes-by-spec waiver on %s: %v", r.issueID, err)
	}
}

// recordRevertRefusal appends one line to the attention ledger naming the
// refusal. Every failure here is a warning: gt done has already decided to
// refuse, and whether the record lands must not change that.
func recordRevertRefusal(warn io.Writer, r *doneRun, g revertReportGit, reverted []git.RevertedMerge) {
	head, err := g.Rev("HEAD")
	if err != nil {
		style.FprintWarning(warn, "could not resolve HEAD to record the refusal: %v", err)
		return
	}
	ref := attention.Refusal{
		TS:      time.Now().UTC(),
		Bead:    r.issueID,
		Rig:     r.rigName,
		Worker:  r.polecatName,
		Branch:  r.branch,
		Head:    head,
		Kind:    attention.KindRevertGuard,
		Summary: revertRefusalSummary(g, reverted),
	}
	if err := attention.AppendRefusal(r.townRoot, ref); err != nil {
		style.FprintWarning(warn, "could not record the refusal for the attention queue: %v", err)
	}
}

// revertRefusalSummary is the ledger line's one-line description: the first
// reverted commit and how many there are.
func revertRefusalSummary(g revertReportGit, reverted []git.RevertedMerge) string {
	first := reverted[0]
	return fmt.Sprintf("branch reverts %d merged commit(s): %s %s",
		len(reverted), ShortSHA(first.Commit), commitSubjectOrUnavailable(g, first.Commit))
}

// relocatedMergesNote names the merged changes the branch moves rather than
// undoes, or "" when there are none. The guard accepts these, so the note is
// informational: it exists so the polecat that wrote the move — and anyone
// reading its submission — can see what the guard saw and why it did not
// refuse.
func relocatedMergesNote(g revertReportGit, relocated []git.RevertedMerge) string {
	if len(relocated) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  Relocated — moved, not undone, so the code they carry survives:\n")
	for _, f := range relocated {
		fmt.Fprintf(&b, "    %s %s\n", ShortSHA(f.Commit), commitSubjectOrUnavailable(g, f.Commit))
		for _, path := range f.Paths {
			fmt.Fprintf(&b, "      moved: %s\n", path)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// revertedMergeRefusal builds the refusal error for a branch that undoes merged
// work. The message deliberately does not name the flag that overrides it:
// agents read refusal text and self-bypass, so the text says what to do about
// the branch instead. unnamed is the reverted paths a deletes-by-spec waiver
// could not cover, and is empty unless the bead carries that label.
func revertedMergeRefusal(g revertReportGit, target string, found []git.RevertedMerge, unnamed []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to submit: this branch undoes work already merged to %s\n\n", target)
	fmt.Fprintf(&b, "These commits on %s are undone by your branch:\n", target)
	for i, f := range found {
		if i == revertReportLimit {
			fmt.Fprintf(&b, "  ... and %d more\n", len(found)-revertReportLimit)
			break
		}
		fmt.Fprintf(&b, "  %s %s\n", ShortSHA(f.Commit), commitSubjectOrUnavailable(g, f.Commit))
		for j, path := range f.Paths {
			if j == revertPathsPerCommit {
				fmt.Fprintf(&b, "      ... and %d more paths\n", len(f.Paths)-revertPathsPerCommit)
				break
			}
			fmt.Fprintf(&b, "      undoes: %s\n", path)
		}
	}
	if len(unnamed) > 0 {
		b.WriteString("\nThe bead carries deletes-by-spec, which waives a directed deletion only when " +
			"the description names every path being deleted. It does not name:\n")
		for _, path := range unnamed {
			fmt.Fprintf(&b, "  unnamed: %s\n", path)
		}
	}
	b.WriteString("\nYour working tree is older than the base your commit claims to sit on, " +
		"so the commit records (your tree) - (that base): a revert of every commit merged " +
		"in between, plus your own change. Submitting it would delete other people's merged work.\n\n")
	fmt.Fprintf(&b, "Integrate with:\n"+
		"  git fetch origin && git rebase %s\n\n", target)
	fmt.Fprintf(&b, "Then confirm the branch lists only YOUR files and re-run gt done:\n"+
		"  git diff --stat %s...HEAD", target)
	return fmt.Errorf("%s", b.String())
}

// commitSubjectOrUnavailable names a commit for a report line, standing in for
// a subject this repository cannot produce rather than dropping the commit.
func commitSubjectOrUnavailable(g revertReportGit, commit string) string {
	subject, err := g.CommitSubject(commit)
	if err != nil || subject == "" {
		return "(subject unavailable)"
	}
	return subject
}
