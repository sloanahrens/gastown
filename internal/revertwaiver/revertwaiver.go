// Package revertwaiver decides which deletions a revert guard may let through.
//
// A bead labeled deletes-by-spec directs its branch to remove the files its
// description names, so the content those deletions revert is work, not a
// stale tree. gt done's submit guard and checkpoint_dog's WIP guard both read
// the waiver here, so the two cannot disagree on what a directed deletion is
// (gt-p8k4i, gt-b8f9z).
package revertwaiver

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
)

// Label marks a bead whose own description names the files its branch is meant
// to delete. The label alone waives nothing, because the author of a bead can
// label it (gt-b8f9z).
const Label = "deletes-by-spec"

// Note reports whether issue is a deletes-by-spec bead whose description names
// every path reverted undoes, and if so the one-line note recording why the
// guard stands down.
func Note(issue *beads.Issue, reverted []git.RevertedMerge) (string, bool) {
	if issue == nil || !beads.HasLabel(issue, Label) {
		return "", false
	}
	if _, unnamed := Paths(reverted, issue.Description); len(unnamed) > 0 {
		return "", false
	}
	return NoteText(reverted, issue.Description), true
}

// Paths returns the paths the branch undoes, once each in report order, and
// the subset the description does not name. The count is of files being
// deleted, not of report lines, so a path undone by two commits counts once.
func Paths(reverted []git.RevertedMerge, description string) (all, unnamed []string) {
	seen := make(map[string]bool)
	for _, f := range reverted {
		for _, path := range f.Paths {
			if seen[path] {
				continue
			}
			seen[path] = true
			all = append(all, path)
			if !NamedInDescription(description, path) {
				unnamed = append(unnamed, path)
			}
		}
	}
	return all, unnamed
}

// NamedInDescription reports whether description names path, whole or through
// a directory above it written with its trailing slash.
//
// A directory spares the description from listing every file under it, so
// "internal/old/" names "internal/old/a.go" and "internal/old/sub/b.go". The
// slash is what makes it a directory: "internal/old" names no file under it,
// and "internal/old/a.go" names that file only.
//
// The directory must be at least two components deep (gt-vjdkl). "internal/"
// names nothing under it: a single top-level directory is prose, not a
// directed deletion — a description that happens to say "rework internal/"
// would otherwise waive the revert guard for the whole tree, far past the
// bead's real scope.
//
// Either form must stand as a whole path rather than as a run of characters
// inside a longer one (gt-j5q4i). A substring match only counts when path
// characters do not touch it, so "docs/README.md" does not name "README.md",
// "internal/x.go.bak" does not name "internal/x.go", and "docs/" does not name
// "mydocs/x.go" or "sub/docs/x.go" — a short path is not named by a longer one
// that ends with it. A sentence's trailing period or comma closes the name, so
// "internal/x.go." and "internal/x.go, which we replaced" both name it.
func NamedInDescription(description, path string) bool {
	if path == "" {
		return false
	}
	if mentionedWhole(description, path) {
		return true
	}
	components := 0
	for i := 0; i < len(path); i++ {
		if path[i] != '/' {
			continue
		}
		components++
		if components < 2 {
			continue
		}
		if mentionedWhole(description, path[:i+1]) {
			return true
		}
	}
	return false
}

// mentionedWhole reports whether description carries name as a whole path, not
// inside a longer one (nameBoundary).
func mentionedWhole(description, name string) bool {
	for from := 0; from+len(name) <= len(description); {
		at := strings.Index(description[from:], name)
		if at < 0 {
			return false
		}
		at += from
		if nameBoundary(description, at, at+len(name)) {
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

// NoteText is the one notes line recording the guard standing down: a later
// reader of the bead can see that its deletions were directed, not stale.
func NoteText(reverted []git.RevertedMerge, description string) string {
	all, _ := Paths(reverted, description)
	return fmt.Sprintf("revert guard waived by deletes-by-spec for %d path(s): %s", len(all), strings.Join(all, ", "))
}
