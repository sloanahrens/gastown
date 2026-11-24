package reconcile

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier's checks: no goroutine outlives its tests, and
// no test starts an external tool (docs/testing.md, "The rules"). The package
// starts nothing, so unittier.Main is enough.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// testNow is the fixed now every test evaluates against, so no test reads the
// real clock.
var testNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func TestReconcileEvaluateAges(t *testing.T) {
	t.Parallel()
	d := Duty{Name: "backup", Limit: 24 * time.Hour, Grace: 4 * time.Hour}
	tests := []struct {
		name string
		age  time.Duration
		want Verdict
	}{
		{"fresh result is ok", 0, VerdictOK},
		{"exactly at limit is ok", d.Limit, VerdictOK},
		{"one second past limit is due", d.Limit + time.Second, VerdictDue},
		{"exactly at limit plus grace is due", d.Limit + d.Grace, VerdictDue},
		{"one second past limit plus grace is overdue", d.Limit + d.Grace + time.Second, VerdictOverdue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(d, testNow, testNow.Add(-tt.age), nil)
			if got.Verdict != tt.want {
				t.Fatalf("Evaluate at age %s: verdict = %q, want %q", tt.age, got.Verdict, tt.want)
			}
			if got.Age != tt.age {
				t.Errorf("Evaluate at age %s: Age = %s", tt.age, got.Age)
			}
			if got.Duty != d.Name {
				t.Errorf("Duty = %q, want %q", got.Duty, d.Name)
			}
		})
	}
}

func TestReconcileEvaluateNoResultIsOverdue(t *testing.T) {
	t.Parallel()
	d := Duty{Name: "archive", Limit: 24 * time.Hour, Grace: 4 * time.Hour}
	tests := []struct {
		name    string
		readErr error
	}{
		{"no result is overdue", ErrNoResult},
		{"wrapped no result is overdue", fmt.Errorf("read archive marker: %w", ErrNoResult)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(d, testNow, time.Time{}, tt.readErr)
			if got.Verdict != VerdictOverdue {
				t.Fatalf("verdict = %q, want %q", got.Verdict, VerdictOverdue)
			}
			if !got.LastGood.IsZero() {
				t.Errorf("LastGood = %v, want the zero time", got.LastGood)
			}
			if got.Err != tt.readErr {
				t.Errorf("Err = %v, want the read error kept", got.Err)
			}
		})
	}
}

func TestReconcileEvaluateUnreadableIsUnknown(t *testing.T) {
	t.Parallel()
	d := Duty{Name: "gc", Limit: 24 * time.Hour, Grace: 4 * time.Hour}
	tests := []struct {
		name     string
		lastGood time.Time
		readErr  error
	}{
		{"other read error is unknown", time.Time{}, errors.New("permission denied")},
		{"read error with a last good time is unknown", testNow.Add(-time.Hour), errors.New("i/o timeout")},
		{"last good after now is unknown", testNow.Add(time.Second), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(d, testNow, tt.lastGood, tt.readErr)
			if got.Verdict != VerdictUnknown {
				t.Fatalf("verdict = %q, want %q", got.Verdict, VerdictUnknown)
			}
			if got.Err != tt.readErr {
				t.Errorf("Err = %v, want %v kept", got.Err, tt.readErr)
			}
		})
	}
}
