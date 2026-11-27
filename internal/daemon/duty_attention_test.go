package daemon

import (
	"context"
	"testing"
	"time"
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
