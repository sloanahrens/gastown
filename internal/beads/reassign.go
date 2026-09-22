package beads

import (
	"fmt"
	"strings"
)

// ReassignmentMarker prefixes the durable record left on a bead whose assignee
// was replaced. The assignee field holds only the last writer, so an audit that
// starts from a bead — find its worker, find that worker's branch — lands on
// the wrong polecat as soon as one reassignment has happened. The record
// carries the outgoing assignee and its branches past that overwrite (gt-zd7c).
const ReassignmentMarker = "REASSIGNED:"

// formatReassignmentNote renders a ReassignmentMarker record. The Branch lines
// match the refinery's merge-rejection note, so one grep over a bead's notes and
// comments finds the branch history of either kind.
//
// A nil branches slice means the caller could not enumerate origin, which is not
// the same claim as an empty one: "(unknown)" sends an auditor to git, where
// "(none on origin)" would tell them to stop looking.
func formatReassignmentNote(from, to, requester string, branches []string) string {
	if to == "" {
		to = "(unassigned)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s -> %s", ReassignmentMarker, from, to)
	switch {
	case branches == nil:
		b.WriteString("\nBranch: (unknown)")
	case len(branches) == 0:
		b.WriteString("\nBranch: (none on origin)")
	}
	for _, branch := range branches {
		b.WriteString("\nBranch: " + branch)
	}
	b.WriteString("\nBy: " + requester)
	return b.String()
}

// RecordReassignment appends a reassignment record to a bead. branches are the
// branches the caller knows belong to the outgoing assignee, newest first: nil
// when origin could not be queried, empty when it was queried and matched none.
//
// Call this before the write that overwrites the old assignee: afterwards the
// only surviving copy of the old value is the Dolt events table, which no gt
// command reads. A no-op when from is empty or already equals to.
func (b *Beads) RecordReassignment(id, from, to, requester string, branches []string) error {
	if id == "" || from == "" || from == to {
		return nil
	}
	return b.AddComment(id, formatReassignmentNote(from, to, requester, branches))
}
