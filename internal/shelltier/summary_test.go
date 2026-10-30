package shelltier

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

func TestSummaryGolden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		r    Result
		want string
	}{
		{
			name: "green",
			r:    Result{Passed: 28, Elapsed: 287 * time.Second},
			want: "tier-sweep: shell GREEN passed=28 failed=0 skipped=0 (logs /d) in 4m47s",
		},
		{
			name: "red with two names",
			r: Result{Passed: 5, Failed: 2,
				FailedNames: []string{"scripts/a_test.sh", "scripts/b_test.sh"},
				Elapsed:     134 * time.Second},
			want: "tier-sweep: shell RED passed=5 failed=2 skipped=0 failed: scripts/a_test.sh scripts/b_test.sh (logs /d) in 2m14s",
		},
		{
			name: "empty tier is red",
			r:    Result{},
			want: "tier-sweep: shell RED passed=0 failed=0 skipped=0 (logs /d) in 0s",
		},
		{
			name: "skips alone are green",
			r:    Result{Skipped: 3},
			want: "tier-sweep: shell GREEN passed=0 failed=0 skipped=3 (logs /d) in 0s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Summary(tt.r, "/d"); got != tt.want {
				t.Errorf("Summary() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"under a minute", 59 * time.Second, "59s"},
		{"one minute", 60 * time.Second, "1m0s"},
		{"just under an hour", 3599 * time.Second, "59m59s"},
		{"one hour", 3600 * time.Second, "1h0m0s"},
		{"hour minute second", 3661 * time.Second, "1h1m1s"},
		{"rounds down below half", 1400 * time.Millisecond, "1s"},
		{"rounds up at half", 1500 * time.Millisecond, "2s"},
		{"rounds up into the next minute", 59600 * time.Millisecond, "1m0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FormatDuration(tt.d); got != tt.want {
				t.Errorf("FormatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

// TestSummaryAgreesWithTiersweep checks the adapters against the one formatter
// they wrap, so the line is not only pinned to literal strings.
func TestSummaryAgreesWithTiersweep(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, 45 * time.Second, 59600 * time.Millisecond, 134 * time.Second, time.Hour} {
		t.Run("duration "+d.String(), func(t *testing.T) {
			t.Parallel()
			want := tiersweep.FormatDuration(int(d.Round(time.Second) / time.Second))
			if got := FormatDuration(d); got != want {
				t.Errorf("FormatDuration(%v) = %q, tiersweep gives %q", d, got, want)
			}
		})
	}
	tests := []struct {
		name string
		r    Result
	}{
		{"green", Result{Passed: 28, Elapsed: 287 * time.Second}},
		{"red with two names", Result{Passed: 5, Failed: 2,
			FailedNames: []string{"scripts/a_test.sh", "scripts/b_test.sh"},
			Elapsed:     134 * time.Second}},
		{"empty tier", Result{}},
		{"skips alone", Result{Skipped: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want, red := tiersweep.Summary("shell", "/d", tt.r.Passed, tt.r.Failed, tt.r.Skipped,
				strings.Join(tt.r.FailedNames, " "), int(tt.r.Elapsed.Round(time.Second)/time.Second))
			if got := Summary(tt.r, "/d"); got != want {
				t.Errorf("Summary() =\n  %q\ntiersweep gives\n  %q", got, want)
			}
			if got := tt.r.Exit() == ExitRed; got != red {
				t.Errorf("Exit() is red=%v, tiersweep verdict red=%v", got, red)
			}
		})
	}
}

func TestExitCodes(t *testing.T) {
	t.Parallel()
	if ExitGreen != 0 || ExitRed != 1 {
		t.Fatalf("ExitGreen=%d ExitRed=%d, want 0 and 1", ExitGreen, ExitRed)
	}
	tests := []struct {
		name string
		r    Result
		want Exit
	}{
		{"all passed", Result{Passed: 3}, ExitGreen},
		{"skips alone", Result{Skipped: 2}, ExitGreen},
		{"one failure", Result{Passed: 2, Failed: 1, FailedNames: []string{"x"}}, ExitRed},
		{"empty tier", Result{}, ExitRed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.r.Exit(); got != tt.want {
				t.Errorf("Exit() = %d, want %d", got, tt.want)
			}
		})
	}
}
