package beads

import "errors"

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
