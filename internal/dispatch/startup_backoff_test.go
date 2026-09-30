package dispatch

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestStartupBackoff_NoRecordAllowsDispatch(t *testing.T) {
	if got := startupBackoff(t.TempDir(), "gt-abc", time.Now()); got != "" {
		t.Errorf("no failure recorded, got backoff %q", got)
	}
}

func TestStartupBackoff_HoldsWithinWindowThenReleases(t *testing.T) {
	town := t.TempDir()
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if err := recordStartupFailure(town, "gt-abc", "startup blocked: trust dialog", at); err != nil {
		t.Fatal(err)
	}

	got := startupBackoff(town, "gt-abc", at.Add(StartupBackoffBase-time.Second))
	if !strings.Contains(got, "startup blocked: trust dialog") {
		t.Errorf("inside the window the backoff should name the failure, got %q", got)
	}
	if got := startupBackoff(town, "gt-abc", at.Add(StartupBackoffBase)); got != "" {
		t.Errorf("window elapsed, want dispatch allowed, got %q", got)
	}
	if got := startupBackoff(town, "gt-other", at); got != "" {
		t.Errorf("another bead must be unaffected, got %q", got)
	}
}

func TestStartupBackoff_ConsecutiveFailuresBackOffExponentially(t *testing.T) {
	town := t.TempDir()
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for range 2 {
		if err := recordStartupFailure(town, "gt-abc", "startup blocked", at); err != nil {
			t.Fatal(err)
		}
	}
	if got := startupBackoff(town, "gt-abc", at.Add(StartupBackoffBase)); got == "" {
		t.Error("second consecutive failure should rest longer than the base window")
	}
	if got := startupBackoff(town, "gt-abc", at.Add(2*StartupBackoffBase)); got != "" {
		t.Errorf("second failure rests 2x base, got %q at 2x", got)
	}
}

func TestStartupBackoffWindow_DoublesAndCaps(t *testing.T) {
	cases := map[int]time.Duration{
		1:   5 * time.Minute,
		2:   10 * time.Minute,
		3:   20 * time.Minute,
		4:   40 * time.Minute,
		5:   StartupBackoffMax,
		100: StartupBackoffMax,
	}
	for count, want := range cases {
		if got := startupBackoffWindow(count); got != want {
			t.Errorf("window(%d) = %s, want %s", count, got, want)
		}
	}
}

func TestClearStartupFailure_ResetsTheBackoffAndTheCount(t *testing.T) {
	town := t.TempDir()
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	_ = recordStartupFailure(town, "gt-abc", "startup blocked", at)
	_ = recordStartupFailure(town, "gt-abc", "startup blocked", at)

	ClearStartupFailure(town, "gt-abc")
	if got := startupBackoff(town, "gt-abc", at); got != "" {
		t.Errorf("cleared, got backoff %q", got)
	}
	_ = recordStartupFailure(town, "gt-abc", "startup blocked", at)
	if got := startupBackoff(town, "gt-abc", at.Add(StartupBackoffBase)); got != "" {
		t.Errorf("count should restart at 1 after a clear, got %q", got)
	}
	ClearStartupFailure(town, "gt-never-failed") // must not panic or error
}

func TestStartupBackoff_UnreadableRecordFailsOpen(t *testing.T) {
	town := t.TempDir()
	if err := recordStartupFailure(town, "gt-abc", "x", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(startupFailurePath(town, "gt-abc"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := StartupBackoff(town, "gt-abc"); got != "" {
		t.Errorf("a corrupt record must not park the bead, got %q", got)
	}
}

func TestRecordStartupFailure_RejectsPathLikeBeadIDs(t *testing.T) {
	town := t.TempDir()
	for _, id := range []string{"", ".", "..", "../x", "a/b"} {
		if err := recordStartupFailure(town, id, "x", time.Now()); err == nil {
			t.Errorf("bead ID %q should be refused", id)
		}
	}
}

func TestRecordStartupFailure_ReasonIsOneBoundedLine(t *testing.T) {
	town := t.TempDir()
	at := time.Now()
	_ = recordStartupFailure(town, "gt-abc", "\nfirst line\nsecond line", at)
	rec, ok := readStartupFailure(town, "gt-abc")
	if !ok || rec.Reason != "first line" {
		t.Errorf("reason = %+v, want first non-empty line", rec)
	}
	_ = recordStartupFailure(town, "gt-abc", strings.Repeat("x", 5*startupReasonMax), at)
	rec, _ = readStartupFailure(town, "gt-abc")
	if len(rec.Reason) != startupReasonMax {
		t.Errorf("reason length %d, want %d", len(rec.Reason), startupReasonMax)
	}
}
