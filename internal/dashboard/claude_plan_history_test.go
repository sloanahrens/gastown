package dashboard

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// planHistoryRows decodes the history at path as the reader decodes it. A path
// with no file has no rows.
func planHistoryRows(t *testing.T, path string) []ClaudePlanWeek {
	t.Helper()
	weeks, err := readClaudePlanWeeks(path)
	if err != nil {
		t.Fatalf("reading the weekly history at %s: %v", path, err)
	}
	return weeks
}

// The history path names where closeouts are appended. The series-side file is
// still read, so the weeks recorded there stay in the month rollup, and a window
// both files hold counts once and is never appended again.
func TestClaudePlanHistoryPath(t *testing.T) {
	t.Parallel()

	closed := planDate(2026, time.October, 12)
	closedLine := planLine(planDate(2026, time.October, 11), 5, 44, closed.Unix())
	openLine := planLine(claudePlanNow, 4, 14, planDate(2026, time.October, 19).Unix())
	closedWeek := ClaudePlanWeek{
		StartAt:  closed.Add(-claudePlanWeekDays * 24 * time.Hour).Unix(),
		ResetsAt: closed.Unix(),
		FinalPct: 44,
	}

	t.Run("a closed week goes to the given path and not beside the series", func(t *testing.T) {
		t.Parallel()
		path := claudePlanFile(t, closedLine, openLine)
		history := filepath.Join(t.TempDir(), "weeks.jsonl")

		p := NewClaudePlanReader(path, 200).WithHistoryPath(history).Read(claudePlanNow)

		if p.StateError != "" {
			t.Fatalf("state_error = %q, want none", p.StateError)
		}
		if rows := planHistoryRows(t, history); len(rows) != 1 || rows[0] != closedWeek {
			t.Errorf("the given history holds %+v, want the one closeout %+v", rows, closedWeek)
		}
		if _, err := os.Stat(claudePlanWeeksPath(path)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a closeout was written beside the series: stat err = %v", err)
		}
	})

	t.Run("a week only beside the series still counts in the month", func(t *testing.T) {
		t.Parallel()
		path := claudePlanFile(t, openLine)
		if err := appendClaudePlanWeeks(claudePlanWeeksPath(path), []ClaudePlanWeek{closedWeek}); err != nil {
			t.Fatal(err)
		}
		history := filepath.Join(t.TempDir(), "weeks.jsonl")

		p := NewClaudePlanReader(path, 200).WithHistoryPath(history).Read(claudePlanNow)

		if p.StateError != "" {
			t.Fatalf("state_error = %q, want none", p.StateError)
		}
		// The series-side closeout and the open week are the month's two weeks.
		if p.WeeksCounted != 2 {
			t.Errorf("month_weeks = %d, want the series-side closeout and the open week", p.WeeksCounted)
		}
		if rows := planHistoryRows(t, history); len(rows) != 0 {
			t.Errorf("the series-side week was copied to the given history: %+v", rows)
		}
	})

	t.Run("a week both files hold counts once and is not appended again", func(t *testing.T) {
		t.Parallel()
		path := claudePlanFile(t, closedLine, openLine)
		if err := appendClaudePlanWeeks(claudePlanWeeksPath(path), []ClaudePlanWeek{closedWeek}); err != nil {
			t.Fatal(err)
		}
		history := filepath.Join(t.TempDir(), "weeks.jsonl")
		if err := appendClaudePlanWeeks(history, []ClaudePlanWeek{closedWeek}); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(history)
		if err != nil {
			t.Fatal(err)
		}

		p := NewClaudePlanReader(path, 200).WithHistoryPath(history).Read(claudePlanNow)

		if p.StateError != "" {
			t.Fatalf("state_error = %q, want none", p.StateError)
		}
		if p.WeeksCounted != 2 {
			t.Errorf("month_weeks = %d, want the closeout once and the open week", p.WeeksCounted)
		}
		after, err := os.ReadFile(history)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Errorf("the given history changed for a week it already held: %q", after)
		}
	})

	t.Run("with no option the series-side file is appended as today", func(t *testing.T) {
		t.Parallel()
		path := claudePlanFile(t, closedLine, openLine)

		p := NewClaudePlanReader(path, 200).Read(claudePlanNow)

		if p.StateError != "" {
			t.Fatalf("state_error = %q, want none", p.StateError)
		}
		if rows := planWeeksFile(t, path); len(rows) != 1 || rows[0] != closedWeek {
			t.Errorf("the series-side history holds %+v, want the one closeout %+v", rows, closedWeek)
		}
	})

	t.Run("an unreadable history path is reported and nothing is appended", func(t *testing.T) {
		t.Parallel()
		path := claudePlanFile(t, closedLine, openLine)
		// A directory cannot be read as a history file.
		history := t.TempDir()

		p := NewClaudePlanReader(path, 200).WithHistoryPath(history).Read(claudePlanNow)

		if !strings.Contains(p.StateError, "weekly history") {
			t.Errorf("state_error = %q, want it to name the weekly history", p.StateError)
		}
		if p.WeeksCounted != 2 {
			t.Errorf("month_weeks = %d, want the closeout from the samples and the open week", p.WeeksCounted)
		}
		if entries, err := os.ReadDir(history); err != nil || len(entries) != 0 {
			t.Errorf("the unreadable history gained entries: %v (err %v)", entries, err)
		}
		if _, err := os.Stat(claudePlanWeeksPath(path)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a closeout was written beside the series: stat err = %v", err)
		}
	})
}
