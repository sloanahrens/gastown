package specdispatch

import (
	"fmt"
	"sort"
	"strings"
)

// HeldBead is one bead a working seat already holds, reduced to what the
// overlap rule reads: its id, the seat holding it, and the paths its own text
// names. The caller reads the set and fills the fields; nothing here queries.
//
// Assignee lets the hold line say whose seat to patrol or which stray bead to
// close; a bead no assignee names was never slung and is not held at all, so
// HeldOverlap skips it whatever the caller passed (gt-z80p9, gt-xrog0).
type HeldBead struct {
	ID       string
	Assignee string
	Paths    []string
}

// maxNamedHeldPaths bounds the shared paths the hold line names, so a pair of
// beads describing an area rather than a defect does not turn a skip line into
// a wall (maxNamedChildren's rule, gt-z80p9).
const maxNamedHeldPaths = 3

// HeldOverlap reports why a candidate is the same defect a working seat is
// already on: a bead in held names a path the candidate's own text names too,
// and the reason names that bead, the seat holding it, and the paths the two
// share (gt-z80p9, gt-xrog0).
//
// Paths alone decide it, and only against work a seat already has in hand. Two
// beads describing one defect from different vantage points share no vocabulary
// but always share files — the signal the sling-time guard reads too — and
// against a held bead that signal stands alone, where against the whole open
// pool it does not: a shared file there is as often two genuinely distinct
// beads in one package (gt-mcq). Naming the lowest overlapping id keeps the
// line a candidate held twice reports identical. An entry with no assignee
// never holds: the hold fails open rather than blocking a candidate on a bead
// no seat is working.
func HeldOverlap(paths []string, held []HeldBead) string {
	if len(paths) == 0 || len(held) == 0 {
		return ""
	}
	mine := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p != "" {
			mine[p] = true
		}
	}
	var match, assignee string
	var shared []string
	for _, h := range held {
		if h.ID == "" || h.Assignee == "" || (match != "" && h.ID >= match) {
			continue
		}
		both := make([]string, 0, len(h.Paths))
		for _, p := range h.Paths {
			if mine[p] {
				both = append(both, p)
			}
		}
		if len(both) == 0 {
			continue
		}
		sort.Strings(both)
		match, assignee, shared = h.ID, h.Assignee, both
	}
	if match == "" {
		return ""
	}
	named := shared
	more := 0
	if len(named) > maxNamedHeldPaths {
		more = len(named) - maxNamedHeldPaths
		named = named[:maxNamedHeldPaths]
	}
	reason := fmt.Sprintf("%s (held by %s) is already hooked to a working seat and names %s", match, assignee, strings.Join(named, ", "))
	if more > 0 {
		reason += fmt.Sprintf(" (+%d more)", more)
	}
	return reason + "; the hold lifts when that bead closes"
}

// HoldFiles is the set of paths the overlap hold reads from a bead's text: the
// paths it names, minus the documents it only reads. A bead cites the
// standards and research it must follow (every test bead cites the
// test-engineering standard) without changing them, so two such beads do not
// hold each other. A reference document under docs/ that the bead's Constraints
// section lists is a file the bead changes, so it stays; a path in the town's
// claude/docs standards tree never stays, listed or not, because that tree
// lives in ~/.claude and no polecat edits it (gt-6t44f, gt-kr17g).
//
// named is the bead's paths; listed is the paths its Constraints section names.
func HoldFiles(named, listed []string) []string {
	constrained := make(map[string]bool, len(listed))
	for _, p := range listed {
		constrained[p] = true
	}
	out := make([]string, 0, len(named))
	for _, p := range named {
		if isStandardDoc(p) || (IsReferenceDoc(p) && !constrained[p]) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// IsReferenceDoc reports whether a normalized path names a read-only reference
// document: the repo's docs/ tree, or the town's claude/docs standards tree
// (isStandardDoc) (gt-6t44f).
func IsReferenceDoc(path string) bool {
	return strings.HasPrefix(path, "docs/") || isStandardDoc(path)
}

// isStandardDoc reports whether a normalized path names a document in the
// town's claude/docs standards tree in any spelling it arrives in
// ("claude/docs/...", "~/.claude/docs/...", an absolute path into it). The
// tree is matched at a directory boundary, so a source file that merely embeds
// "claude/docs/" in its name does not match (gt-014o6, gt-kr17g).
func isStandardDoc(path string) bool {
	return strings.HasPrefix(path, "claude/docs/") ||
		strings.Contains(path, "/claude/docs/") ||
		strings.Contains(path, "/.claude/docs/")
}
