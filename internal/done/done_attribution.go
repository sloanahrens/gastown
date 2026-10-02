package done

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
)

// This file keeps AI attribution out of the commit messages gt done submits
// (gt-v4ssj.10). Claude Code appends a "Co-Authored-By: Claude ..." trailer and
// a "Generated with Claude Code" line to commits unless the prompt forbids it,
// and the landing script refuses any branch carrying them: all work appears
// human-authored. Two polecat branches (gt-fd2cu.4.1, gt-7g14a) reached landing
// with the trailer and were refused there. The polecat template now says not to
// add it; this is the backstop for a session that did anyway.
//
// A trailer line is stripped by rewriting the branch's messages. A subject line
// that is itself the attribution cannot be stripped without inventing a new
// subject, so that refuses and leaves the rewrite to the author.

var (
	// A Co-Authored-By trailer of any identity. A polecat has no human
	// co-author to credit, so the trailer is machine-added whoever it names.
	attributionTrailerRE = regexp.MustCompile(`(?i)^\s*co-authored-by\s*:`)
	// The "Generated with Claude Code" footer, with or without its robot emoji
	// and markdown link.
	attributionGeneratedRE = regexp.MustCompile(`(?i)^\s*(?:🤖\s*)?generated\s+(?:with|by)\b.*\b(?:claude|anthropic)\b`)
)

// isAttributionLine reports whether line is a Co-Authored-By trailer or a
// "Generated with Claude" footer.
func isAttributionLine(line string) bool {
	return attributionTrailerRE.MatchString(line) || attributionGeneratedRE.MatchString(line)
}

// stripAttributionLines removes attribution lines from a commit message and
// returns the cleaned message plus the lines it removed. The message comes back
// untouched, byte for byte, when there is nothing to remove.
func stripAttributionLines(message string) (string, []string) {
	var kept, removed []string
	for _, line := range strings.Split(message, "\n") {
		if isAttributionLine(line) {
			removed = append(removed, strings.TrimSpace(line))
			continue
		}
		kept = append(kept, line)
	}
	if len(removed) == 0 {
		return message, nil
	}
	// The trailer block is usually last and set off by a blank line; trimming
	// drops the blank line it leaves behind.
	return strings.TrimSpace(strings.Join(kept, "\n")), removed
}

// stripAttributionTrailers removes attribution lines from every commit message
// in baseRef..HEAD, and refuses when a subject line is itself attribution.
// It fails closed: a range it cannot read or rewrite is not submitted.
func stripAttributionTrailers(g *git.Git, baseRef string) error {
	rewritten, err := g.RewriteCommitMessages(baseRef, func(commit, message string) (string, error) {
		subject, _, _ := strings.Cut(message, "\n")
		if isAttributionLine(subject) {
			return "", &attributedSubjectError{commit: commit, subject: subject, baseRef: baseRef}
		}
		cleaned, _ := stripAttributionLines(message)
		return cleaned, nil
	})
	var refused *attributedSubjectError
	if errors.As(err, &refused) {
		return refused
	}
	if err != nil {
		return fmt.Errorf("cannot strip AI attribution from commit messages: %w\n"+
			"Refusing to submit rather than risk landing it. "+
			"Run `git fetch origin && git rebase %s`, then re-run gt done.", err, baseRef)
	}
	if rewritten > 0 {
		fmt.Printf("%s Removed AI attribution trailers from %d commit message(s)\n", style.Bold.Render("✓"), rewritten)
	}
	return nil
}

// attributedSubjectError refuses a commit whose subject line is an attribution
// line. Stripping it would leave a commit with no subject, and choosing one is
// the author's job.
type attributedSubjectError struct {
	commit, subject, baseRef string
}

func (e *attributedSubjectError) Error() string {
	return fmt.Sprintf("refusing to submit: the subject of commit %s is an AI attribution line, not a description of the change:\n\n"+
		"  %s\n\n"+
		"Landing refuses a branch that carries AI attribution, and gt done cannot write a subject for you. "+
		"Reword the commit, then re-run gt done. For the newest commit:\n"+
		"  git commit --amend -m \"<type>: <what changed>\"\n\n"+
		"Then confirm no commit on the branch names an AI author:\n"+
		"  git log --format=%%B %s..HEAD", ShortSHA(e.commit), e.subject, e.baseRef)
}
