package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// claudePlanSeriesLine is one status line sample, the way the status line
// appends it, with the weekly percentage sevenDay and the weekly reset sevenResets.
func claudePlanSeriesLine(at time.Time, sevenDay int, sevenResets time.Time) string {
	return fmt.Sprintf(`{"t":%d,"five_hour":8,"five_hour_resets_at":%d,"seven_day":%d,"seven_day_resets_at":%d}`,
		at.Unix(), at.Add(time.Hour).Unix(), sevenDay, sevenResets.Unix())
}

// TestDashboardClaudePlanWeeksPath: the weekly closeouts are appended under the
// town's runtime directory, and a series read from another directory leaves
// nothing beside itself.
func TestDashboardClaudePlanWeeksPath(t *testing.T) {
	t.Parallel()

	t.Run("the history lives under the town's runtime directory", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		want := filepath.Join(root, ".runtime", "claude-plan", "weeks.jsonl")
		if got := dashboardClaudePlanWeeksPath(root); got != want {
			t.Errorf("the closeout history is %q, want %q", got, want)
		}
	})

	t.Run("a series read from another directory closes its week under the town", func(t *testing.T) {
		t.Parallel()

		town := t.TempDir()
		home := t.TempDir()
		series := homeClaudePlanFile(home)
		if err := os.MkdirAll(filepath.Dir(series), 0o755); err != nil {
			t.Fatal(err)
		}
		// The week that reset on the 12th closed at 44% (its last reading is the
		// 11th's), and the week open now reads 14%.
		closed := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
		body := claudePlanSeriesLine(closed.Add(-24*time.Hour), 44, closed) + "\n" +
			claudePlanSeriesLine(fixedClaudePlanNow, 14, fixedClaudePlanNow.Add(4*24*time.Hour)) + "\n"
		if err := os.WriteFile(series, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}

		read := newDashboardClaudePlanRead(town, rowsOf(), func() (string, error) { return home, nil }, os.Stat,
			newDashWhyLog(func(string, ...any) {}), newDashUsageReader(town),
			dashboardClaudePlanUSD, dashboard.ClaudePlanDefaultRenewalDay, func() time.Time { return fixedClaudePlanNow })
		p := read()

		if p.StateError != "" {
			t.Fatalf("state_error = %q, want none: the closeout could not be recorded", p.StateError)
		}
		weeks := dashboardClaudePlanWeeksPath(town)
		got, err := os.ReadFile(weeks)
		if err != nil {
			t.Fatalf("the closeout was not appended under the town: %v", err)
		}
		if lines := strings.Count(strings.TrimSpace(string(got)), "\n") + 1; lines != 1 {
			t.Errorf("the town's history holds %d lines, want the one closeout:\n%s", lines, got)
		}
		beside := filepath.Join(filepath.Dir(series), "weeks.jsonl")
		if _, err := os.Stat(beside); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a closeout was written beside the series at %s: stat err = %v", beside, err)
		}
	})

	t.Run("a town whose history directory cannot be made reports the failed closeout", func(t *testing.T) {
		t.Parallel()

		home := t.TempDir()
		series := homeClaudePlanFile(home)
		if err := os.MkdirAll(filepath.Dir(series), 0o755); err != nil {
			t.Fatal(err)
		}
		closed := time.Date(2026, time.October, 12, 12, 0, 0, 0, time.UTC)
		body := claudePlanSeriesLine(closed.Add(-24*time.Hour), 44, closed) + "\n" +
			claudePlanSeriesLine(fixedClaudePlanNow, 14, fixedClaudePlanNow.Add(4*24*time.Hour)) + "\n"
		if err := os.WriteFile(series, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		// The town root is a file, so its runtime directory cannot exist.
		town := filepath.Join(t.TempDir(), "town-is-a-file")
		if err := os.WriteFile(town, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		read := newDashboardClaudePlanRead(town, rowsOf(), func() (string, error) { return home, nil }, os.Stat,
			newDashWhyLog(func(string, ...any) {}), newDashUsageReader(home),
			dashboardClaudePlanUSD, dashboard.ClaudePlanDefaultRenewalDay, func() time.Time { return fixedClaudePlanNow })
		p := read()

		if !strings.Contains(p.StateError, "weekly history") {
			t.Errorf("state_error = %q, want the failed closeout named", p.StateError)
		}
		if p.WeeksCounted == 0 {
			t.Errorf("the failed closeout took the week out of the reading: %+v", p)
		}
	})
}
