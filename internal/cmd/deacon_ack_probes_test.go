package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/mail"
)

// fakeAckableMailbox is an in-memory ackableMailbox for testing
// ackDeaconSelfProbes without a real bd-backed mailbox. MarkReadOnly and
// AcknowledgeDeliveries mutate the same message pointers List() returns,
// mirroring how a real *mail.Mailbox's ack becomes visible on the next
// List() — so tests can assert on post-ack state directly.
type fakeAckableMailbox struct {
	messages      []*mail.Message
	listErr       error
	markReadErr   error
	ackErr        error
	markedReadIDs []string
	ackedIDs      []string
}

func (f *fakeAckableMailbox) List() ([]*mail.Message, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.messages, nil
}

func (f *fakeAckableMailbox) MarkReadOnly(id string) error {
	if f.markReadErr != nil {
		return f.markReadErr
	}
	f.markedReadIDs = append(f.markedReadIDs, id)
	for _, m := range f.messages {
		if m.ID == id {
			m.Read = true
		}
	}
	return nil
}

func (f *fakeAckableMailbox) AcknowledgeDeliveries(recipientAddress string, messages []*mail.Message) error {
	if f.ackErr != nil {
		return f.ackErr
	}
	now := time.Now()
	for _, msg := range messages {
		f.ackedIDs = append(f.ackedIDs, msg.ID)
		msg.DeliveryState = mail.DeliveryStateAcked
		msg.DeliveryAckedBy = recipientAddress
		msg.DeliveryAckedAt = &now
	}
	return nil
}

func probeMessage(id, nonce, deliveryState string) *mail.Message {
	return &mail.Message{
		ID:            id,
		Subject:       daemon.DeaconSelfProbeSubjectPrefix + " " + nonce,
		To:            "deacon",
		DeliveryState: deliveryState,
	}
}

func TestAckDeaconSelfProbes_AcksOnlyPendingProbes(t *testing.T) {
	box := &fakeAckableMailbox{
		messages: []*mail.Message{
			probeMessage("probe-pending", "nonce-1", mail.DeliveryStatePending),
			probeMessage("probe-already-acked", "nonce-2", mail.DeliveryStateAcked),
			{ID: "not-a-probe", Subject: "RECOVERED_BEAD gt-abc", To: "deacon", DeliveryState: mail.DeliveryStatePending},
		},
	}

	result, err := ackDeaconSelfProbes(box, "deacon")
	if err != nil {
		t.Fatalf("ackDeaconSelfProbes: %v", err)
	}

	if result.Found != 1 || result.Acked != 1 {
		t.Fatalf("result = %+v, want Found=1 Acked=1", result)
	}
	if len(result.IDs) != 1 || result.IDs[0] != "probe-pending" {
		t.Fatalf("result.IDs = %v, want [probe-pending]", result.IDs)
	}
	if len(box.markedReadIDs) != 1 || box.markedReadIDs[0] != "probe-pending" {
		t.Fatalf("markedReadIDs = %v, want [probe-pending]", box.markedReadIDs)
	}
	if len(box.ackedIDs) != 1 || box.ackedIDs[0] != "probe-pending" {
		t.Fatalf("ackedIDs = %v, want [probe-pending]", box.ackedIDs)
	}
}

func TestAckDeaconSelfProbes_NoPendingProbes_NoOp(t *testing.T) {
	box := &fakeAckableMailbox{
		messages: []*mail.Message{
			{ID: "normal-1", Subject: "hello", To: "deacon"},
		},
	}

	result, err := ackDeaconSelfProbes(box, "deacon")
	if err != nil {
		t.Fatalf("ackDeaconSelfProbes: %v", err)
	}
	if result.Found != 0 || result.Acked != 0 || len(result.IDs) != 0 {
		t.Fatalf("result = %+v, want all zero", result)
	}
}

func TestAckDeaconSelfProbes_PropagatesListError(t *testing.T) {
	box := &fakeAckableMailbox{listErr: errors.New("dolt: connection refused")}

	if _, err := ackDeaconSelfProbes(box, "deacon"); err == nil {
		t.Fatal("expected List error to propagate")
	}
}

func TestAckDeaconSelfProbes_PropagatesAckError(t *testing.T) {
	box := &fakeAckableMailbox{
		messages: []*mail.Message{probeMessage("probe-1", "nonce-1", mail.DeliveryStatePending)},
		ackErr:   errors.New("bd: write failed"),
	}

	result, err := ackDeaconSelfProbes(box, "deacon")
	if err == nil {
		t.Fatal("expected AcknowledgeDeliveries error to propagate")
	}
	// Found is still reported even though the ack itself failed partway.
	if result.Found != 1 || result.Acked != 0 {
		t.Fatalf("result = %+v, want Found=1 Acked=0 on ack failure", result)
	}
}

// TestAckDeaconSelfProbes_EndToEnd_DoctorCheckWouldPass exercises the full
// flow the bead describes: a probe is sent (built the same way
// daemon.SendDeaconSelfProbe constructs one), it is undiscoverable via the
// filtered inbox view (mirrored by filterDeaconSelfProbes below), gets
// enumerated and acked via ackDeaconSelfProbes, and the resulting message
// state satisfies exactly the condition
// daemon.EvaluateDeaconSelfProbe/DeaconSelfProbeCheck reads back (DeliveryState
// == acked, DeliveryAckedAt set) — i.e. the doctor check would now pass
// where before the fix it stayed pending forever and hard-failed.
func TestAckDeaconSelfProbes_EndToEnd_DoctorCheckWouldPass(t *testing.T) {
	nonce := "e2e-nonce-1"
	probe := probeMessage("probe-e2e", nonce, mail.DeliveryStatePending)
	box := &fakeAckableMailbox{messages: []*mail.Message{probe}}

	// The deacon cannot discover the probe through the normal inbox view —
	// gt mail inbox strips DEACON_SELF_PROBE messages by design.
	visibleInInbox := filterDeaconSelfProbes(box.messages)
	if len(visibleInInbox) != 0 {
		t.Fatalf("probe should be filtered from the default inbox view, got %d visible", len(visibleInInbox))
	}

	// gt deacon ack-probes enumerates it anyway (bypassing that filter) and acks it.
	result, err := ackDeaconSelfProbes(box, "deacon")
	if err != nil {
		t.Fatalf("ackDeaconSelfProbes: %v", err)
	}
	if result.Acked != 1 {
		t.Fatalf("result.Acked = %d, want 1", result.Acked)
	}

	// This is exactly the condition daemon.EvaluateDeaconSelfProbe checks
	// (deacon_self_probe.go: found.DeliveryState != mail.DeliveryStateAcked ||
	// found.DeliveryAckedAt == nil) before reporting the doctor check as
	// StatusError.
	if probe.DeliveryState != mail.DeliveryStateAcked {
		t.Fatalf("probe.DeliveryState = %q, want %q (doctor check would still fail)", probe.DeliveryState, mail.DeliveryStateAcked)
	}
	if probe.DeliveryAckedAt == nil {
		t.Fatal("probe.DeliveryAckedAt is nil, doctor check would still fail")
	}
}
