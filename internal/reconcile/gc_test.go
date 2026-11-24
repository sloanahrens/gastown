package reconcile

import (
	"errors"
	"testing"
	"time"
)

func TestReconcileGCObservationsPerDatabase(t *testing.T) {
	t.Parallel()
	duty := Duty{Name: "gc", Limit: 164 * time.Hour, Grace: 4 * time.Hour}
	lastGC := map[string]time.Time{
		"gt": testNow.Add(-2 * time.Hour),
		"hm": testNow.Add(-8 * 24 * time.Hour),
	}
	// Listed out of order so the returned order proves the name sort.
	got := GCObservations(duty, testNow, lastGC, []string{"toolbox", "hm", "gt"})

	want := []struct {
		duty    string
		verdict Verdict
	}{
		{"gc:gt", VerdictOK},
		{"gc:hm", VerdictOverdue},
		{"gc:toolbox", VerdictOverdue},
	}
	if len(got) != len(want) {
		t.Fatalf("GCObservations returned %d observations, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Duty != w.duty || got[i].Verdict != w.verdict {
			t.Errorf("observation %d = %s %s, want %s %s", i, got[i].Duty, got[i].Verdict, w.duty, w.verdict)
		}
	}
	if !errors.Is(got[2].Err, ErrNoResult) {
		t.Errorf("gc:toolbox Err = %v, want ErrNoResult", got[2].Err)
	}
}

func TestReconcileGCObservationsIgnoresUnlisted(t *testing.T) {
	t.Parallel()
	duty := Duty{Name: "gc", Limit: 164 * time.Hour, Grace: 4 * time.Hour}
	lastGC := map[string]time.Time{
		"gt":         testNow.Add(-2 * time.Hour),
		"retired-db": testNow.Add(-30 * 24 * time.Hour),
	}
	got := GCObservations(duty, testNow, lastGC, []string{"gt"})
	if len(got) != 1 {
		t.Fatalf("GCObservations returned %d observations, want 1: %+v", len(got), got)
	}
	if got[0].Duty != "gc:gt" || got[0].Verdict != VerdictOK {
		t.Errorf("observation = %s %s, want gc:gt ok", got[0].Duty, got[0].Verdict)
	}
}
