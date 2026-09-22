package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// writeDeaconHealthPingTimeout writes a town-level roles/deacon.toml
// override so deaconSelfProbeBudget resolves to a known value instead of
// the compiled-in default (config/roles/deacon.toml's ping_timeout = "30s").
func writeDeaconHealthPingTimeout(t *testing.T, townRoot, timeout string) {
	t.Helper()
	rolesDir := filepath.Join(townRoot, "roles")
	if err := os.MkdirAll(rolesDir, 0755); err != nil {
		t.Fatalf("MkdirAll roles: %v", err)
	}
	content := "[health]\nping_timeout = \"" + timeout + "\"\n"
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
	writeDeaconHealthPingTimeout(t, townRoot, "1m")
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
	writeDeaconHealthPingTimeout(t, townRoot, "30s")
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
