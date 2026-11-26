package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
