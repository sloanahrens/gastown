package specdispatch

import (
	"fmt"
	"sort"
	"strings"
)

// HeldBead is one bead a working seat already holds, reduced to what the
// overlap rule reads: the bead's id, and the paths its own text names. The
// caller reads the set (a rig's hooked and in-progress beads) and fills Paths;
// nothing here queries (gt-z80p9).
type HeldBead struct {
	ID    string
	Paths []string
}

// maxNamedHeldPaths bounds the shared paths the hold line names, so a pair of
// beads describing an area rather than a defect does not turn a skip line into
// a wall (maxNamedChildren's rule, gt-z80p9).
const maxNamedHeldPaths = 3

// HeldOverlap reports why a candidate is the same defect a working seat is
// already on: a bead in held names a path the candidate's own text names too,
// and the reason names that bead and the paths the two share (gt-z80p9).
//
// Paths alone decide it, and only against work a seat already has in hand. Two
// beads describing one defect from different vantage points share no vocabulary
// but always share files — the signal the sling-time guard reads too — and
// against a held bead that signal stands alone, where against the whole open
// pool it does not: a shared file there is as often two genuinely distinct
// beads in one package (gt-mcq). Naming the lowest overlapping id keeps the
// line a candidate held twice reports identical.
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
	var match string
	var shared []string
	for _, h := range held {
		if h.ID == "" || (match != "" && h.ID >= match) {
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
		match, shared = h.ID, both
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
	reason := fmt.Sprintf("%s is already hooked to a working seat and names %s", match, strings.Join(named, ", "))
	if more > 0 {
		reason += fmt.Sprintf(" (+%d more)", more)
	}
	return reason + "; the hold lifts when that bead closes"
}
