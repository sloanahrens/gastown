package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// plantMark puts a mark at path: a directory when dir, else the given bytes.
func plantMark(t *testing.T, path, body string, dir bool) {
	t.Helper()
	var err error
	if dir {
		err = os.MkdirAll(path, 0o755)
	} else if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		err = os.WriteFile(path, []byte(body), 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// TestTierSweepInFlightMarkRoundTrip: a written mark reads back its commit and
// start time from the tier-sweep directory, no mark reads as fs.ErrNotExist,
// and clearing removes it and clearing again is no error (gt-rfsbkw).
func TestTierSweepInFlightMarkRoundTrip(t *testing.T) {
	t.Parallel()
	town, started := t.TempDir(), time.Date(2026, 11, 27, 5, 13, 0, 0, time.UTC)
	if _, err := readTierSweepInFlight(town, "gastown"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read with no mark = %v, want fs.ErrNotExist", err)
	}
	if err := writeTierSweepInFlight(town, "gastown", tierSweepInFlightMark{SHA: "abc123", Started: started}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := tierSweepInFlightPath(town, "gastown"), filepath.Join(town, ".runtime", "tier-sweep", "gastown.inflight.json"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if mark, err := readTierSweepInFlight(town, "gastown"); err != nil || mark.SHA != "abc123" || !mark.Started.Equal(started) {
		t.Errorf("read = %+v, %v, want abc123 started %v", mark, err, started)
	}
	if err := clearTierSweepInFlight(town, "gastown"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := readTierSweepInFlight(town, "gastown"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read after clear = %v, want fs.ErrNotExist", err)
	}
	if err := clearTierSweepInFlight(town, "gastown"); err != nil {
		t.Errorf("clear with no mark = %v, want nil", err)
	}
}

// TestTierSweepInFlightMarkRefusesOneItCannotUse: a mark that is unreadable, or
// does not parse, or names no commit or no start time, is an error.
func TestTierSweepInFlightMarkRefusesOneItCannotUse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body string
		dir        bool
	}{
		{"unreadable", "", true},
		{"not JSON", "{not json", false},
		{"no commit", `{"started":"2026-11-27T05:13:00Z"}`, false},
		{"no start time", `{"sha":"abc123"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			plantMark(t, tierSweepInFlightPath(town, "gastown"), tc.body, tc.dir)
			if mark, err := readTierSweepInFlight(town, "gastown"); err == nil {
				t.Errorf("read = %+v, want an error", mark)
			}
		})
	}
}

// TestTierSweepInFlightMarkReportsFailures: a mark that cannot be written or
// removed returns the error rather than reading as written or cleared.
func TestTierSweepInFlightMarkReportsFailures(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := tierSweepInFlightPath(town, "gastown")
	plantMark(t, path, "", true)
	mark := tierSweepInFlightMark{SHA: "abc123", Started: time.Date(2026, 11, 27, 5, 13, 0, 0, time.UTC)}
	if err := writeTierSweepInFlight(town, "gastown", mark); err == nil {
		t.Error("write over a directory = nil, want an error")
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := clearTierSweepInFlight(town, "gastown"); err == nil {
		t.Error("clear of a non-empty directory = nil, want an error")
	}
}
