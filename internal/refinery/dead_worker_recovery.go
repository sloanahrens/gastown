package refinery

import (
	"fmt"
	"io"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
)

// MergeRejectionNoteMarker is the canonical vocabulary written into source-bead
// notes when a branch-caused merge rejection is recorded. The polecat work
// formula (mol-polecat-work) greps bead notes for this exact marker during its
// resume path, so a redispatched polecat can find the rejection details and
// reuse the surviving branch. Keep the marker and the formula in sync (gt-tc0).
const MergeRejectionNoteMarker = "MERGE REJECTION"

// deadWorkerRecoveryRequest describes a branch-caused merge rejection whose
// worker polecat may no longer have a live session.
type deadWorkerRecoveryRequest struct {
	MRID          string
	Branch        string
	Target        string
	SourceIssue   string
	Worker        string // as recorded on the MR, e.g. "polecats/nux"
	RigName       string
	FailureType   string
	ErrorMsg      string
	AttemptNumber int
}

// rejectedSourceBeads is the narrow beads surface needed for recovery.
// *beads.Beads satisfies it.
type rejectedSourceBeads interface {
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	Run(args ...string) ([]byte, error)
}

// workerNameFromMR extracts the bare polecat name from an MR's Worker field,
// which may be recorded as "nux", "polecats/nux", or "<rig>/polecats/nux".
func workerNameFromMR(worker string) string {
	worker = strings.TrimSpace(worker)
	if worker == "" {
		return ""
	}
	parts := strings.Split(worker, "/")
	return parts[len(parts)-1]
}

func formatMergeRejectionNote(req deadWorkerRecoveryRequest) string {
	return fmt.Sprintf("%s (attempt %d): %s - %s\nBranch: %s\nTarget: %s\nMR: %s",
		MergeRejectionNoteMarker, req.AttemptNumber, req.FailureType, req.ErrorMsg,
		req.Branch, req.Target, req.MRID)
}

// recoverRejectedMRDeadWorker routes a rejected MR into the deacon redispatch
// pipeline when the assignee polecat has no live session (gt-tc0).
//
// A transient polecat exits at gt done and its source bead is closed, so the
// FIX_NEEDED nudge/mail sent on rejection lands nowhere: the MR re-queues and
// the refinery re-gates the unchanged branch until thrash control gives up,
// while the fix never happens. When the worker is dead this function:
//
//  1. persists the rejection into the source bead notes using the
//     MergeRejectionNoteMarker vocabulary the polecat resume path greps for,
//  2. reopens the source bead with no assignee (deacon redispatch only acts
//     on open beads), and
//  3. sends RECOVERED_BEAD to the deacon, whose Redispatch handler enforces
//     cooldown and max-attempt limits before re-slinging.
//
// The redispatched polecat finds the note plus the surviving remote branch and
// makes a targeted fix instead of starting over.
//
// Returns true when the bead was handed to the deacon for redispatch.
func recoverRejectedMRDeadWorker(bd rejectedSourceBeads, sessionAlive func(polecatName string) (bool, error), sendMail func(*mail.Message) error, out io.Writer, req deadWorkerRecoveryRequest) bool {
	logf := func(format string, args ...interface{}) {
		if out != nil {
			_, _ = fmt.Fprintf(out, format, args...)
		}
	}

	polecatName := workerNameFromMR(req.Worker)
	if polecatName == "" || strings.TrimSpace(req.SourceIssue) == "" {
		return false
	}
	if bd == nil || sessionAlive == nil {
		return false
	}

	alive, err := sessionAlive(polecatName)
	if err != nil {
		// Can't determine liveness — assume the nudge reached the worker
		// rather than risk a spawn storm on flaky tmux state.
		logf("[Engineer] Warning: could not check session for %s: %v (skipping dead-worker recovery)\n", polecatName, err)
		return false
	}
	if alive {
		return false
	}

	issue, err := bd.Show(req.SourceIssue)
	if err != nil || issue == nil {
		logf("[Engineer] Warning: dead-worker recovery could not read source bead %s: %v\n", req.SourceIssue, err)
		return false
	}

	status := beads.IssueStatus(strings.TrimSpace(issue.Status))
	assignee := strings.TrimSpace(issue.Assignee)
	// If a live reassignment already happened (another polecat owns the bead),
	// leave it alone — the deacon or a fresh worker is already on it.
	if !status.IsTerminal() && assignee != "" &&
		assignee != polecatName && !strings.HasSuffix(assignee, "/"+polecatName) {
		logf("[Engineer] Source bead %s already reassigned to %s — skipping dead-worker recovery\n", req.SourceIssue, assignee)
		return false
	}

	// Persist the rejection so it survives into the next session. Notes are
	// the resume-path contract: mol-polecat-work greps for the marker and the
	// Branch: line. Append to any existing notes rather than clobbering them,
	// and skip the write when the same note is already there (the refinery
	// re-gates an unchanged branch on every poll until redispatch lands, so
	// this path can repeat with an identical rejection).
	note := formatMergeRejectionNote(req)
	if !strings.Contains(issue.Notes, note) {
		combined := note
		if existing := strings.TrimSpace(issue.Notes); existing != "" {
			combined = existing + "\n\n" + note
		}
		if _, err := bd.Run("update", req.SourceIssue, "--notes", combined); err != nil {
			logf("[Engineer] Warning: failed to record rejection note on %s: %v\n", req.SourceIssue, err)
		}
	}

	// Reopen with no assignee so the deacon's Redispatch (which requires
	// status=open) can re-sling it.
	openStatus := string(beads.StatusOpen)
	emptyAssignee := ""
	if err := bd.Update(req.SourceIssue, beads.UpdateOptions{Status: &openStatus, Assignee: &emptyAssignee}); err != nil {
		logf("[Engineer] Warning: failed to reopen source bead %s for redispatch: %v\n", req.SourceIssue, err)
		return false
	}
	logf("[Engineer] Worker %s has no live session — reopened %s for redispatch (%s)\n", polecatName, req.SourceIssue, MergeRejectionNoteMarker)

	if sendMail == nil {
		logf("[Engineer] Warning: no mail router — %s reopened but RECOVERED_BEAD not sent; deacon patrol will pick it up from ready queue\n", req.SourceIssue)
		return false
	}

	msg := &mail.Message{
		From:     req.RigName + "/refinery",
		To:       "deacon/",
		Subject:  fmt.Sprintf("RECOVERED_BEAD %s", req.SourceIssue),
		Priority: mail.PriorityHigh,
		Body: fmt.Sprintf(`Merge rejection with no live worker (transient polecat).

Bead: %s
Polecat: %s/polecats/%s
MR: %s
Branch: %s
Failure-Type: %s
Attempt: %d

The source bead has been reopened with %s notes.
Please re-dispatch. The branch survives on origin, so the next polecat
can check it out and make a targeted fix instead of starting over.`,
			req.SourceIssue, req.RigName, polecatName, req.MRID, req.Branch,
			req.FailureType, req.AttemptNumber, MergeRejectionNoteMarker),
	}
	if err := sendMail(msg); err != nil {
		logf("[Engineer] Warning: failed to send RECOVERED_BEAD for %s: %v\n", req.SourceIssue, err)
		return false
	}
	logf("[Engineer] Sent RECOVERED_BEAD %s to deacon for redispatch\n", req.SourceIssue)
	return true
}
