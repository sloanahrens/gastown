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
