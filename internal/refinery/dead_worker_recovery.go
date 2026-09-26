package refinery

import (
	"fmt"
	"io"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// MergeRejectionNoteMarker is the canonical vocabulary written into source-bead
// notes when a branch-caused merge rejection is recorded; see
// dispatch.MergeRejectionNoteMarker, where it is defined so the convoy feeders
// can read it without importing refinery (gt-ghyfx).
const MergeRejectionNoteMarker = dispatch.MergeRejectionNoteMarker

// Rejection failure classes: what a rejection was actually about, the
// vocabulary `gt mq reject --failure-type` accepts. These are strings rather
// than refinery.FailureType because that type classifies what the merge
// machinery did (checkout, push, conflict) while these classify what the
// reviewer found wrong with the branch.
//
// A rejection's class is durable history a later reader triages by, so it is
// never defaulted: a caller that cannot say what broke records no class
// (gt-1jig).
const (
	FailureTypeEditorial = "editorial"
	FailureTypeTests     = "tests"
	FailureTypeBuild     = "build"
	FailureTypeLint      = "lint"
	FailureTypeTypecheck = "typecheck"
)

// failureTypeAliases maps the spellings other parts of the system use for a
// class onto the canonical one, so one class cannot become two entries in a
// bead's history. mol-refinery-patrol's FIX_NEEDED bodies write "om-editorial".
var failureTypeAliases = map[string]string{
	"om-editorial": FailureTypeEditorial,
}

// failureTypes is the accepted vocabulary, in the order help and error text
// list it.
var failureTypes = []string{
	FailureTypeTests, FailureTypeBuild, FailureTypeLint, FailureTypeTypecheck, FailureTypeEditorial,
}

// ClassifyRejectionFailureType normalizes a caller-supplied failure class,
// returning "" for an empty one (the caller cannot classify this rejection)
// and an error for a value outside the vocabulary.
func ClassifyRejectionFailureType(class string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(class))
	if normalized == "" {
		return "", nil
	}
	if canonical, ok := failureTypeAliases[normalized]; ok {
		return canonical, nil
	}
	for _, known := range failureTypes {
		if normalized == known {
			return known, nil
		}
	}
	return "", fmt.Errorf("unknown failure type %q: want one of %s", class, strings.Join(failureTypes, ", "))
}

// deadWorkerRecoveryRequest describes a branch-caused merge rejection whose
// worker polecat may no longer have a live session.
type deadWorkerRecoveryRequest struct {
	MRID          string
	Branch        string
	Target        string
	SourceIssue   string
	Worker        string // as recorded on the MR, e.g. "polecats/nux"
	RigName       string
	ErrorMsg      string
	AttemptNumber int

	// FailureType is what this rejection was actually about, in the vocabulary
	// ClassifyRejectionFailureType accepts. Empty means the caller did not
	// classify it: the note and the RECOVERED_BEAD mail then omit the class
	// rather than carry a defaulted one (gt-1jig).
	FailureType string

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

	// Receipt carries the om review score and carried-forward unresolved
	// finding ids behind this rejection, present only when the rejection
	// came from an actual om review (the batch path's automatic gt mq
	// review) rather than a manual `gt mq reject` with no measurable
	// verdict. When set, it travels onto the source bead notes and the
	// RECOVERED_BEAD mail so `gt deacon redispatch` can recover it and run
	// deacon.RedispatchEditorial's convergence/max_attempts rule instead of
	// falling back to the plain attempt-count Redispatch (om-gate T10).
	Receipt *EditorialReceipt
}

// EditorialReceipt is the minimal signal from an om editorial verdict that
// the deacon's redispatch gate needs to decide whether a resubmit is
// converging — mirrors deacon.ReceiptSummary, kept as its own type here so
// this package does not import internal/deacon for a two-field struct.
type EditorialReceipt struct {
	// Score is the om review score behind this rejection.
	Score float64

	// Unresolved is the finding ids om reports as still open relative to
	// this bead's full rejection history — om computes this itself from the
	// bead's accumulated MERGE REJECTION notes (BuildPriorFindings), so it
	// already reflects carry-forward across every prior attempt, not just
	// the immediately preceding one.
	Unresolved []string
}

// RejectionFinding is one om editorial finding attached to a merge
// rejection. ID is the finding's stable 12-hex id (om: first 12 hex of
// sha256(path|title)), matching the id om's --prior-findings classification
// keys on.
//
// The id must be unique per finding. The reviewed head sha is not an id: one
// rejection's findings are all found on that head, so using it collides the
// moment a rejection reports two, and om rejects a repeated id outright —
// fail-closed, so the colliding note blocks every later review of the source
// issue, not just the attempt that wrote it (gt-2ok0).
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

// RejectionFindingsFromNote converts an om verdict's raw findings into the
// RejectionFinding shape a deadWorkerRecoveryRequest carries forward. The
// two types track the same fields (om's own verdict finding shape) so this
// is a straight field-for-field copy.
func RejectionFindingsFromNote(findings []editorial.Finding) []RejectionFinding {
	if len(findings) == 0 {
		return nil
	}
	out := make([]RejectionFinding, len(findings))
	for i, f := range findings {
		out[i] = RejectionFinding{
			ID:       f.ID,
			Severity: f.Severity,
			Path:     f.Path,
			Line:     f.Line,
			Title:    f.Title,
		}
	}
	return out
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

// rejectionAttempt is the attempt number a rejection is recorded as. A caller
// that numbers its attempts (mol-refinery-patrol counts the source bead's
// "MERGE REJECTION (attempt" entries) passes the number it also puts in the
// reason, so the note header and the reason name the same attempt. The MR's
// RetryCount is a conflict-retry count and is only a fallback for callers with
// no attempt of their own (gt-s4f6).
func rejectionAttempt(mr *MergeRequest, rec *RejectionRecord) int {
	if rec != nil && rec.Attempt > 0 {
		return rec.Attempt
	}
	return mr.RetryCount + 1
}

// noteField collapses a free-text note field onto one line. Titles, paths, and
// the reject reason are model- or agent-supplied, and a newline in one of them
// injects whole lines into the notes: a forged finding line, a "MERGE
// REJECTION (attempt" marker that inflates the attempt count, or a receipt
// line `gt deacon redispatch` reads back (gt-s4f6).
func noteField(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func formatMergeRejectionNote(req deadWorkerRecoveryRequest) string {
	attempt := req.AttemptNumber
	if attempt < 1 {
		attempt = 1
	}
	// The class stands in the header only when the caller classified it, so an
	// unclassified rejection reads as one instead of naming a subsystem that
	// had nothing to do with it (gt-1jig).
	header := fmt.Sprintf("%s (attempt %d): ", MergeRejectionNoteMarker, attempt)
	if class := noteField(req.FailureType); class != "" {
		header += class + " - "
	}
	note := header + noteField(req.ErrorMsg) +
		fmt.Sprintf("\nBranch: %s\nTarget: %s\nMR: %s", req.Branch, req.Target, req.MRID)
	for _, f := range req.Findings {
		note += fmt.Sprintf("\n- id:%s sev:%s %s:%d — %s",
			noteField(f.ID), noteField(f.Severity), noteField(f.Path), f.Line, noteField(f.Title))
	}
	// Score/Unresolved are the machine-readable receipt `gt deacon
	// redispatch` greps back out (deacon.ParseEditorialReceiptFromNotes) to
	// run RedispatchEditorial's convergence rule. Only written when this
	// rejection actually carries an om verdict.
	if req.Receipt != nil {
		note += fmt.Sprintf("\nScore: %.4f", req.Receipt.Score)
		if len(req.Receipt.Unresolved) > 0 {
			note += fmt.Sprintf("\nUnresolved: %s", strings.Join(req.Receipt.Unresolved, ","))
		}
	}
	return note
}

// appendRejectionNote adds note to a source bead's notes unless an identical
// block is already there, so a refinery re-gating an unchanged branch on every
// poll does not pile up copies of one rejection.
//
// The write appends (`--append-notes`) rather than replacing the field from
// the copy read here: a rejection is usually recorded while the owning polecat
// is still alive and appending its own notes, and a replace silently drops
// whatever landed between the read and the write (gt-nxvg).
//
// Dead-worker recovery and the Manager's rejection record both write through
// here so their blocks are byte-identical: the notes are read back by
// editorial.BuildPriorFindings, and a rejection recorded in a second format is
// one the next attempt never sees (gt-s4f6).
func appendRejectionNote(bd rejectedSourceBeads, issue *beads.Issue, note string) error {
	if issue == nil || strings.TrimSpace(note) == "" {
		return nil
	}
	if strings.Contains(issue.Notes, note) {
		return nil
	}
	if _, err := bd.Run("update", issue.ID, "--append-notes", note); err != nil {
		return fmt.Errorf("appending rejection note to %s: %w", issue.ID, err)
	}
	return nil
}

// recordRejectionFindings appends the MERGE REJECTION note for req onto its
// source bead's notes, read through an injected client so the write is
// testable without a live beads store (the seam newDeadWorkerRecoverer exists
// for). The bead is neither reopened nor mailed: the caller's own redispatch
// path owns that, and this is the record it leaves behind.
//
// It reports a write it could not make. The durable record IS the point of the
// call, so a caller that swallows the error exits 0 on a rejection whose
// findings never reached the next attempt — the failure gt-s4f6 is about.
func recordRejectionFindings(bd rejectedSourceBeads, req deadWorkerRecoveryRequest) error {
	if strings.TrimSpace(req.SourceIssue) == "" {
		return nil
	}
	issue, err := bd.Show(req.SourceIssue)
	if err != nil {
		return fmt.Errorf("reading source bead %s to record rejection findings: %w", req.SourceIssue, err)
	}
	if issue == nil {
		return fmt.Errorf("reading source bead %s to record rejection findings: not found", req.SourceIssue)
	}
	return appendRejectionNote(bd, issue, formatMergeRejectionNote(req))
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
	"cancel", // prefix match: covers both spellings of canceled
	"wontfix",
	"won't fix",
	"not-planned",
	"obsolete",
	"abandoned",
	"merged in ", // convention used elsewhere for "closed because merged via another MR/dep"
}

// isDeliberateTerminalCloseReason reports whether a closed bead's close_reason
// indicates the closure was intentional (canceled/superseded/duplicate/already
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
// or when a closed bead's own close_reason marks it deliberately canceled,
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
	// also be closed because an operator deliberately canceled/superseded
	// it, or because the work already landed via a different MR (gt-pvwy —
	// the superseded-duplicate case that motivated this check happened for
	// real: gt-wisp-bakv, closed as superseded, would have been resurrected
	// by 'gt mq reject' had this gate not existed). The close reason is the
	// cheap, decisive signal a status/assignee check alone cannot give.
	if status.IsTerminal() {
		if reason := strings.TrimSpace(issue.CloseReason); reason != "" && isDeliberateTerminalCloseReason(reason) {
			logf("[Engineer] Source bead %s closed deliberately (close_reason=%q) — skipping dead-worker recovery to avoid resurrecting canceled/superseded/already-merged work\n", req.SourceIssue, reason)
			return false
		}
	}

	// If a live reassignment already happened (another polecat owns the bead),
	// leave it alone — the deacon or a fresh worker is already on it. This
	// used to require a non-empty assignee to fire, but status.IsAssigned()
	// (hooked/in_progress) is itself evidence of a live hold: an assignee
	// that's missing or doesn't match this worker on an assigned-status bead
	// means SOME holder we can't positively identify has it, not that nobody
	// does. Falling through to the unconditional recovery below in that case
	// is exactly how a hooked-but-momentarily-unassigned bead had its
	// assignee cleared out from under an actively working polecat (gt-on0d).
	if !status.IsTerminal() && !stillAssignedHere && (assignee != "" || status.IsAssigned()) {
		if assignee != "" {
			logf("[Engineer] Source bead %s already reassigned to %s — skipping dead-worker recovery\n", req.SourceIssue, assignee)
		} else {
			logf("[Engineer] Source bead %s is %s with no confirmed holder — skipping dead-worker recovery rather than clear a possibly-live hold\n", req.SourceIssue, status)
		}
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
	// Branch: line. A write that fails is warned about, not fatal: the reopen
	// and the RECOVERED_BEAD mail below carry the same findings, and a bead
	// left closed because its note would not write is the orphan this whole
	// function exists to avoid.
	if err := appendRejectionNote(bd, issue, formatMergeRejectionNote(req)); err != nil {
		logf("[Engineer] Warning: %v\n", err)
	}

	// Reopen with no assignee so the deacon's Redispatch (which requires
	// status=open) can re-sling it. bd's reassign fence is answerable with
	// --force only where it can have fired: a non-terminal bead carrying an
	// assignee, which is the dead holder's claim and nothing else.
	if err := reopenSourceForRedispatch(bd, req.SourceIssue, !status.IsTerminal() && assignee != "", logf); err != nil {
		logf("[Engineer] Warning: failed to reopen source bead %s for redispatch: %v\n", req.SourceIssue, err)
		escalateStrandedSource(sendMail, req, polecatName, err, logf)
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
	if req.Receipt != nil {
		fmt.Fprintf(&rejectionLines, "Rejection-Score: %.4f\n", req.Receipt.Score)
		if len(req.Receipt.Unresolved) > 0 {
			fmt.Fprintf(&rejectionLines, "Rejection-Unresolved: %s\n", strings.Join(req.Receipt.Unresolved, ","))
		}
	}

	// The class is a line only when the caller classified it: a recipient
	// triages by this field, so one naming a failure nobody established is
	// worse than one that admits it does not know (gt-1jig).
	failureLine := ""
	if class := strings.TrimSpace(req.FailureType); class != "" {
		failureLine = fmt.Sprintf("Failure-Type: %s\n", class)
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
%sAttempt: %d
%s
The source bead has been reopened with %s notes.
Please re-dispatch. The branch survives on origin, so the next polecat
can check it out and make a targeted fix instead of starting over.`,
			req.SourceIssue, req.RigName, polecatName, req.MRID, req.Branch,
			failureLine, req.AttemptNumber, rejectionLines.String(), MergeRejectionNoteMarker),
	}
	if err := sendMail(msg); err != nil {
		logf("[Engineer] Warning: failed to send RECOVERED_BEAD for %s: %v\n", req.SourceIssue, err)
		return false
	}
	logf("[Engineer] Sent RECOVERED_BEAD %s to deacon for redispatch\n", req.SourceIssue)
	return true
}

// reopenSourceForRedispatch clears the source bead's assignee and returns it
// to open, so the deacon's Redispatch — which only re-slings a bead whose
// status is open — has something to act on.
//
// forcePastClaim retries the reopen with bd's --force, for the one refusal
// recovery can answer: bd fences a plain reassign that strips another actor's
// in_progress claim, and here that claim belongs to a polecat whose dead
// session the caller already established. bd cannot see the session, so the
// retry is how the established fact reaches it. A refusal anywhere else is a
// real failure, and --force would only paper over it. Abandoning the reopen
// instead strands the bead under a worker that will never act on it while the
// RECOVERED_BEAD mail below — the only signal that re-slings it — goes unsent
// (gt-mabxx).
func reopenSourceForRedispatch(bd rejectedSourceBeads, id string, forcePastClaim bool, logf func(string, ...interface{})) error {
	openStatus := string(beads.StatusOpen)
	emptyAssignee := ""
	opts := beads.UpdateOptions{Status: &openStatus, Assignee: &emptyAssignee}

	err := bd.Update(id, opts)
	if err == nil || !forcePastClaim {
		return err
	}

	opts.Force = true
	if forceErr := bd.Update(id, opts); forceErr != nil {
		return fmt.Errorf("plain update: %w; retry with --force: %v", err, forceErr)
	}
	logf("[Engineer] Source bead %s: reopen refused (%v); cleared the dead holder's claim with --force\n", id, err)
	return nil
}

// escalateStrandedSource mails the mayor about a source bead recovery could
// not reopen, so a rejection is not recorded as handled while the bead stays
// held by a worker that cannot act on it.
//
// Nothing else covers that state: the RECOVERED_BEAD mail requires an open
// bead, the deacon re-dispatches only from that mail, and the witness's
// ready-queue sweep only sees beads that are already open. A mail rather than
// the nudge routine refinery signals use, because the record is what a later
// reader needs. Best-effort: the caller has already logged the reopen failure,
// so a mail that will not send is a warning rather than a new failure
// (gt-mabxx).
func escalateStrandedSource(sendMail func(*mail.Message) error, req deadWorkerRecoveryRequest, polecatName string, reopenErr error, logf func(string, ...interface{})) {
	if sendMail == nil {
		return
	}
	msg := &mail.Message{
		From:     req.RigName + "/refinery",
		To:       "mayor/",
		Subject:  fmt.Sprintf("STRANDED_BEAD %s", req.SourceIssue),
		Priority: mail.PriorityHigh,
		Body: fmt.Sprintf(`Merge rejection with no live worker, and the source bead could not be reopened.

Bead: %s
Polecat: %s/polecats/%s
MR: %s
Branch: %s
Attempt: %d
Reopen: %v

No RECOVERED_BEAD was sent: the deacon re-dispatches only an open bead, and
this one is still held by the worker that cannot act on it. Nothing patrols
for a bead in that state, so it needs a hand — bd reclaim for a stale lease,
or bd update %s --status=open --assignee= --force. The branch survives on
origin.`, req.SourceIssue, req.RigName, polecatName, req.MRID, req.Branch,
			req.AttemptNumber, reopenErr, req.SourceIssue),
	}
	if err := sendMail(msg); err != nil {
		logf("[Engineer] Warning: failed to send STRANDED_BEAD for %s: %v\n", req.SourceIssue, err)
	}
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
