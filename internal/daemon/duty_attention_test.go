package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// A duty row is not a patrol of its own: it is off exactly when the reconciler
// patrol that writes it is off, while a non-duty name keeps reading its own
// setting (gt-ubzyds).
func TestDutyAttentionOffWithTheReconciler(t *testing.T) {
	t.Parallel()
	duty := JobReading{Name: "duty:backup", State: jobStateOverdue, LastRun: jobOverdueNow.Add(-3 * 24 * time.Hour), Cadence: 24 * time.Hour}
	for _, tc := range []struct {
		name          string
		disabled      map[string]bool
		wantDutyItems int
		wantGCOff     bool
	}{
		{"reconciler off holds its duty rows off", map[string]bool{"reconciler": true}, 0, false},
		{"reconciler on leaves its duty rows on", nil, 1, false},
		{"a disabled non-duty name stays off with the reconciler on", map[string]bool{"dolt_gc": true}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, jobOverdueNow)
			f.src.d.disabledPatrols = tc.disabled
			src := f.src.d.attentionSources(jobOverdueNow)
			if src.patrolOff == nil {
				t.Fatal("patrolOff is nil")
			}
			if got := src.patrolOff("dolt_gc"); got != tc.wantGCOff {
				t.Errorf("patrolOff(dolt_gc) = %v, want %v: a non-duty name keeps its own answer", got, tc.wantGCOff)
			}
			src.jobList = func(now time.Time) ([]JobReading, error) { return []JobReading{duty}, nil }
			items, err := src.collectJobOverdue(context.Background())
			if err != nil {
				t.Fatalf("collector: %v", err)
			}
			if len(items) != tc.wantDutyItems {
				t.Fatalf("items = %+v, want %d for %s", items, tc.wantDutyItems, duty.Name)
			}
		})
	}
}

// dutyAttentionItems runs the job-overdue collector over readings through the
// daemon's own builder, so the duty-name rule in attention.go is under test
// rather than a stub.
func dutyAttentionItems(t *testing.T, f *attentionFixture, readings []JobReading) []attention.Item {
	t.Helper()
	src := f.src.d.attentionSources(jobOverdueNow)
	src.jobList = func(time.Time) ([]JobReading, error) { return readings, nil }
	items, err := collectorFor(t, src, attention.KindJobOverdue).collect(context.Background())
	if err != nil {
		t.Fatalf("collector: %v", err)
	}
	return items
}

// An overdue duty row raises the job-overdue item while the reconciler patrol
// is enabled: a duty name is on with the reconciler, so the alert is not
// silenced (gt-nnnn4o).
func TestDutyAttentionRaisesOverdueRow(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, jobOverdueNow)
	duty := JobReading{Name: "duty:backup", State: jobStateOverdue, LastRun: jobOverdueNow.Add(-3 * 24 * time.Hour), Cadence: 24 * time.Hour}
	items := dutyAttentionItems(t, f, []JobReading{duty})
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one for %s with the reconciler on", items, duty.Name)
	}
	if got, want := items[0].Key, "job-overdue:duty:backup"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}

// A duty row in state unknown whose newest outcome failed raises its item: the
// failed outcome outranks the unknown state (gt-nnnn4o).
func TestDutyAttentionRaisesFailedUnknown(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, jobOverdueNow)
	duty := JobReading{Name: "duty:archive", State: jobStateUnknown, Outcome: jobOutcomeFailed}
	items := dutyAttentionItems(t, f, []JobReading{duty})
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one for a failed %s", items, duty.Name)
	}
	if got, want := items[0].Key, "job-overdue:duty:archive"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}
