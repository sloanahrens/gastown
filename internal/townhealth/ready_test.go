package townhealth

import (
	"strings"
	"testing"
	"time"
)

func TestReadyTable(t *testing.T) {
	t.Parallel()
	recordedRed := func(name string) Field {
		return Field{Name: name, Tag: Recorded, Verdict: Red, Value: "off"}
	}
	tests := []struct {
		name       string
		report     Report
		wantOK     bool
		wantReason string
	}{
		{
			name:   "green is ready",
			report: Report{At: ago(time.Minute), Verdict: Green, Fields: []Field{{Name: FieldDolt, Tag: Live, Verdict: Green, Value: "3ms"}}},
			wantOK: true,
		},
		{
			name: "red with only recorded fields is ready",
			report: Report{At: ago(time.Minute), Verdict: Red, Fields: []Field{
				recordedRed(FieldDispatch),
				{Name: FieldBackup, Tag: Recorded, Verdict: Red, Value: "none"},
			}},
			wantOK: true,
		},
		{
			name: "degraded with one measured non-green field is not ready and the reason names it",
			report: Report{At: ago(time.Minute), Verdict: Degraded, Fields: []Field{
				{Name: FieldDolt, Tag: Live, Verdict: Degraded, Value: "p50 900ms"},
			}},
			wantReason: FieldDolt,
		},
		{
			name: "an UNKNOWN-tagged field is not ready",
			report: Report{At: ago(time.Minute), Verdict: Red, Fields: []Field{
				recordedRed(FieldDispatch),
				{Name: FieldHost, Tag: Unknown, Verdict: VerdictUnknown, Value: "?"},
			}},
			wantReason: FieldHost,
		},
		{
			name:       "an unknown verdict is not ready",
			report:     Report{At: ago(time.Minute), Verdict: VerdictUnknown, Fields: []Field{recordedRed(FieldDispatch)}},
			wantReason: "verdict is unknown",
		},
		{
			name:       "a stale red report is not ready",
			report:     Report{At: ago(DefaultStaleAfter), Verdict: Red, Fields: []Field{recordedRed(FieldDispatch)}},
			wantReason: "stale",
		},
		{
			name:       "a red report with no non-green field has no recorded cause",
			report:     Report{At: ago(time.Minute), Verdict: Red, Fields: []Field{{Name: FieldDolt, Tag: Live, Verdict: Green, Value: "3ms"}}},
			wantReason: "no non-green field",
		},
		{
			name: "a recorded field whose verdict is unknown is not ready",
			report: Report{At: ago(time.Minute), Verdict: Red, Fields: []Field{
				recordedRed(FieldDispatch),
				{Name: FieldSlot, Tag: Recorded, Verdict: VerdictUnknown, Value: "?"},
			}},
			wantReason: FieldSlot,
		},
		{
			name: "the reason names the worst blocking field, not the first in report order",
			report: Report{At: ago(time.Minute), Verdict: Red, Fields: []Field{
				{Name: FieldDolt, Tag: Live, Verdict: Degraded, Value: "p50 900ms"},
				{Name: FieldHost, Tag: Live, Verdict: Red, Value: "cpu 2%"},
			}},
			wantReason: FieldHost,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ok, reason := Ready(tt.report, now, DefaultStaleAfter)
			if ok != tt.wantOK {
				t.Fatalf("Ready = %v (%q), want %v", ok, reason, tt.wantOK)
			}
			if tt.wantOK {
				if reason != "" {
					t.Errorf("ready with reason %q, want none", reason)
				}
				return
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to name %q", reason, tt.wantReason)
			}
		})
	}
}

// TestReadyAttentionRowDoesNotBlock is the reported incident (gt-pittz6): a
// bead waits for the overseer, so that row is LIVE and needs no RECORDED tag,
// and beside healthy serving rows it must leave the town ready. The row keeps
// showing in the line.
func TestReadyAttentionRowDoesNotBlock(t *testing.T) {
	t.Parallel()
	report := Report{At: ago(time.Minute), Verdict: Degraded, Fields: []Field{
		{Name: FieldDolt, Tag: Live, Verdict: Green, Value: "3ms"},
		{Name: FieldConfig, Tag: Live, Verdict: Green, Value: "valid"},
		{Name: FieldNeedsOverseer, Tag: Live, Verdict: Degraded, Value: "1", Detail: "1 waiting for overseer"},
	}}
	if ok, reason := Ready(report, now, DefaultStaleAfter); !ok || reason != "" {
		t.Fatalf("Ready = %v (%q), want ready: a person's queue must not fail readiness", ok, reason)
	}
	if line := Line(report, now, DefaultStaleAfter); !strings.Contains(line, "needs-overseer=1") {
		t.Errorf("Line = %q, want it to keep showing the person row", line)
	}
}

// TestReadyAttentionRowDoesNotMaskAServingFault: excusing the attention rows
// must not excuse a measured serving row beside them, and the reason names the
// serving row, not the person row it skips.
func TestReadyAttentionRowDoesNotMaskAServingFault(t *testing.T) {
	t.Parallel()
	report := Report{At: ago(time.Minute), Verdict: Red, Fields: []Field{
		{Name: FieldNeedsOverseer, Tag: Live, Verdict: Red, Value: "1"},
		{Name: FieldDolt, Tag: Live, Verdict: Degraded, Value: "p50 900ms"},
	}}
	ok, reason := Ready(report, now, DefaultStaleAfter)
	if ok {
		t.Fatal("Ready = true, want not ready: a measured serving row is non-green")
	}
	if !strings.Contains(reason, FieldDolt) {
		t.Errorf("reason = %q, want it to name %s", reason, FieldDolt)
	}
	if strings.Contains(reason, FieldNeedsOverseer) {
		t.Errorf("reason = %q, want it to skip the attention row", reason)
	}
}

// TestReadyUnknownAttentionRowBlocks: an attention row whose read failed is
// UNKNOWN, and an unknown field is not ready, so the person-row exemption does
// not reach it. The reason names the row.
func TestReadyUnknownAttentionRowBlocks(t *testing.T) {
	t.Parallel()
	report := Report{At: ago(time.Minute), Verdict: Red, Fields: []Field{
		{Name: FieldDispatch, Tag: Recorded, Verdict: Red, Value: "off"},
		{Name: FieldNeedsOverseer, Tag: Unknown, Verdict: VerdictUnknown, Value: "?"},
	}}
	ok, reason := Ready(report, now, DefaultStaleAfter)
	if ok {
		t.Fatal("Ready = true, want not ready: an UNKNOWN attention row is an unknown field")
	}
	if !strings.Contains(reason, FieldNeedsOverseer) {
		t.Errorf("reason = %q, want it to name %s", reason, FieldNeedsOverseer)
	}
}

// TestReadyFieldTable names every health row and whether a non-green one
// blocks the readiness answer (gt-pittz6). The tag is the one Compute gives
// the row. A RECORDED row is a state the readiness rule accepts; a LIVE row
// blocks unless it is an attention row, which says a person should look rather
// than that the daemon cannot serve; host is green whenever it is read, so it
// never blocks.
func TestReadyFieldTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tag     Tag
		verdict Verdict
		wantOK  bool
		// why states the reason the row does or does not block.
		why string
	}{
		// Serving rows measured now: the daemon cannot serve.
		{FieldDolt, Live, Degraded, false, "the datastore answers too slowly"},
		{FieldExecTax, Live, Degraded, false, "every fresh executable pays too much to start"},
		{FieldDaemon, Live, Degraded, false, "the heartbeat count stopped advancing"},
		{FieldConfig, Live, Red, false, "the town config does not validate"},
		// Host is green whenever the host could be read: it has no non-green
		// verdict to block readiness with.
		{FieldHost, Live, Green, true, "green whenever the host is readable"},
		// Serving rows the computation records: a non-green one is a known,
		// accepted state.
		{FieldTick, Recorded, Degraded, true, "a tick ran late, recorded"},
		{FieldLanding, Recorded, Red, true, "a landing queue is stuck, recorded"},
		{FieldSlot, Recorded, Red, true, "a container slot is held, recorded"},
		{FieldBackup, Recorded, Red, true, "no recent backup, recorded"},
		{FieldMain, Recorded, Red, true, "main is red, recorded"},
		{FieldSeat, Recorded, Red, true, "a seat is dead or stalled, recorded"},
		{FieldPromote, Recorded, Red, true, "a promotion diverged, recorded"},
		{FieldDispatch, Recorded, Red, true, "the dispatcher is off or stalled, recorded"},
		{FieldForgejo, Recorded, Red, true, "the forge is down, recorded"},
		{FieldRunner, Recorded, Red, true, "no runner picks runs up, recorded"},
		// Attention rows: a person should look, so they stay in the line but
		// do not fail readiness.
		{FieldNeedsOverseer, Live, Red, true, "a bead waits for the overseer"},
		{FieldNeedsOperator, Live, Red, true, "a bead waits for the operator"},
		{FieldNeedsHuman, Live, Red, true, "a bead waits under the old person label"},
		{FieldEscalation, Live, Red, true, "an escalation waits for a person"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fields := []Field{{Name: tt.name, Tag: tt.tag, Verdict: tt.verdict, Value: "non-green"}}
			verdict := tt.verdict
			if verdict == Green {
				// A green report is ready before Ready reads any field, so a
				// green row only tests something beside a recorded red cause.
				verdict = Red
				fields = append(fields, Field{Name: FieldDispatch, Tag: Recorded, Verdict: Red, Value: "off"})
			}
			report := Report{At: ago(time.Minute), Verdict: verdict, Fields: fields}
			ok, reason := Ready(report, now, DefaultStaleAfter)
			if ok != tt.wantOK {
				t.Fatalf("Ready = %v (%q), want %v: %s", ok, reason, tt.wantOK, tt.why)
			}
			if !ok && !strings.Contains(reason, tt.name) {
				t.Errorf("reason = %q, want it to name the blocking row %s", reason, tt.name)
			}
		})
	}
}
