package refinery

import (
	"fmt"
	"io"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
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

	// Findings carries the om editorial findings behind this rejection, when
	// it came from an om review (`gt mq review`) rather than a plain
	// build/test failure. Nil/empty for non-editorial rejections. Findings
	// travel forward onto the source bead notes and the RECOVERED_BEAD mail
	// so the next attempt — and its reviewer — sees them (gt-zdxn, absorbs
	// gt-htn2): T5's prior-findings builder parses the '- id:' note lines.
	Findings []RejectionFinding

	// Summary is a one-line verdict summary for the RECOVERED_BEAD mail's
	// Rejection-Summary line. Empty for non-editorial rejections.
	Summary string
}

// RejectionFinding is one om editorial finding attached to a merge
// rejection. ID is the finding's stable 12-hex id (om: first 12 hex of
// sha256(path|title)), matching the id om's --prior-findings classification
// keys on.
type RejectionFinding struct {
	ID       string
	Severity string // "major" | "minor" | "info"
	Path     string
	Line     int
	Title    string
}

// rejectionFindingIDs returns just the ids, in order, for the
// Rejection-Findings mail line.
func rejectionFindingIDs(findings []RejectionFinding) []string {
	ids := make([]string, len(findings))
	for i, f := range findings {
		ids[i] = f.ID
	}
	return ids
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
	note := fmt.Sprintf("%s (attempt %d): %s - %s\nBranch: %s\nTarget: %s\nMR: %s",
		MergeRejectionNoteMarker, req.AttemptNumber, req.FailureType, req.ErrorMsg,
		req.Branch, req.Target, req.MRID)
	for _, f := range req.Findings {
		note += fmt.Sprintf("\n- id:%s sev:%s %s:%d — %s", f.ID, f.Severity, f.Path, f.Line, f.Title)
	}
	return note
}

// deliberateTerminalCloseReasonMarkers are substrings of a bead's close_reason
// that mean the bead was closed on purpose by an operator or by another MR's
// success, not merely because the assigned worker finished or vanished.
// Reopening a bead closed for one of these reasons resurrects work that is
// intentionally done — the exact gt-pvwy incident (gt-wisp-bakv, closed
// "superseded by gt-me9t" after granite merged the duplicate).
var deliberateTerminalCloseReasonMarkers = []string{
	"supersede", // matches "superseded", "supersedes"
	"duplicate",
	"cancel", // matches "cancelled", "canceled"
	"wontfix",
	"won't fix",
	"not-planned",
	"obsolete",
	"abandoned",
	"merged in ", // convention used elsewhere for "closed because merged via another MR/dep"
}

// isDeliberateTerminalCloseReason reports whether a closed bead's close_reason
// indicates the closure was intentional (cancelled/superseded/duplicate/already
// merged elsewhere) rather than "the assigned worker finished the work and
// closed it themselves" — the only case dead-worker recovery should resurrect.
func isDeliberateTerminalCloseReason(reason string) bool {
	lower := strings.ToLower(reason)
	for _, marker := range deliberateTerminalCloseReasonMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// recoverRejectedMRDeadWorker routes a rejected MR into the deacon redispatch
// pipeline when the assignee polecat can no longer act on it (gt-tc0, gt-2usm).
//
// A transient polecat exits at gt done and its source bead is closed, so the
// FIX_NEEDED nudge/mail sent on rejection lands nowhere: the MR re-queues and
// the refinery re-gates the unchanged branch until thrash control gives up,
// while the fix never happens. When the worker can no longer act this function:
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
// Session liveness alone is the wrong signal for "can this worker still fix
// it" (gt-2usm): a persistent polecat that already ran gt done keeps a
// reusable, live-but-inert tmux session and will never act on a nudge. The
// real signal is whether the worker still HOLDS the assignment: the source
// bead's own status/assignee. Recovery proceeds whenever the bead shows the
// worker no longer holds it — closed (gt done already ran) or unassigned —
// and is skipped when a *different* worker plainly owns it now, when this
// same worker still holds an open bead and its session is confirmed alive,
// or when a closed bead's own close_reason marks it deliberately cancelled,
// superseded, or already landed via another MR (gt-pvwy) — reopening those
// resurrects work an operator or a prior merge already finished.
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
	if bd == nil {
		return false
	}

	issue, err := bd.Show(req.SourceIssue)
	if err != nil || issue == nil {
		logf("[Engineer] Warning: dead-worker recovery could not read source bead %s: %v\n", req.SourceIssue, err)
		return false
	}

	status := beads.IssueStatus(strings.TrimSpace(issue.Status))
	assignee := strings.TrimSpace(issue.Assignee)
	stillAssignedHere := assignee == polecatName || strings.HasSuffix(assignee, "/"+polecatName)

	// A terminal bead is not automatically safe to resurrect: it may be
	// closed because the worker finished it (reopen is right), but it may
	// also be closed because an operator deliberately cancelled/superseded
	// it, or because the work already landed via a different MR (gt-pvwy —
	// the superseded-duplicate case that motivated this check happened for
	// real: gt-wisp-bakv, closed as superseded, would have been resurrected
	// by 'gt mq reject' had this gate not existed). The close reason is the
	// cheap, decisive signal a status/assignee check alone cannot give.
	if status.IsTerminal() {
		if reason := strings.TrimSpace(issue.CloseReason); reason != "" && isDeliberateTerminalCloseReason(reason) {
			logf("[Engineer] Source bead %s closed deliberately (close_reason=%q) — skipping dead-worker recovery to avoid resurrecting cancelled/superseded/already-merged work\n", req.SourceIssue, reason)
			return false
		}
	}

	// If a live reassignment already happened (another polecat owns the bead),
	// leave it alone — the deacon or a fresh worker is already on it.
	if !status.IsTerminal() && assignee != "" && !stillAssignedHere {
		logf("[Engineer] Source bead %s already reassigned to %s — skipping dead-worker recovery\n", req.SourceIssue, assignee)
		return false
	}

	// The bead is still open and THIS worker still holds it. That alone
	// doesn't mean recovery is unnecessary (the worker may be genuinely
	// dead, not merely idle) — session liveness is the tiebreaker for this
	// one ambiguous case only.
	if !status.IsTerminal() && stillAssignedHere {
		if sessionAlive == nil {
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
	logf("[Engineer] Worker %s no longer holds %s — reopened for redispatch (%s)\n", polecatName, req.SourceIssue, MergeRejectionNoteMarker)

	if sendMail == nil {
		logf("[Engineer] Warning: no mail router — %s reopened but RECOVERED_BEAD not sent; deacon patrol will pick it up from ready queue\n", req.SourceIssue)
		return false
	}

	var rejectionLines strings.Builder
	if len(req.Findings) > 0 {
		fmt.Fprintf(&rejectionLines, "Rejection-Findings: %s\n", strings.Join(rejectionFindingIDs(req.Findings), ","))
	}
	if req.Summary != "" {
		fmt.Fprintf(&rejectionLines, "Rejection-Summary: %s\n", req.Summary)
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
%s
The source bead has been reopened with %s notes.
Please re-dispatch. The branch survives on origin, so the next polecat
can check it out and make a targeted fix instead of starting over.`,
			req.SourceIssue, req.RigName, polecatName, req.MRID, req.Branch,
			req.FailureType, req.AttemptNumber, rejectionLines.String(), MergeRejectionNoteMarker),
	}
	if err := sendMail(msg); err != nil {
		logf("[Engineer] Warning: failed to send RECOVERED_BEAD for %s: %v\n", req.SourceIssue, err)
		return false
	}
	logf("[Engineer] Sent RECOVERED_BEAD %s to deacon for redispatch\n", req.SourceIssue)
	return true
}

// newDeadWorkerRecoverer builds the standard (tmux + mail) wiring for
// recoverRejectedMRDeadWorker, shared by every reject path — the Engineer's
// automatic build/test-failure poller and the Manager's manual `gt mq
// reject` CLI path — so both routes maintain source-issue state identically
// (gt-2usm: the CLI path used to bypass recovery entirely).
//
// beadsClient is optional: pass the caller's own injected client (e.g. the
// Engineer's e.beads, which tests point at an in-process store via
// beads.NewWithStore) so recovery observes the same beads backend the rest
// of the caller's tests exercise, instead of silently constructing its own
// beads.New(r.BeadsPath()) and shelling out to a real bd subprocess. Pass
// nil to fall back to that default construction.
func newDeadWorkerRecoverer(r *rig.Rig, router *mail.Router, out io.Writer, beadsClient rejectedSourceBeads) func(deadWorkerRecoveryRequest) bool {
	if beadsClient == nil {
		beadsClient = beads.New(r.BeadsPath())
	}
	return func(req deadWorkerRecoveryRequest) bool {
		sessionAlive := func(polecatName string) (bool, error) {
			t := tmux.NewTmux()
			return t.HasSession(session.PolecatSessionName(session.PrefixFor(r.Name), polecatName))
		}
		var send func(*mail.Message) error
		if router != nil {
			send = router.Send
		}
		return recoverRejectedMRDeadWorker(beadsClient, sessionAlive, send, out, req)
	}
}
