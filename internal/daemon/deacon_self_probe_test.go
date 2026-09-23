package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/mail"
)

// fakeDeaconInboxLister lets tests control what EvaluateDeaconSelfProbe
// sees in the deacon inbox without a real Dolt-backed mailbox.
type fakeDeaconInboxLister struct {
	messages []*mail.Message
	err      error
}

func (l *fakeDeaconInboxLister) List() ([]*mail.Message, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.messages, nil
}

// fakeFailingSender lets tests force SendDeaconSelfProbe's error branch.
// fakeRouter (plugin_script_test.go, same package) always succeeds, so it
// can't cover a failed send.
type fakeFailingSender struct {
	sent []*mail.Message
	err  error
}

func (s *fakeFailingSender) Send(m *mail.Message) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, m)
	return nil
}

func ackedProbeMessage(nonce string, sentAt time.Time, latency time.Duration) *mail.Message {
	ackedAt := sentAt.Add(latency)
	return &mail.Message{
		Subject:         deaconSelfProbeSubject(nonce),
		DeliveryState:   mail.DeliveryStateAcked,
		DeliveryAckedAt: &ackedAt,
	}
}

func unackedProbeMessage(nonce string) *mail.Message {
	return &mail.Message{
		Subject:       deaconSelfProbeSubject(nonce),
		DeliveryState: mail.DeliveryStatePending,
	}
}

func writeDeaconSelfProbeBaselineForTest(t *testing.T, townRoot, nonce string, sentAt time.Time) {
	t.Helper()
	if err := writeDeaconSelfProbeBaseline(deaconSelfProbeStatePath(townRoot), deaconSelfProbeBaseline{Nonce: nonce, SentAt: sentAt}); err != nil {
		t.Fatalf("writeDeaconSelfProbeBaseline: %v", err)
	}
}

// writeDeaconHealthSelfProbeBudget writes a town-level roles/deacon.toml
// override so deaconSelfProbeBudget resolves to a known value instead of
// the compiled-in default (config/roles/deacon.toml's self_probe_budget =
// "15m"). Also sets an unmistakably different ping_timeout so a test can
// prove the budget function isn't quietly falling back to it.
func writeDeaconHealthSelfProbeBudget(t *testing.T, townRoot, budget string) {
	t.Helper()
	rolesDir := filepath.Join(townRoot, "roles")
	if err := os.MkdirAll(rolesDir, 0755); err != nil {
		t.Fatalf("MkdirAll roles: %v", err)
	}
	content := "[health]\nself_probe_budget = \"" + budget + "\"\nping_timeout = \"1h\"\n"
	if err := os.WriteFile(filepath.Join(rolesDir, "deacon.toml"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile deacon.toml override: %v", err)
	}
}

func TestSendDeaconSelfProbe_Success_RecordsBaseline(t *testing.T) {
	townRoot := t.TempDir()
	sender := &fakeRouter{}

	if err := sendDeaconSelfProbeWith(sender, townRoot); err != nil {
		t.Fatalf("sendDeaconSelfProbeWith: %v", err)
	}

	if len(sender.sent) != 1 {
		t.Fatalf("expected exactly one probe sent, got %d", len(sender.sent))
	}
	msg := sender.sent[0]
	if !strings.HasPrefix(msg.Subject, DeaconSelfProbeSubjectPrefix) {
		t.Errorf("probe subject = %q, want prefix %q", msg.Subject, DeaconSelfProbeSubjectPrefix)
	}
	if !msg.Wisp {
		t.Error("probe must be sent as a wisp (ephemeral, no durable bead beyond it)")
	}

	baseline, ok := readDeaconSelfProbeBaseline(deaconSelfProbeStatePath(townRoot))
	if !ok || baseline.Nonce == "" || baseline.LastSendError != "" {
		t.Errorf("expected a clean baseline with a nonce, got %+v (ok=%v)", baseline, ok)
	}
}

func TestSendDeaconSelfProbe_Failure_PreservesPreviousBaselineAndRecordsError(t *testing.T) {
	townRoot := t.TempDir()
	sentAt := time.Now().Add(-2 * time.Minute)
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", sentAt)

	sender := &fakeFailingSender{err: errors.New("bd: dolt server unreachable")}

	err := sendDeaconSelfProbeWith(sender, townRoot)
	if err == nil {
		t.Fatal("expected sendDeaconSelfProbeWith to return the send error")
	}

	baseline, ok := readDeaconSelfProbeBaseline(deaconSelfProbeStatePath(townRoot))
	if !ok {
		t.Fatal("expected baseline to still be present")
	}
	if baseline.Nonce != "nonce-1" {
		t.Errorf("expected the previous nonce to be preserved, got %q", baseline.Nonce)
	}
	if baseline.LastSendError == "" {
		t.Error("expected LastSendError to be recorded")
	}
}

func TestEvaluateDeaconSelfProbe_NoBaseline_Skipped(t *testing.T) {
	townRoot := t.TempDir()

	verdict := evaluateDeaconSelfProbeWith(&fakeDeaconInboxLister{}, townRoot)

	if verdict.Verdict != deaconSelfProbeVerdictSkipped {
		t.Fatalf("Verdict = %q, want %q; message: %s", verdict.Verdict, deaconSelfProbeVerdictSkipped, verdict.Message)
	}
}

func TestEvaluateDeaconSelfProbe_AckedWithinBudget_OK(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconHealthSelfProbeBudget(t, townRoot, "1m")
	sentAt := time.Now().Add(-2 * time.Minute)
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", sentAt)

	reader := &fakeDeaconInboxLister{messages: []*mail.Message{
		ackedProbeMessage("nonce-1", sentAt, 10*time.Second),
	}}

	verdict := evaluateDeaconSelfProbeWith(reader, townRoot)

	if verdict.Verdict != deaconSelfProbeVerdictOK {
		t.Fatalf("Verdict = %q, want %q; message: %s", verdict.Verdict, deaconSelfProbeVerdictOK, verdict.Message)
	}
}

func TestEvaluateDeaconSelfProbe_AckedLate_Error(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconHealthSelfProbeBudget(t, townRoot, "30s")
	sentAt := time.Now().Add(-2 * time.Minute)
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", sentAt)

	reader := &fakeDeaconInboxLister{messages: []*mail.Message{
		ackedProbeMessage("nonce-1", sentAt, 90*time.Second), // later than the 30s budget
	}}

	verdict := evaluateDeaconSelfProbeWith(reader, townRoot)

	if verdict.Verdict != deaconSelfProbeVerdictError {
		t.Fatalf("Verdict = %q, want %q; message: %s", verdict.Verdict, deaconSelfProbeVerdictError, verdict.Message)
	}
	if !strings.Contains(verdict.Message, "late") {
		t.Errorf("Message = %q, want it to mention a late ack", verdict.Message)
	}
}

func TestEvaluateDeaconSelfProbe_Unacked_Error(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", time.Now().Add(-10*time.Minute))

	reader := &fakeDeaconInboxLister{messages: []*mail.Message{
		unackedProbeMessage("nonce-1"),
	}}

	verdict := evaluateDeaconSelfProbeWith(reader, townRoot)

	if verdict.Verdict != deaconSelfProbeVerdictError {
		t.Fatalf("Verdict = %q, want %q; message: %s", verdict.Verdict, deaconSelfProbeVerdictError, verdict.Message)
	}
	if !strings.Contains(verdict.Message, "not ack") {
		t.Errorf("Message = %q, want it to say the probe was not acked", verdict.Message)
	}
}

func TestEvaluateDeaconSelfProbe_ProbeMissing_Skipped(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", time.Now().Add(-10*time.Minute))

	reader := &fakeDeaconInboxLister{messages: nil} // patrol already archived it, or it never existed

	verdict := evaluateDeaconSelfProbeWith(reader, townRoot)

	if verdict.Verdict != deaconSelfProbeVerdictSkipped {
		t.Fatalf("Verdict = %q, want %q; message: %s", verdict.Verdict, deaconSelfProbeVerdictSkipped, verdict.Message)
	}
}

func TestEvaluateDeaconSelfProbe_InboxUnreadable_Skipped(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", time.Now().Add(-10*time.Minute))

	reader := &fakeDeaconInboxLister{err: errors.New("dolt: connection refused")}

	verdict := evaluateDeaconSelfProbeWith(reader, townRoot)

	if verdict.Verdict != deaconSelfProbeVerdictSkipped {
		t.Fatalf("Verdict = %q, want %q; message: %s", verdict.Verdict, deaconSelfProbeVerdictSkipped, verdict.Message)
	}
}

// TestDeaconSelfProbeBudget_ReadsSelfProbeBudget_NotPingTimeout guards
// against the exact regression this redesign fixes: a probe judged against
// ping_timeout (a 30s network health-check value) looks fixed the moment the
// deacon can ack at all, then flips straight to "acked late" and stays a
// permanent Error nobody can meet. self_probe_budget=30s / ping_timeout=1h
// makes the two config keys disagree, so reading the wrong one is
// unmistakable: an ack 90s later must fail, not pass.
func TestDeaconSelfProbeBudget_ReadsSelfProbeBudget_NotPingTimeout(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconHealthSelfProbeBudget(t, townRoot, "30s")

	got := deaconSelfProbeBudget(townRoot)
	if got != 30*time.Second {
		t.Fatalf("deaconSelfProbeBudget = %s, want 30s (ping_timeout override is 1h; reading it instead is the bug this redesign fixes)", got)
	}
}

// fakeAlertRecorder stands in for the daemon's escalateAlert/clearAlerts,
// which shell out to `gt escalate`: tests must observe the call, never send
// live mail or hit a real Dolt-backed bead.
type fakeAlertRecorder struct {
	alerts []string // fingerprint keys passed to alert
	clears []string // fingerprint keys passed to clear
}

func (r *fakeAlertRecorder) alert(key, source, message string) {
	r.alerts = append(r.alerts, key)
}

func (r *fakeAlertRecorder) clear(reason string, keys ...string) {
	r.clears = append(r.clears, keys...)
}

// TestDeaconSelfProbe_EscalatesAfterConsecutiveErrors is the escalation
// test gt-6gsfv requires: it must fail against current origin/main, which
// has no ConsecutiveErrors field and never calls escalateAlert for a self
// probe at all — ack is deliberately withheld, evaluation runs
// deaconSelfProbeErrorThreshold times, and only the last one may escalate.
func TestDeaconSelfProbe_EscalatesAfterConsecutiveErrors(t *testing.T) {
	townRoot := t.TempDir()
	sentAt := time.Now().Add(-1 * time.Hour)
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", sentAt)

	recorder := &fakeAlertRecorder{}
	unacked := &fakeDeaconInboxLister{messages: []*mail.Message{unackedProbeMessage("nonce-1")}}

	// probesExamined is the vacuous-pass guard gt-6gsfv also requires: a bug
	// that skipped every evaluation (e.g. an early return before the loop
	// runs) would leave alerts empty and this test would pass for the wrong
	// reason — "no probes examined, therefore no failures observed".
	probesExamined := 0
	for i := 0; i < deaconSelfProbeErrorThreshold; i++ {
		verdict := evaluateDeaconSelfProbeWith(unacked, townRoot)
		probesExamined++
		if verdict.Verdict != deaconSelfProbeVerdictError {
			t.Fatalf("round %d: Verdict = %q, want %q; message: %s", i+1, verdict.Verdict, deaconSelfProbeVerdictError, verdict.Message)
		}
		recordDeaconSelfProbeVerdict(townRoot, verdict, recorder.alert, recorder.clear)
	}

	if probesExamined == 0 {
		t.Fatal("vacuous-pass guard: no probes were examined")
	}
	if len(recorder.alerts) != 1 {
		t.Fatalf("expected exactly one escalation after %d consecutive errors, got %d: %v", deaconSelfProbeErrorThreshold, len(recorder.alerts), recorder.alerts)
	}
	if recorder.alerts[0] != deaconSelfProbeAlertKey {
		t.Errorf("escalation fingerprint = %q, want %q", recorder.alerts[0], deaconSelfProbeAlertKey)
	}

	baseline, ok := readDeaconSelfProbeBaseline(deaconSelfProbeStatePath(townRoot))
	if !ok || baseline.ConsecutiveErrors != deaconSelfProbeErrorThreshold {
		t.Errorf("ConsecutiveErrors = %d, want %d", baseline.ConsecutiveErrors, deaconSelfProbeErrorThreshold)
	}
}

// TestDeaconSelfProbe_EscalatesOnceThenDedupes confirms a fourth, fifth,
// etc. consecutive Error keeps re-escalating under the SAME fingerprint (`gt
// escalate` dedupes on it) rather than staying silent — a stuck deacon must
// not go quiet just because the mayor already knows once.
func TestDeaconSelfProbe_EscalatesOnceThenDedupes(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", time.Now().Add(-1*time.Hour))
	recorder := &fakeAlertRecorder{}
	unacked := &fakeDeaconInboxLister{messages: []*mail.Message{unackedProbeMessage("nonce-1")}}

	for i := 0; i < deaconSelfProbeErrorThreshold+2; i++ {
		verdict := evaluateDeaconSelfProbeWith(unacked, townRoot)
		recordDeaconSelfProbeVerdict(townRoot, verdict, recorder.alert, recorder.clear)
	}

	if len(recorder.alerts) != 3 { // rounds 3, 4, 5 (threshold, threshold+1, threshold+2)
		t.Fatalf("expected 3 escalations (one per round at/after threshold), got %d", len(recorder.alerts))
	}
	for _, key := range recorder.alerts {
		if key != deaconSelfProbeAlertKey {
			t.Errorf("escalation fingerprint = %q, want stable key %q", key, deaconSelfProbeAlertKey)
		}
	}
}

// TestDeaconSelfProbe_BelowThreshold_NoEscalation guards the other edge:
// one or two Errors are noise (a slow tick, a transient Dolt hiccup), not
// evidence of a stuck deacon, and must not page the mayor.
func TestDeaconSelfProbe_BelowThreshold_NoEscalation(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", time.Now().Add(-1*time.Hour))
	recorder := &fakeAlertRecorder{}
	unacked := &fakeDeaconInboxLister{messages: []*mail.Message{unackedProbeMessage("nonce-1")}}

	for i := 0; i < deaconSelfProbeErrorThreshold-1; i++ {
		verdict := evaluateDeaconSelfProbeWith(unacked, townRoot)
		recordDeaconSelfProbeVerdict(townRoot, verdict, recorder.alert, recorder.clear)
	}

	if len(recorder.alerts) != 0 {
		t.Fatalf("expected no escalation below threshold, got %v", recorder.alerts)
	}
}

// TestDeaconSelfProbe_Recovery_ResetsCounterAndClears: Error, Error, then OK
// must reset ConsecutiveErrors to 0 and clear the alert — a patrol that
// recovers must not stay flagged, and a later relapse must count from zero,
// not resume from where the old streak left off.
func TestDeaconSelfProbe_Recovery_ResetsCounterAndClears(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", time.Now().Add(-1*time.Hour))
	writeDeaconHealthSelfProbeBudget(t, townRoot, "1h") // generous, so the OK round doesn't trip on latency
	recorder := &fakeAlertRecorder{}

	unacked := &fakeDeaconInboxLister{messages: []*mail.Message{unackedProbeMessage("nonce-1")}}
	for i := 0; i < 2; i++ {
		verdict := evaluateDeaconSelfProbeWith(unacked, townRoot)
		recordDeaconSelfProbeVerdict(townRoot, verdict, recorder.alert, recorder.clear)
	}
	baseline, _ := readDeaconSelfProbeBaseline(deaconSelfProbeStatePath(townRoot))
	if baseline.ConsecutiveErrors != 2 {
		t.Fatalf("after 2 errors, ConsecutiveErrors = %d, want 2", baseline.ConsecutiveErrors)
	}

	acked := &fakeDeaconInboxLister{messages: []*mail.Message{
		ackedProbeMessage("nonce-1", time.Now().Add(-1*time.Hour), time.Minute),
	}}
	verdict := evaluateDeaconSelfProbeWith(acked, townRoot)
	if verdict.Verdict != deaconSelfProbeVerdictOK {
		t.Fatalf("recovery round: Verdict = %q, want %q; message: %s", verdict.Verdict, deaconSelfProbeVerdictOK, verdict.Message)
	}
	recordDeaconSelfProbeVerdict(townRoot, verdict, recorder.alert, recorder.clear)

	baseline, _ = readDeaconSelfProbeBaseline(deaconSelfProbeStatePath(townRoot))
	if baseline.ConsecutiveErrors != 0 {
		t.Errorf("after recovery, ConsecutiveErrors = %d, want 0", baseline.ConsecutiveErrors)
	}
	if len(recorder.clears) == 0 {
		t.Error("expected clearAlerts to be called on recovery")
	}
	if len(recorder.alerts) != 0 {
		t.Errorf("recovery round must not escalate, got %v", recorder.alerts)
	}
}

// TestDeaconSelfProbe_Skipped_LeavesCounterUntouched: a Skipped verdict
// (probe missing, inbox unreadable) is not evidence either way and must
// neither mask a real failure streak nor falsely reset one.
func TestDeaconSelfProbe_Skipped_LeavesCounterUntouched(t *testing.T) {
	townRoot := t.TempDir()
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", time.Now().Add(-1*time.Hour))
	recorder := &fakeAlertRecorder{}

	unacked := &fakeDeaconInboxLister{messages: []*mail.Message{unackedProbeMessage("nonce-1")}}
	verdict := evaluateDeaconSelfProbeWith(unacked, townRoot)
	recordDeaconSelfProbeVerdict(townRoot, verdict, recorder.alert, recorder.clear)

	missing := &fakeDeaconInboxLister{messages: nil}
	verdict = evaluateDeaconSelfProbeWith(missing, townRoot)
	if verdict.Verdict != deaconSelfProbeVerdictSkipped {
		t.Fatalf("Verdict = %q, want %q", verdict.Verdict, deaconSelfProbeVerdictSkipped)
	}
	recordDeaconSelfProbeVerdict(townRoot, verdict, recorder.alert, recorder.clear)

	baseline, _ := readDeaconSelfProbeBaseline(deaconSelfProbeStatePath(townRoot))
	if baseline.ConsecutiveErrors != 1 {
		t.Errorf("ConsecutiveErrors = %d after a Skipped round, want 1 (unchanged from the prior Error)", baseline.ConsecutiveErrors)
	}
}

// TestRunDeaconSelfProbeCycle_PausedDeacon_NoEscalation is the paused-deacon
// gate NON-NEGOTIABLE from gt-6gsfv: a paused deacon legitimately will not
// ack, so evaluating anyway would raise a false alarm the first time an
// operator runs `gt deacon pause`. Three rounds of an unacked probe would
// escalate if the gate were missing (see
// TestDeaconSelfProbe_EscalatesAfterConsecutiveErrors); paused, none may.
func TestRunDeaconSelfProbeCycle_PausedDeacon_NoEscalation(t *testing.T) {
	townRoot := t.TempDir()
	if err := deacon.Pause(townRoot, "test", "test"); err != nil {
		t.Fatalf("deacon.Pause: %v", err)
	}
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", time.Now().Add(-1*time.Hour))
	recorder := &fakeAlertRecorder{}
	unacked := &fakeDeaconInboxLister{messages: []*mail.Message{unackedProbeMessage("nonce-1")}}
	sender := &fakeRouter{}

	for i := 0; i < deaconSelfProbeErrorThreshold; i++ {
		if err := runDeaconSelfProbeCycle(townRoot, unacked, sender, recorder.alert, recorder.clear); err != nil {
			t.Fatalf("round %d: runDeaconSelfProbeCycle: %v", i+1, err)
		}
	}

	if len(recorder.alerts) != 0 {
		t.Errorf("a paused deacon must never escalate, got %v", recorder.alerts)
	}
	if len(sender.sent) != 0 {
		t.Errorf("a paused deacon's cycle must not send a new probe either, got %d sent", len(sender.sent))
	}
	baseline, ok := readDeaconSelfProbeBaseline(deaconSelfProbeStatePath(townRoot))
	if !ok || baseline.ConsecutiveErrors != 0 {
		t.Errorf("a paused cycle must not advance ConsecutiveErrors, got %d (exists=%v)", baseline.ConsecutiveErrors, ok)
	}
}

// TestRunDeaconSelfProbeCycle_UnreadablePauseState_FailsClosed: an
// unreadable pause file must be treated as "cannot tell", never as "not
// paused" — copying heartbeat.go:156's precedent. A corrupt pause file must
// not silently re-enable evaluation and escalation.
func TestRunDeaconSelfProbeCycle_UnreadablePauseState_FailsClosed(t *testing.T) {
	townRoot := t.TempDir()
	pauseFile := deacon.GetPauseFile(townRoot)
	if err := os.MkdirAll(filepath.Dir(pauseFile), 0755); err != nil {
		t.Fatalf("MkdirAll pause dir: %v", err)
	}
	if err := os.WriteFile(pauseFile, []byte("{not valid json"), 0644); err != nil {
		t.Fatalf("WriteFile corrupt pause state: %v", err)
	}
	writeDeaconSelfProbeBaselineForTest(t, townRoot, "nonce-1", time.Now().Add(-1*time.Hour))
	recorder := &fakeAlertRecorder{}
	unacked := &fakeDeaconInboxLister{messages: []*mail.Message{unackedProbeMessage("nonce-1")}}
	sender := &fakeRouter{}

	if err := runDeaconSelfProbeCycle(townRoot, unacked, sender, recorder.alert, recorder.clear); err != nil {
		t.Fatalf("runDeaconSelfProbeCycle: %v", err)
	}

	if len(recorder.alerts) != 0 {
		t.Errorf("an unreadable pause state must fail closed (no escalation), got %v", recorder.alerts)
	}
	if len(sender.sent) != 0 {
		t.Errorf("an unreadable pause state must fail closed (no send), got %d sent", len(sender.sent))
	}
}

// TestRunDeaconSelfProbeCycle_BoundsBacklogToOneOutstandingProbe: repeated
// ticks must never let the tracked probe count grow — each cycle's send
// replaces the baseline's nonce, so there is exactly one at any time, not a
// list that grows with every unacked round.
func TestRunDeaconSelfProbeCycle_BoundsBacklogToOneOutstandingProbe(t *testing.T) {
	townRoot := t.TempDir()
	recorder := &fakeAlertRecorder{}
	sender := &fakeRouter{}
	mailbox := &fakeDeaconInboxLister{} // empty: every round evaluates to Skipped (no baseline yet, then probe-missing)

	for i := 0; i < 5; i++ {
		if err := runDeaconSelfProbeCycle(townRoot, mailbox, sender, recorder.alert, recorder.clear); err != nil {
			t.Fatalf("round %d: runDeaconSelfProbeCycle: %v", i+1, err)
		}
	}

	if len(sender.sent) != 5 {
		t.Fatalf("expected one send per tick (5 ticks), got %d", len(sender.sent))
	}
	baseline, ok := readDeaconSelfProbeBaseline(deaconSelfProbeStatePath(townRoot))
	if !ok {
		t.Fatal("expected a baseline after 5 sends")
	}
	if baseline.Nonce != sender.sent[len(sender.sent)-1].Subject[len(DeaconSelfProbeSubjectPrefix)+1:] {
		t.Errorf("baseline tracks exactly the most recent send's nonce, not an accumulating list")
	}
}

func TestEvaluateDeaconSelfProbe_LastSendFailed_Skipped(t *testing.T) {
	townRoot := t.TempDir()
	statePath := deaconSelfProbeStatePath(townRoot)
	if err := writeDeaconSelfProbeBaseline(statePath, deaconSelfProbeBaseline{
		Nonce:         "nonce-1",
		SentAt:        time.Now().Add(-2 * time.Minute),
		LastSendError: "bd: dolt server unreachable",
	}); err != nil {
		t.Fatalf("writeDeaconSelfProbeBaseline: %v", err)
	}

	// Even if the inbox would show a perfectly good ack, a failed send this
	// cycle must not be reported as a pass — there is no evidence the patrol
	// is currently reactive.
	reader := &fakeDeaconInboxLister{messages: []*mail.Message{
		ackedProbeMessage("nonce-1", time.Now().Add(-2*time.Minute), 5*time.Second),
	}}

	verdict := evaluateDeaconSelfProbeWith(reader, townRoot)

	if verdict.Verdict != deaconSelfProbeVerdictSkipped {
		t.Fatalf("Verdict = %q, want %q; message: %s", verdict.Verdict, deaconSelfProbeVerdictSkipped, verdict.Message)
	}
}
