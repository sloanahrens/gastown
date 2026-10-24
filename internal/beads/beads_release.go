package beads

import (
	"errors"
	"fmt"
)

// bdGuardNotHeldExit is bd's exit code when an --if-assignee/--if-status
// precondition no longer holds: nothing was written.
const bdGuardNotHeldExit = 13

// ErrGuardNotHeld reports a guarded bd update that wrote nothing because its
// precondition no longer held (another actor won the race).
var ErrGuardNotHeld = errors.New("bd update guard no longer held")

// ReleaseIfAssignee returns a work bead to open with no assignee, but only
// while it is still assigned to assignee (bd --if-assignee, atomic). It is the
// one release write for a polecat's hooked work (gt-vm5g4, gt-7evi4): it never
// takes a bead back from an agent it was re-slung to, and it is the claim
// transfer bd permits on an in_progress bead, where a plain assignee write is
// fenced. released=false with a nil error means the guard no longer held.
func (b *Beads) ReleaseIfAssignee(id, assignee string) (released bool, err error) {
	return b.TransferIfAssignee(id, assignee, "open", "")
}

// TransferIfAssignee sets a bead's status and assignee, but only while it is
// still assigned to expected (bd --if-assignee, atomic). It is how a failed
// sling hands a bead back to its original holder. transferred=false with a nil
// error means the guard no longer held.
func (b *Beads) TransferIfAssignee(id, expected, status, assignee string) (transferred bool, err error) {
	if !b.noRoute {
		if target := b.forIssueID(id); target != b {
			return target.TransferIfAssignee(id, expected, status, assignee)
		}
	}
	_, err = b.run("update", id, "--status="+status, "--assignee="+assignee, "--if-assignee="+expected)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrGuardNotHeld):
		return false, nil
	}
	return false, err
}

// TransferIfHeld sets a bead's status and assignee, but only while it is still
// held by expectedAssignee in expectedStatus (bd --if-assignee and --if-status,
// one atomic update). The status half keeps a restore from overwriting a bead
// whose state moved under it — closed, submitted, or taken by another actor
// (gt-moxlb). transferred=false with a nil error means a guard no longer held.
func TransferIfHeld(c Client, id, expectedAssignee, expectedStatus, status, assignee string) (transferred bool, err error) {
	if b, ok := c.(*Beads); ok {
		return transferIfHeld(b, id, expectedAssignee, expectedStatus, status, assignee)
	}
	// A Client that cannot name bd's status guard does the compare in two
	// steps: the read holds off a bead whose status moved, and
	// TransferIfAssignee's guard is the atomic half for the assignee.
	issue, err := c.Show(id)
	if err != nil {
		return false, err
	}
	if issue.Status != expectedStatus || issue.Assignee != expectedAssignee {
		return false, nil
	}
	return c.TransferIfAssignee(id, expectedAssignee, status, assignee)
}

// transferIfHeld is the *Beads path: one bd update carrying both guards, so no
// read can go stale between the compare and the write, and routing follows the
// issue's prefix the way the other guarded writes do.
func transferIfHeld(b *Beads, id, expectedAssignee, expectedStatus, status, assignee string) (transferred bool, err error) {
	if !b.noRoute {
		if target := b.forIssueID(id); target != b {
			return transferIfHeld(target, id, expectedAssignee, expectedStatus, status, assignee)
		}
	}
	_, err = b.run("update", id, "--status="+status, "--assignee="+assignee, "--if-assignee="+expectedAssignee, "--if-status="+expectedStatus)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrGuardNotHeld):
		return false, nil
	}
	return false, err
}

// TransferIfUnassigned sets a bead's status and assignee, but only while the
// bead is still open and unassigned: the undo of a force re-sling's unhook,
// which never takes a bead another actor has since hooked (gt-ct2ob).
// transferred=false with a nil error means the guard no longer held; a missing
// bead is an error.
func TransferIfUnassigned(c Client, id, status, assignee string) (transferred bool, err error) {
	if b, ok := c.(*Beads); ok {
		return transferIfUnassigned(b, id, status, assignee)
	}
	// A Client that cannot name bd's guard does the same compare in two
	// steps: the read holds off a closed or already-taken bead, and
	// TransferIfAssignee's empty guard is the atomic "still unassigned".
	issue, err := c.Show(id)
	if err != nil {
		return false, err
	}
	if issue.Status != string(StatusOpen) || issue.Assignee != "" {
		return false, nil
	}
	return c.TransferIfAssignee(id, "", status, assignee)
}

// transferIfUnassigned is the *Beads path: one bd update carrying both
// preconditions (--if-status=open, --if-assignee=), so no read can go stale
// between the compare and the write, and routing follows the issue's prefix
// the way the other guarded writes do.
func transferIfUnassigned(b *Beads, id, status, assignee string) (transferred bool, err error) {
	if !b.noRoute {
		if target := b.forIssueID(id); target != b {
			return transferIfUnassigned(target, id, status, assignee)
		}
	}
	_, err = b.run("update", id, "--status="+status, "--assignee="+assignee, "--if-status=open", "--if-assignee=")
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrGuardNotHeld):
		return false, nil
	}
	return false, err
}

// AppendNotes appends note to an issue's notes (bd update --append-notes),
// which bd joins to existing notes with a newline. It routes by the issue's
// prefix the way Update does. An empty note is a no-op.
func (b *Beads) AppendNotes(id, note string) error {
	if note == "" {
		// bd would append a bare newline to existing notes.
		return nil
	}
	if !b.noRoute {
		if target := b.forIssueID(id); target != b {
			return target.AppendNotes(id, note)
		}
	}
	// A dash-leading note needs no separator: bd takes the next argument as
	// --append-notes' value whatever it looks like, unlike a bare positional
	// (gt-1q9nl).
	_, err := b.run("update", id, "--append-notes", note)
	return err
}

// ReleasePinnedSteps unpins each pinned issue of steps, clearing its claim as
// well, and reports how many it released. A pin is a close fence, as an
// assignee other than the actor is, and bd refuses to close a pinned issue
// whoever holds it, so a closer clears the pin before it closes the step. A
// release that fails is joined into the error, and the rest still release.
func ReleasePinnedSteps(b interface {
	Update(id string, opts UpdateOptions) error
}, steps []*Issue) (int, error) {
	open, unassigned := string(StatusOpen), ""
	var errs []error
	released := 0
	for _, step := range steps {
		if IssueStatus(step.Status) != IssueStatusPinned {
			continue
		}
		if err := b.Update(step.ID, UpdateOptions{Status: &open, Assignee: &unassigned}); err != nil {
			errs = append(errs, fmt.Errorf("releasing pinned step %s: %w", step.ID, err))
			continue
		}
		released++
	}
	return released, errors.Join(errs...)
}
