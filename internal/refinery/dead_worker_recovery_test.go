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
}

func (f *fakeRejectedBeads) Show(id string) (*beads.Issue, error) {
	if f.showErr != nil {
		return nil, f.showErr
	}
	return f.issue, nil
}

func (f *fakeRejectedBeads) Update(id string, opts beads.UpdateOptions) error {
	f.updates = append(f.updates, opts)
	return f.updateErr
}

func (f *fakeRejectedBeads) Run(args ...string) ([]byte, error) {
	f.runCalls = append(f.runCalls, args)
	return nil, f.runErr
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
	notesArg := bd.runCalls[0][len(bd.runCalls[0])-1]
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

func TestIsDeliberateTerminalCloseReason(t *testing.T) {
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

func TestRecoverRejectedMRDeadWorker_DeadWorkerStillAssigned_Recovers(t *testing.T) {
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

func TestWorkerNameFromMR(t *testing.T) {
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
