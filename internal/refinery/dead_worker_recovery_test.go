package refinery

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
)

type fakeRejectedBeads struct {
	issue     *beads.Issue
	showErr   error
	runCalls  [][]string
	runErr    error
	updates   []beads.UpdateOptions
	updateErr error

	// liveClaim models bd's AssigneeNotStolen fence: a plain update that
	// reassigns an in_progress bead out of another actor's claim is refused,
	// and only Force gets past it (gt-mabxx).
	liveClaim bool
}

func (f *fakeRejectedBeads) Show(id string) (*beads.Issue, error) {
	if f.showErr != nil {
		return nil, f.showErr
	}
	return f.issue, nil
}

func (f *fakeRejectedBeads) Update(id string, opts beads.UpdateOptions) error {
	f.updates = append(f.updates, opts)
	if f.updateErr != nil {
		return f.updateErr
	}
	if f.liveClaim && !opts.Force && opts.Assignee != nil && f.issue != nil &&
		*opts.Assignee != f.issue.Assignee {
		return fmt.Errorf("cannot reassign %s: held by %q (in_progress)", id, f.issue.Assignee)
	}
	return nil
}

func (f *fakeRejectedBeads) Run(args ...string) ([]byte, error) {
	f.runCalls = append(f.runCalls, args)
	if f.runErr != nil {
		return nil, f.runErr
	}
	// Model `bd update <id> --append-notes <note>`, so a test can assert the
	// bead's end state rather than one call's argv. The write is an append
	// because it runs while the owning polecat may still be appending its own
	// notes, and a replace drops whatever landed between read and write
	// (gt-nxvg).
	if note, ok := appendNotesArg(args); ok && f.issue != nil {
		if existing := strings.TrimSpace(f.issue.Notes); existing != "" {
			f.issue.Notes = existing + "\n\n" + note
		} else {
			f.issue.Notes = note
		}
	}
	return nil, nil
}

// appendNotesArg returns the note of a `bd update ... --append-notes <note>`
// argv, and whether the call is one.
func appendNotesArg(args []string) (string, bool) {
	for i, a := range args {
		if a == "--append-notes" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func deadWorkerReq() deadWorkerRecoveryRequest {
	return deadWorkerRecoveryRequest{
		MRID:          "gt-mr1",
		Branch:        "polecat/nux/gt-src1+abc123",
		Target:        "main",
		SourceIssue:   "gt-src1",
		Worker:        "polecats/nux",
		RigName:       "testrig",
		FailureType:   "tests",
		ErrorMsg:      "editorial gate: request_changes (score 0.05)",
		AttemptNumber: 1,
	}
}

func deadSession(string) (bool, error) { return false, nil }
func liveSession(string) (bool, error) { return true, nil }

func TestRecoverRejectedMRDeadWorker_ReopensAndMailsDeacon(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{
		ID:     "gt-src1",
		Status: "closed",
		Notes:  "existing findings",
	}}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}
	var out bytes.Buffer

	recovered := recoverRejectedMRDeadWorker(bd, deadSession, sendMail, &out, deadWorkerReq())

	if !recovered {
		t.Fatalf("expected recovery, got false; output:\n%s", out.String())
	}

	// Rejection note appended with the canonical marker, prior notes kept,
	// and the branch recorded so the resume path can find the work.
	if len(bd.runCalls) != 1 {
		t.Fatalf("expected 1 notes update, got %d", len(bd.runCalls))
	}
	if _, ok := appendNotesArg(bd.runCalls[0]); !ok {
		t.Errorf("notes write is not an append, so a live polecat's own notes can be lost (gt-nxvg): %v", bd.runCalls[0])
	}
	notesArg := bd.issue.Notes
	if !strings.Contains(notesArg, MergeRejectionNoteMarker+" (attempt 1)") {
		t.Errorf("notes missing rejection marker: %q", notesArg)
	}
	if !strings.Contains(notesArg, "existing findings") {
		t.Errorf("notes update clobbered existing notes: %q", notesArg)
	}
	if !strings.Contains(notesArg, "Branch: polecat/nux/gt-src1+abc123") {
		t.Errorf("notes missing branch line: %q", notesArg)
	}

	// Bead reopened with assignee cleared.
	if len(bd.updates) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(bd.updates))
	}
	if bd.updates[0].Status == nil || *bd.updates[0].Status != "open" {
		t.Errorf("expected status reset to open, got %+v", bd.updates[0].Status)
	}
	if bd.updates[0].Assignee == nil || *bd.updates[0].Assignee != "" {
		t.Errorf("expected assignee cleared, got %+v", bd.updates[0].Assignee)
	}

	// RECOVERED_BEAD mail to deacon, with the Polecat: line the deacon
	// parses the source rig from.
	if len(sent) != 1 {
		t.Fatalf("expected 1 mail, got %d", len(sent))
	}
	if sent[0].To != "deacon/" {
		t.Errorf("mail To = %q, want deacon/", sent[0].To)
	}
	if sent[0].Subject != "RECOVERED_BEAD gt-src1" {
		t.Errorf("mail Subject = %q", sent[0].Subject)
	}
	if !strings.Contains(sent[0].Body, "Polecat: testrig/polecats/nux") {
		t.Errorf("mail body missing Polecat line:\n%s", sent[0].Body)
	}
	if beadID, ok := parseRecoveredSubject(sent[0].Subject); !ok || beadID != "gt-src1" {
		t.Errorf("subject not parseable as RECOVERED_BEAD: %q", sent[0].Subject)
	}
}

// parseRecoveredSubject mirrors deacon.ParseRecoveredBeadSubject to keep the
// subject contract pinned without an import cycle.
func parseRecoveredSubject(subject string) (string, bool) {
	const prefix = "RECOVERED_BEAD "
	if !strings.HasPrefix(subject, prefix) {
		return "", false
	}
	id := strings.TrimSpace(strings.TrimPrefix(subject, prefix))
	return id, id != ""
}

// TestRecoverRejectedMRDeadWorker_ClosedSourceIssue_AliveSession_StillRecovers
// is the direct gt-2usm regression test: a persistent polecat that already
// ran gt done (closing its source bead) keeps a reusable, live-but-inert
// tmux session. Session liveness must NOT gate recovery here — the bead's
// own closed status is what proves the worker no longer holds it.
func TestRecoverRejectedMRDeadWorker_ClosedSourceIssue_AliveSession_StillRecovers(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed"}}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}

	if !recoverRejectedMRDeadWorker(bd, liveSession, sendMail, nil, deadWorkerReq()) {
		t.Fatal("expected recovery for a closed source issue even with a live session")
	}
	if len(bd.updates) != 1 {
		t.Fatalf("expected bead reopened, got %d updates", len(bd.updates))
	}
	if bd.updates[0].Status == nil || *bd.updates[0].Status != "open" {
		t.Errorf("expected status reset to open, got %+v", bd.updates[0].Status)
	}
	if bd.updates[0].Assignee == nil || *bd.updates[0].Assignee != "" {
		t.Errorf("expected assignee cleared, got %+v", bd.updates[0].Assignee)
	}
	if len(sent) != 1 {
		t.Fatalf("expected RECOVERED_BEAD mail, got %d", len(sent))
	}
}

// TestRecoverRejectedMRDeadWorker_SupersededCloseReason_NoAction is the
// gt-pvwy regression test: the exact gt-wisp-bakv incident. A source bead
// closed with a deliberate supersede/duplicate close_reason must never be
// resurrected by 'gt mq reject' even though it reads as "closed, unassigned"
// exactly like the "worker finished it" case this recovery targets.
func TestRecoverRejectedMRDeadWorker_SupersededCloseReason_NoAction(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{
		ID:          "gt-src1",
		Status:      "closed",
		CloseReason: "superseded by gt-me9t",
	}}
	sendMail := func(m *mail.Message) error {
		t.Fatal("mail should not be sent for a deliberately superseded bead")
		return nil
	}

	if recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, deadWorkerReq()) {
		t.Fatal("expected no recovery for a bead closed as superseded")
	}
	if len(bd.updates) != 0 || len(bd.runCalls) != 0 {
		t.Fatal("expected no bead mutations for a superseded bead")
	}
}

// TestRecoverRejectedMRDeadWorker_DeliberateCloseReasons covers the other
// close_reason shapes an operator or another MR's success can leave behind,
// all of which must skip recovery the same way superseded does.
func TestRecoverRejectedMRDeadWorker_DeliberateCloseReasons(t *testing.T) {
	t.Parallel()
	reasons := []string{
		"Duplicate of gt-abc1",
		"cancelled: no longer needed",
		"Canceled per mayor directive",
		"wontfix",
		"Won't Fix — not a bug",
		"not-planned for this cycle",
		"obsolete after redesign",
		"abandoned",
		"Merged in gt-wisp-iryq",
	}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			bd := &fakeRejectedBeads{issue: &beads.Issue{
				ID:          "gt-src1",
				Status:      "closed",
				CloseReason: reason,
			}}
			if recoverRejectedMRDeadWorker(bd, deadSession, nil, nil, deadWorkerReq()) {
				t.Fatalf("expected no recovery for close_reason %q", reason)
			}
			if len(bd.updates) != 0 || len(bd.runCalls) != 0 {
				t.Fatalf("expected no bead mutations for close_reason %q", reason)
			}
		})
	}
}

// TestRecoverRejectedMRDeadWorker_ClosedNoReason_StillRecovers guards against
// over-correcting: a closed bead with no close_reason (or an ordinary one)
// is still the "worker finished it" case and must keep recovering — the
// close_reason gate only fires for markers that signal deliberate closure.
func TestRecoverRejectedMRDeadWorker_ClosedNoReason_StillRecovers(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed"}}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}

	if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, deadWorkerReq()) {
		t.Fatal("expected recovery for a closed bead with no close_reason")
	}
	if len(sent) != 1 {
		t.Fatalf("expected RECOVERED_BEAD mail, got %d", len(sent))
	}
}

// TestFormatMergeRejectionNote_FindingsAppendIDLines is gt-zdxn acceptance
// criterion 1: a rejection carrying findings gains one '- id:' line per
// finding on the source bead notes, in the format the T5 prior-findings
// builder parses.
func TestFormatMergeRejectionNote_FindingsAppendIDLines(t *testing.T) {
	t.Parallel()
	req := deadWorkerReq()
	req.Findings = []RejectionFinding{
		{ID: "abc123def456", Severity: "major", Path: "internal/foo.go", Line: 42, Title: "missing nil check"},
		{ID: "789012345678", Severity: "minor", Path: "internal/bar.go", Line: 7, Title: "unused import"},
	}

	note := formatMergeRejectionNote(req)

	want1 := "- id:abc123def456 sev:major internal/foo.go:42 — missing nil check"
	want2 := "- id:789012345678 sev:minor internal/bar.go:7 — unused import"
	if !strings.Contains(note, want1) {
		t.Errorf("note missing first finding line:\nwant substring: %s\ngot: %s", want1, note)
	}
	if !strings.Contains(note, want2) {
		t.Errorf("note missing second finding line:\nwant substring: %s\ngot: %s", want2, note)
	}
	if got := strings.Count(note, "- id:"); got != 2 {
		t.Errorf("expected exactly 2 '- id:' lines, got %d in:\n%s", got, note)
	}
}

// TestFormatMergeRejectionNote_NoFindings_NoIDLines guards the non-editorial
// (build/test failure) path: no findings means no '- id:' lines are added,
// preserving the original note format exactly.
func TestFormatMergeRejectionNote_NoFindings_NoIDLines(t *testing.T) {
	t.Parallel()
	note := formatMergeRejectionNote(deadWorkerReq())
	if strings.Contains(note, "- id:") {
		t.Errorf("expected no finding lines when Findings is empty, got:\n%s", note)
	}
}

// TestRecoverRejectedMRDeadWorker_FindingsTravelToNotesAndMail is gt-zdxn
// acceptance criteria 1 and 2 end-to-end: a rejection with two findings puts
// two '- id:' lines on the source bead notes, and the RECOVERED_BEAD mail
// body carries both ids under 'Rejection-Findings:' plus 'Rejection-Summary:'.
func TestRecoverRejectedMRDeadWorker_FindingsTravelToNotesAndMail(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed"}}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}

	req := deadWorkerReq()
	req.Summary = "2 findings: 1 major, 1 minor"
	req.Findings = []RejectionFinding{
		{ID: "abc123def456", Severity: "major", Path: "internal/foo.go", Line: 42, Title: "missing nil check"},
		{ID: "789012345678", Severity: "minor", Path: "internal/bar.go", Line: 7, Title: "unused import"},
	}

	if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, req) {
		t.Fatal("expected recovery")
	}

	if len(bd.runCalls) != 1 {
		t.Fatalf("expected 1 notes update, got %d", len(bd.runCalls))
	}
	notesArg := bd.issue.Notes
	if got := strings.Count(notesArg, "- id:"); got != 2 {
		t.Errorf("expected 2 '- id:' lines in notes, got %d:\n%s", got, notesArg)
	}

	if len(sent) != 1 {
		t.Fatalf("expected 1 mail, got %d", len(sent))
	}
	body := sent[0].Body
	if !strings.Contains(body, "Rejection-Findings: abc123def456,789012345678") {
		t.Errorf("mail body missing Rejection-Findings line:\n%s", body)
	}
	if !strings.Contains(body, "Rejection-Summary: 2 findings: 1 major, 1 minor") {
		t.Errorf("mail body missing Rejection-Summary line:\n%s", body)
	}
}

// TestFormatMergeRejectionNote_ReceiptAppendsScoreLine is om-gate T10's
// wiring fix (gt-j6ez): a rejection carrying an EditorialReceipt writes a
// machine-parseable Score:/Unresolved: pair onto the bead's notes, which
// deacon.ParseEditorialReceiptFromNotes greps back out so `gt deacon
// redispatch` can run RedispatchEditorial instead of Redispatch.
func TestFormatMergeRejectionNote_ReceiptAppendsScoreLine(t *testing.T) {
	t.Parallel()
	req := deadWorkerReq()
	req.Receipt = &EditorialReceipt{Score: 0.42, Unresolved: []string{"abc123def456", "789012345678"}}

	note := formatMergeRejectionNote(req)

	if !strings.Contains(note, "Score: 0.4200") {
		t.Errorf("note missing Score line:\n%s", note)
	}
	if !strings.Contains(note, "Unresolved: abc123def456,789012345678") {
		t.Errorf("note missing Unresolved line:\n%s", note)
	}
}

// TestFormatMergeRejectionNote_NoReceipt_NoScoreLine guards the non-editorial
// and manual-reject paths: no Receipt means no Score:/Unresolved: lines.
func TestFormatMergeRejectionNote_NoReceipt_NoScoreLine(t *testing.T) {
	t.Parallel()
	note := formatMergeRejectionNote(deadWorkerReq())
	if strings.Contains(note, "Score:") || strings.Contains(note, "Unresolved:") {
		t.Errorf("expected no Score/Unresolved lines when Receipt is nil, got:\n%s", note)
	}
}

// TestRecoverRejectedMRDeadWorker_ReceiptTravelsToMail extends
// TestRecoverRejectedMRDeadWorker_FindingsTravelToNotesAndMail to the Score/
// Unresolved receipt: the RECOVERED_BEAD mail body must carry
// Rejection-Score and Rejection-Unresolved lines whenever the rejection has
// one, so deacon.ParseEditorialReceiptFromNotes (reading the persisted bead
// notes) and a human reading the mail see the same data.
func TestRecoverRejectedMRDeadWorker_ReceiptTravelsToMail(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed"}}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}

	req := deadWorkerReq()
	req.Receipt = &EditorialReceipt{Score: 0.42, Unresolved: []string{"abc123def456"}}

	if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, req) {
		t.Fatal("expected recovery")
	}
	if len(sent) != 1 {
		t.Fatalf("expected 1 mail, got %d", len(sent))
	}
	body := sent[0].Body
	if !strings.Contains(body, "Rejection-Score: 0.4200") {
		t.Errorf("mail body missing Rejection-Score line:\n%s", body)
	}
	if !strings.Contains(body, "Rejection-Unresolved: abc123def456") {
		t.Errorf("mail body missing Rejection-Unresolved line:\n%s", body)
	}
}

// TestRecoverRejectedMRDeadWorker_NoFindings_NoRejectionLinesInMail guards
// the non-editorial path: mail body carries no Rejection-Findings/-Summary
// lines when the rejection has none.
func TestRecoverRejectedMRDeadWorker_NoFindings_NoRejectionLinesInMail(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed"}}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}

	if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, deadWorkerReq()) {
		t.Fatal("expected recovery")
	}
	if len(sent) != 1 {
		t.Fatalf("expected 1 mail, got %d", len(sent))
	}
	body := sent[0].Body
	if strings.Contains(body, "Rejection-Findings:") || strings.Contains(body, "Rejection-Summary:") {
		t.Errorf("mail body should carry no rejection-finding lines when there are none:\n%s", body)
	}
}

func TestIsDeliberateTerminalCloseReason(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":                             false,
		"finished":                     false,
		"done":                         false,
		"superseded by gt-me9t":        true,
		"Duplicate of gt-abc1":         true,
		"cancelled":                    true,
		"canceled by operator":         true,
		"wontfix":                      true,
		"won't fix":                    true,
		"not-planned":                  true,
		"obsolete":                     true,
		"abandoned":                    true,
		"Merged in gt-wisp-iryq":       true,
		"rejected: editorial feedback": false,
	}
	for reason, want := range cases {
		if got := isDeliberateTerminalCloseReason(reason); got != want {
			t.Errorf("isDeliberateTerminalCloseReason(%q) = %v, want %v", reason, got, want)
		}
	}
}

// TestRecoverRejectedMRDeadWorker_OpenAndStillAssigned_AliveSession_NoAction
// covers the genuinely-still-working case: the source bead is open and this
// same worker still holds it, and its session is confirmed alive. Session
// liveness is the correct tiebreaker only here.
func TestRecoverRejectedMRDeadWorker_OpenAndStillAssigned_AliveSession_NoAction(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{
		ID:       "gt-src1",
		Status:   "hooked",
		Assignee: "testrig/polecats/nux",
	}}
	sendMail := func(m *mail.Message) error {
		t.Fatal("mail should not be sent when the worker still holds the assignment and is alive")
		return nil
	}

	if recoverRejectedMRDeadWorker(bd, liveSession, sendMail, nil, deadWorkerReq()) {
		t.Fatal("expected no recovery for a live worker who still holds the assignment")
	}
	if len(bd.updates) != 0 || len(bd.runCalls) != 0 {
		t.Fatal("expected no bead mutations for a live, still-assigned worker")
	}
}

func TestRecoverRejectedMRDeadWorker_LivenessError_NoAction(t *testing.T) {
	t.Parallel()
	// Ambiguous case only: open bead, still assigned to this worker, but we
	// can't tell if it's alive. Liveness only gates this specific case.
	bd := &fakeRejectedBeads{issue: &beads.Issue{
		ID:       "gt-src1",
		Status:   "hooked",
		Assignee: "testrig/polecats/nux",
	}}
	flaky := func(string) (bool, error) { return false, errors.New("tmux exploded") }
	var out bytes.Buffer

	if recoverRejectedMRDeadWorker(bd, flaky, nil, &out, deadWorkerReq()) {
		t.Fatal("expected no recovery when liveness is unknown")
	}
	if len(bd.updates) != 0 {
		t.Fatal("expected no bead mutations when liveness is unknown")
	}
}

func TestRecoverRejectedMRDeadWorker_ReassignedBead_NoAction(t *testing.T) {
	t.Parallel()
	// The bead was already re-slung to another polecat: leave it alone.
	bd := &fakeRejectedBeads{issue: &beads.Issue{
		ID:       "gt-src1",
		Status:   "hooked",
		Assignee: "testrig/polecats/other",
	}}

	if recoverRejectedMRDeadWorker(bd, deadSession, nil, nil, deadWorkerReq()) {
		t.Fatal("expected no recovery for a reassigned bead")
	}
	if len(bd.updates) != 0 || len(bd.runCalls) != 0 {
		t.Fatal("expected no bead mutations for a reassigned bead")
	}
}

// TestRecoverRejectedMRDeadWorker_HookedNoAssignee_NoAction covers the gap
// behind gt-on0d: a bead can read status=hooked (an active molecule is
// attached — someone is working it right now) with an empty or stale
// assignee, e.g. because a prior redispatch didn't populate it, or a
// concurrent reject already touched it. Status=hooked is itself evidence of
// a live hold; recovery must not read the missing/mismatched assignee as
// "nobody holds this" and clear it out from under the actual holder,
// regardless of whether the rejected MR's own worker session is alive or
// dead. The reassigned-skip guard used to require a non-empty assignee to
// fire, so this exact case fell through to the unconditional recovery below.
func TestRecoverRejectedMRDeadWorker_HookedNoAssignee_NoAction(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{
		ID:       "gt-src1",
		Status:   "hooked",
		Assignee: "",
	}}
	sendMail := func(m *mail.Message) error {
		t.Fatal("mail should not be sent for a hooked bead with no confirmed holder match")
		return nil
	}

	if recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, deadWorkerReq()) {
		t.Fatal("expected no recovery for a hooked bead whose assignee doesn't confirm this worker still holds it")
	}
	if len(bd.updates) != 0 || len(bd.runCalls) != 0 {
		t.Fatal("expected no bead mutations for a hooked bead with an unconfirmed holder")
	}
}

// TestRecoverRejectedMRDeadWorker_InProgressNoAssignee_NoAction is the same
// gap on the other status IsAssigned() reports as a live hold.
func TestRecoverRejectedMRDeadWorker_InProgressNoAssignee_NoAction(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{
		ID:       "gt-src1",
		Status:   "in_progress",
		Assignee: "",
	}}

	if recoverRejectedMRDeadWorker(bd, deadSession, nil, nil, deadWorkerReq()) {
		t.Fatal("expected no recovery for an in_progress bead whose assignee doesn't confirm this worker still holds it")
	}
	if len(bd.updates) != 0 || len(bd.runCalls) != 0 {
		t.Fatal("expected no bead mutations for an in_progress bead with an unconfirmed holder")
	}
}

func TestRecoverRejectedMRDeadWorker_DeadWorkerStillAssigned_Recovers(t *testing.T) {
	t.Parallel()
	// Bead still hooked to the dead worker itself: reset it.
	bd := &fakeRejectedBeads{issue: &beads.Issue{
		ID:       "gt-src1",
		Status:   "hooked",
		Assignee: "testrig/polecats/nux",
	}}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}

	if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, deadWorkerReq()) {
		t.Fatal("expected recovery when the dead worker still holds the bead")
	}
	if len(bd.updates) != 1 || len(sent) != 1 {
		t.Fatalf("expected reopen + mail, got %d updates, %d mails", len(bd.updates), len(sent))
	}
}

// TestRecoverRejectedMRDeadWorker_OpenAndUnassigned_Recovers covers the third
// branch of the status/assignee matrix (om flagged this one as untested):
// the bead is open but nobody holds it — neither the "reassigned" skip nor
// the "still assigned, check liveness" path applies, so recovery must still
// proceed and reopen/re-notify.
func TestRecoverRejectedMRDeadWorker_OpenAndUnassigned_Recovers(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "open", Assignee: ""}}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}

	if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, deadWorkerReq()) {
		t.Fatal("expected recovery for an open, unassigned bead")
	}
	if len(bd.updates) != 1 {
		t.Fatalf("expected bead reopened, got %d updates", len(bd.updates))
	}
	if len(sent) != 1 {
		t.Fatalf("expected RECOVERED_BEAD mail, got %d", len(sent))
	}
}

func TestRecoverRejectedMRDeadWorker_MissingFields_NoAction(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed"}}

	req := deadWorkerReq()
	req.Worker = ""
	if recoverRejectedMRDeadWorker(bd, deadSession, nil, nil, req) {
		t.Fatal("expected no recovery without a worker")
	}

	req = deadWorkerReq()
	req.SourceIssue = ""
	if recoverRejectedMRDeadWorker(bd, deadSession, nil, nil, req) {
		t.Fatal("expected no recovery without a source issue")
	}
}

func TestRecoverRejectedMRDeadWorker_MailFailure_ReturnsFalse(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed"}}
	sendMail := func(m *mail.Message) error { return fmt.Errorf("router down") }
	var out bytes.Buffer

	if recoverRejectedMRDeadWorker(bd, deadSession, sendMail, &out, deadWorkerReq()) {
		t.Fatal("expected false when mail fails")
	}
	// The bead should still have been reopened — the deacon patrol can find
	// open unassigned beads even without the mail.
	if len(bd.updates) != 1 {
		t.Fatalf("expected bead reopened despite mail failure, got %d updates", len(bd.updates))
	}
}

func TestRecoverRejectedMRDeadWorker_DuplicateNote_NotAppended(t *testing.T) {
	t.Parallel()
	// The refinery re-gates an unchanged branch on every poll until the
	// redispatch lands; the identical rejection must not pile up in notes.
	req := deadWorkerReq()
	bd := &fakeRejectedBeads{issue: &beads.Issue{
		ID:     "gt-src1",
		Status: "open",
		Notes:  formatMergeRejectionNote(req),
	}}
	sendMail := func(m *mail.Message) error { return nil }

	if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, req) {
		t.Fatal("expected recovery to proceed")
	}
	if len(bd.runCalls) != 0 {
		t.Fatalf("expected no duplicate notes write, got %d", len(bd.runCalls))
	}
}

// TestRecoverRejectedMRDeadWorker_ReopenRefused_RetriesWithForce covers the
// stale-holder case (gt-mabxx): the bead is still in_progress under a dead
// worker, so bd refuses the plain reassign. Recovery has to get past that
// refusal, because the RECOVERED_BEAD mail below it is the only thing that
// re-slings the bead — bd's Refinery sent neither and left the bead held by a
// worker that could never act on it.
func TestRecoverRejectedMRDeadWorker_ReopenRefused_RetriesWithForce(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{
		issue: &beads.Issue{
			ID:       "gt-src1",
			Status:   "in_progress",
			Assignee: "testrig/polecats/nux",
		},
		liveClaim: true,
	}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}
	var out bytes.Buffer

	if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, &out, deadWorkerReq()) {
		t.Fatalf("expected recovery to survive the refused reopen; output:\n%s", out.String())
	}

	if len(bd.updates) != 2 {
		t.Fatalf("expected plain reopen then a forced retry, got %d updates", len(bd.updates))
	}
	if bd.updates[0].Force {
		t.Error("the first reopen should be the plain one; forcing it up front hides the fence the retry exists for")
	}
	if !bd.updates[1].Force {
		t.Error("the retry did not pass Force, so a live-claim refusal is not overridden (gt-mabxx)")
	}

	if len(sent) != 1 {
		t.Fatalf("expected RECOVERED_BEAD, got %d mails", len(sent))
	}
	if sent[0].To != "deacon/" {
		t.Errorf("mail To = %q, want deacon/ — the redispatch signal must still go out", sent[0].To)
	}
}

// TestRecoverRejectedMRDeadWorker_ReopenCannotLand_Escalates covers the other
// half of gt-mabxx: when even the forced reopen fails, the bead is held by a
// worker that cannot act on it and no RECOVERED_BEAD can be sent, so recovery
// must leave a durable trace instead of returning false quietly.
func TestRecoverRejectedMRDeadWorker_ReopenCannotLand_Escalates(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{
		issue:     &beads.Issue{ID: "gt-src1", Status: "in_progress", Assignee: "testrig/polecats/nux"},
		updateErr: fmt.Errorf("bd update gt-src1: dolt unreachable"),
	}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}
	var out bytes.Buffer

	if recoverRejectedMRDeadWorker(bd, deadSession, sendMail, &out, deadWorkerReq()) {
		t.Fatal("expected false when the bead cannot be reopened")
	}

	if len(sent) != 1 {
		t.Fatalf("expected 1 escalated mail, got %d", len(sent))
	}
	if sent[0].To != "mayor/" {
		t.Errorf("escalation To = %q, want mayor/ — the deacon cannot act on a bead that is not open", sent[0].To)
	}
	if !strings.Contains(sent[0].Subject, "gt-src1") {
		t.Errorf("escalation subject %q does not name the stranded bead", sent[0].Subject)
	}
	if strings.HasPrefix(sent[0].Subject, "RECOVERED_BEAD") {
		t.Error("sent RECOVERED_BEAD for a bead that is still held; the deacon skips anything but an open bead")
	}
}

// TestRecoverRejectedMRDeadWorker_ReopenFailureWithoutAClaim_DoesNotForce
// pins the retry's blast radius: --force answers bd's live-claim fence, so it
// is spent only where that fence can have fired. A refusal on a bead carrying
// no claim is a real failure, and forcing past it would hide one.
func TestRecoverRejectedMRDeadWorker_ReopenFailureWithoutAClaim_DoesNotForce(t *testing.T) {
	t.Parallel()
	bd := &fakeRejectedBeads{
		issue:     &beads.Issue{ID: "gt-src1", Status: "closed"},
		updateErr: fmt.Errorf("bd update gt-src1: dolt unreachable"),
	}
	var sent []*mail.Message
	sendMail := func(m *mail.Message) error {
		sent = append(sent, m)
		return nil
	}
	var out bytes.Buffer

	if recoverRejectedMRDeadWorker(bd, deadSession, sendMail, &out, deadWorkerReq()) {
		t.Fatal("expected false when the bead cannot be reopened")
	}

	if len(bd.updates) != 1 {
		t.Fatalf("expected a single unforced reopen attempt, got %d", len(bd.updates))
	}
	if bd.updates[0].Force {
		t.Error("forced a bead that carries no claim; --force is for the dead holder's claim, not for any failure")
	}
	if len(sent) != 1 || sent[0].To != "mayor/" {
		t.Fatalf("expected the stranded-bead escalation, got %d mails", len(sent))
	}
}

func TestWorkerNameFromMR(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"nux":                  "nux",
		"polecats/nux":         "nux",
		"testrig/polecats/nux": "nux",
		"  ":                   "",
		"":                     "",
	}
	for in, want := range cases {
		if got := workerNameFromMR(in); got != want {
			t.Errorf("workerNameFromMR(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewDeadWorkerRecoverer_UsesInjectedBeadsClient asserts the constructor
// honors an injected beads client instead of always shelling out to a real
// bd subprocess against r.BeadsPath() — the test-injection seam om flagged
// as broken for the Engineer's e.beads. A fake client wired here must be the
// one recovery actually calls, provable because it flows through to a
// mutation that only the fake would record correctly.
func TestNewDeadWorkerRecoverer_UsesInjectedBeadsClient(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{Name: "testrig", Path: t.TempDir()}
	fake := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed"}}

	// No router: recoverRejectedMRDeadWorker still reopens the bead before
	// touching mail, so the reopen update is enough to prove which beads
	// client recovery actually reached (a real bd subprocess against
	// r.BeadsPath() would fail against this empty temp dir instead).
	recover := newDeadWorkerRecoverer(r, nil, nil, fake)
	recover(deadWorkerReq())
	if len(fake.updates) != 1 {
		t.Fatalf("expected the injected fake client to receive the reopen update, got %d", len(fake.updates))
	}
}

func TestHandleMRInfoFailure_DeadWorkerRecoveryWired(t *testing.T) {
	// Not t.Parallel(): fakeBDAndGt uses t.Setenv, which panics with a
	// parallel ancestor. HandleMRInfoFailure nudges the polecat and mayor
	// for a non-conflict failure (gt-i0ld) — fake gt on PATH so the test
	// never shells out to the real binary.
	fakeBDAndGt(t)
	// A non-conflict branch failure must consult the dead-worker recovery
	// seam; conflict failures must not (they get a conflict-resolution task).
	workDir := t.TempDir()
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	var buf bytes.Buffer
	e.output = &buf
	e.workDir = workDir

	var gotReq *deadWorkerRecoveryRequest
	e.recoverDeadWorker = func(req deadWorkerRecoveryRequest) bool {
		gotReq = &req
		return true
	}

	mr := &MRInfo{
		ID:          "gt-mr1",
		Branch:      "polecat/nux/gt-src1+abc123",
		Target:      "main",
		SourceIssue: "gt-src1",
		Worker:      "polecats/nux",
		RetryCount:  0,
	}
	e.HandleMRInfoFailure(mr, ProcessResult{Success: false, TestsFailed: true, Error: "gate failed"})

	if gotReq == nil {
		t.Fatal("expected dead-worker recovery to be attempted for a tests failure")
	}
	if gotReq.SourceIssue != "gt-src1" || gotReq.Worker != "polecats/nux" {
		t.Errorf("unexpected recovery request: %+v", gotReq)
	}
	if gotReq.FailureType != "tests" {
		t.Errorf("FailureType = %q, want tests", gotReq.FailureType)
	}
	if gotReq.AttemptNumber != 1 {
		t.Errorf("AttemptNumber = %d, want 1", gotReq.AttemptNumber)
	}
}

func TestHandleMRInfoFailure_SlotTimeout_NoRecovery(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	r := &rig.Rig{Name: "test-rig", Path: workDir}
	e := NewEngineer(r)
	var buf bytes.Buffer
	e.output = &buf
	e.workDir = workDir

	called := false
	e.recoverDeadWorker = func(req deadWorkerRecoveryRequest) bool {
		called = true
		return false
	}

	mr := &MRInfo{ID: "gt-mr1", Worker: "polecats/nux", SourceIssue: "gt-src1"}
	e.HandleMRInfoFailure(mr, ProcessResult{Success: false, SlotTimeout: true, Error: "slot busy"})

	if called {
		t.Fatal("slot timeout must not trigger dead-worker recovery")
	}
}

// TestRecoverRejectedMRDeadWorker_MailCarriesOnlyAStatedClass is gt-1jig on the
// RECOVERED_BEAD mail the deacon triages by: the Failure-Type line appears only
// when the rejection was actually classified, and an unclassified one carries
// no class rather than a defaulted one. The line the mail's other readers use
// (Polecat:, the Attempt count) must survive either way.
func TestRecoverRejectedMRDeadWorker_MailCarriesOnlyAStatedClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		class string
	}{
		{name: "classified", class: FailureTypeBuild},
		{name: "unclassified", class: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bd := &fakeRejectedBeads{issue: &beads.Issue{ID: "gt-src1", Status: "closed"}}
			var sent []*mail.Message
			sendMail := func(m *mail.Message) error {
				sent = append(sent, m)
				return nil
			}
			req := deadWorkerReq()
			req.FailureType = tc.class

			if !recoverRejectedMRDeadWorker(bd, deadSession, sendMail, nil, req) {
				t.Fatal("expected recovery")
			}
			if len(sent) != 1 {
				t.Fatalf("expected 1 mail, got %d", len(sent))
			}
			body := sent[0].Body

			if tc.class == "" {
				if strings.Contains(body, "Failure-Type:") {
					t.Errorf("unclassified rejection's mail names a class:\n%s", body)
				}
			} else if !strings.Contains(body, "Failure-Type: "+tc.class+"\n") {
				t.Errorf("mail body missing the rejection's class:\n%s", body)
			}
			// The class sits on the line before Attempt:, so an omitted one
			// must not swallow the line the attempt count is read from.
			if !strings.Contains(body, "\nAttempt: 1\n") {
				t.Errorf("mail body lost its Attempt line:\n%s", body)
			}
			if !strings.Contains(body, "Polecat: testrig/polecats/nux") {
				t.Errorf("mail body lost the Polecat line the deacon reads the rig from:\n%s", body)
			}
		})
	}
}
