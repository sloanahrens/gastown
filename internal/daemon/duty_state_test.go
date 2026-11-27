package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reconcile"
)

// TestDutyStateBuild covers how observations become the duty state record.
func TestDutyStateBuild(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	okLast := now.Add(-90 * time.Second)
	overdueLast := now.Add(-2 * time.Hour)
	// A last good time after now is the unknown verdict with a negative age,
	// and it is the case where the age is not a whole number of seconds.
	futureLast := now.Add(1500 * time.Millisecond)
	obs := []reconcile.Observation{
		{Duty: "backup", LastGood: okLast, Age: 90 * time.Second, Verdict: reconcile.VerdictOK},
		{Duty: "gc", LastGood: overdueLast, Age: 2 * time.Hour, Verdict: reconcile.VerdictOverdue},
		{Duty: "archive", LastGood: futureLast, Age: -1500 * time.Millisecond, Verdict: reconcile.VerdictUnknown},
	}
	limits := map[string]time.Duration{
		"backup":  10 * time.Minute,
		"gc":      30 * time.Minute,
		"archive": 6 * time.Hour,
	}

	t.Run("each observation becomes a record in order", func(t *testing.T) {
		t.Parallel()
		state := newDutyState(now, obs, limits)
		if !state.At.Equal(now) {
			t.Errorf("At = %v, want %v", state.At, now)
		}
		want := []DutyRecord{
			{Name: "backup", Verdict: reconcile.VerdictOK, LastGood: okLast, AgeSeconds: 90, LimitSeconds: 600},
			{Name: "gc", Verdict: reconcile.VerdictOverdue, LastGood: overdueLast, AgeSeconds: 7200, LimitSeconds: 1800},
			{Name: "archive", Verdict: reconcile.VerdictUnknown, LastGood: futureLast, AgeSeconds: -1, LimitSeconds: 21600},
		}
		if len(state.Duties) != len(want) {
			t.Fatalf("got %d duty records, want %d", len(state.Duties), len(want))
		}
		for i := range want {
			if got := state.Duties[i]; got != want[i] {
				t.Errorf("record %d = %+v, want %+v", i, got, want[i])
			}
		}
	})

	t.Run("a duty missing from limits records a zero limit", func(t *testing.T) {
		t.Parallel()
		state := newDutyState(now, []reconcile.Observation{
			{Duty: "town-state", Verdict: reconcile.VerdictUnknown},
		}, limits)
		if got := state.Duties[0].LimitSeconds; got != 0 {
			t.Errorf("LimitSeconds = %d, want 0", got)
		}
	})

	t.Run("no observations records no duties", func(t *testing.T) {
		t.Parallel()
		if got := newDutyState(now, nil, limits); len(got.Duties) != 0 {
			t.Errorf("got %d duty records, want none", len(got.Duties))
		}
	})
}

// TestDutyStateWriteRecords covers the record WriteDutyState leaves: the file
// under daemon/ holds newDutyState's result as JSON at mode 0644.
func TestDutyStateWriteRecords(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	obs := []reconcile.Observation{
		{Duty: "backup", LastGood: now.Add(-90 * time.Second), Age: 90 * time.Second, Verdict: reconcile.VerdictOK},
		{Duty: "gc", LastGood: now.Add(-2 * time.Hour), Age: 2 * time.Hour, Verdict: reconcile.VerdictOverdue},
	}
	limits := map[string]time.Duration{"backup": 10 * time.Minute, "gc": 30 * time.Minute}

	if err := WriteDutyState(townRoot, now, obs, limits); err != nil {
		t.Fatalf("WriteDutyState = %v, want no error", err)
	}
	path := filepath.Join(townRoot, "daemon", "duty_state.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.MarshalIndent(newDutyState(now, obs, limits), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("duty_state.json = %s, want the state record %s", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("duty_state.json mode = %04o, want 0644", perm)
	}
}

// TestDutyStateWriteIsAtomic covers how a later write replaces the record and
// what a publish the directory refuses leaves behind: a second write puts a new
// file in place rather than editing the old one, and a refused rename returns
// the error with the target untouched and no temp file beside it.
func TestDutyStateWriteIsAtomic(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	t.Run("a second write replaces the file instead of editing it", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := WriteDutyState(townRoot, now, nil, nil); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(townRoot, "daemon", "duty_state.json")
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteDutyState(townRoot, now.Add(time.Minute), nil, nil); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(before, after) {
			t.Error("the second write edited the state file in place, so a reader can see a half-written record")
		}
	})

	t.Run("a rename the directory refuses returns the error and keeps the entry", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		dir := filepath.Join(townRoot, "daemon")
		occupied := filepath.Join(dir, "duty_state.json")
		keep := filepath.Join(occupied, "keep")
		if err := os.MkdirAll(occupied, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keep, []byte("previous"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := WriteDutyState(townRoot, now, nil, nil)
		if err == nil {
			t.Fatal("WriteDutyState onto a directory = nil, want an error")
		}
		if !strings.Contains(err.Error(), occupied) {
			t.Errorf("error %q does not name the file %q", err, occupied)
		}
		if data, err := os.ReadFile(keep); err != nil || string(data) != "previous" {
			t.Errorf("the entry at the target path = %q, %v after the failed write, want it untouched", data, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() != "duty_state.json" {
				t.Errorf("the failed write left %q beside the target", entry.Name())
			}
		}
	})

	t.Run("an empty town root is refused", func(t *testing.T) {
		t.Parallel()
		if err := WriteDutyState("", now, nil, nil); err == nil {
			t.Error("WriteDutyState with no town root = nil, want an error")
		}
	})
}

// TestDutyStateRoundTrip covers what ReadDutyState gives back after
// WriteDutyState: the same time and the same records, in the same order.
func TestDutyStateRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	obs := []reconcile.Observation{
		{Duty: "backup", LastGood: now.Add(-90 * time.Second), Age: 90 * time.Second, Verdict: reconcile.VerdictOK},
		{Duty: "gc", LastGood: now.Add(-2 * time.Hour), Age: 2 * time.Hour, Verdict: reconcile.VerdictOverdue},
		{Duty: "town-state", Verdict: reconcile.VerdictUnknown},
	}
	limits := map[string]time.Duration{"backup": 10 * time.Minute, "gc": 30 * time.Minute}

	townRoot := t.TempDir()
	if err := WriteDutyState(townRoot, now, obs, limits); err != nil {
		t.Fatalf("WriteDutyState = %v, want no error", err)
	}
	got, err := ReadDutyState(townRoot)
	if err != nil {
		t.Fatalf("ReadDutyState = %v, want no error", err)
	}
	if want := newDutyState(now, obs, limits); !reflect.DeepEqual(got, want) {
		t.Errorf("ReadDutyState = %+v, want %+v", got, want)
	}
}

// TestDutyStateReadFailures covers the reads that must fail. A missing file
// wraps fs.ErrNotExist; any other failure is an error that is not mistaken for
// absence. None of them returns an empty state.
func TestDutyStateReadFailures(t *testing.T) {
	t.Parallel()

	t.Run("a missing file satisfies errors.Is fs.ErrNotExist", func(t *testing.T) {
		t.Parallel()
		got, err := ReadDutyState(t.TempDir())
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ReadDutyState error = %v, want one wrapping fs.ErrNotExist", err)
		}
		if !reflect.DeepEqual(got, DutyState{}) {
			t.Errorf("ReadDutyState = %+v with the error, want the zero state", got)
		}
	})

	for _, tc := range []struct {
		name    string
		content string
	}{
		{"a file that is not JSON", "{not json"},
		{"an empty file", ""},
		{"a file holding null", "null"},
	} {
		t.Run(tc.name+" is an error", func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			path := dutyStatePath(townRoot)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, err := ReadDutyState(townRoot); err == nil || errors.Is(err, fs.ErrNotExist) {
				t.Errorf("ReadDutyState = %+v, %v; want an error that is not absence", got, err)
			}
		})
	}

	t.Run("a directory at the path is unreadable, not absent", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := os.MkdirAll(dutyStatePath(townRoot), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadDutyState(townRoot); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadDutyState onto a directory = %v, want an error that is not absence", err)
		}
	})

	t.Run("an empty town root is refused before any read", func(t *testing.T) {
		t.Parallel()
		// Without the refusal the read is of a path relative to the test's
		// directory, which fails as absent; the refusal must not look like that.
		if _, err := ReadDutyState(""); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadDutyState with no town root = %v, want the refusal", err)
		}
	})
}
