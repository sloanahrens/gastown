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
)

// homeClaudePlanFile is where the status line writes its series under a home.
func homeClaudePlanFile(home string) string {
	return filepath.Join(home, ".claude", ".runtime", "claude-plan", "samples.jsonl")
}

// TestPlanMeterFileHomeSeriesOutranksTheTownCopy is the resolver's table: the
// home series answers when it exists, and every other case answers with the
// town's copy, naming the reason when the cause is not plain absence.
func TestPlanMeterFileHomeSeriesOutranksTheTownCopy(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	home := t.TempDir()
	townFile := dashboardClaudePlanPath(town)
	statFails := errors.New("permission denied")
	cases := []struct {
		name    string
		home    func() (string, error)
		stat    func(string) (fs.FileInfo, error)
		want    string
		wantLog string
	}{
		{"home series exists", func() (string, error) { return home, nil },
			func(string) (fs.FileInfo, error) { return nil, nil }, homeClaudePlanFile(home), ""},
		{"home series absent", func() (string, error) { return home, nil },
			func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }, townFile, ""},
		{"no home directory", func() (string, error) { return "", errors.New("no HOME") },
			func(string) (fs.FileInfo, error) { return nil, nil }, townFile, "no home directory"},
		{"stat fails for another reason", func() (string, error) { return home, nil },
			func(string) (fs.FileInfo, error) { return nil, statFails }, townFile, "cannot stat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logged []string
			got := resolveClaudePlanFile(town, tc.home, tc.stat, planWhyLogInto(&logged))
			if got != tc.want {
				t.Errorf("series = %q, want %q", got, tc.want)
			}
			all := strings.Join(logged, "\n")
			if tc.wantLog == "" && all != "" {
				t.Errorf("a plain case logged %q", all)
			}
			if tc.wantLog != "" && !strings.Contains(all, tc.wantLog) {
				t.Errorf("log %q does not hold %q", all, tc.wantLog)
			}
		})
	}
}

// TestPlanMeterReadsTheFreshHomeSeriesWhileTheTownCopyIsDaysOld is the
// incident: the town's copy holds a week-old sample, the status line's copy
// under the home is current, and there are no claude_plan rows. The meter
// reads the home copy and is neither stale nor unavailable.
func TestPlanMeterReadsTheFreshHomeSeriesWhileTheTownCopyIsDaysOld(t *testing.T) {
	t.Parallel()
	town, home := t.TempDir(), t.TempDir()
	writeClaudePlanSampleAt(t, dashboardClaudePlanPath(town), fixedClaudePlanNow.Add(-6*24*time.Hour), 59)
	writeClaudePlanSampleAt(t, homeClaudePlanFile(home), fixedClaudePlanNow.Add(-time.Minute), 38)
	read := newDashboardClaudePlanRead(town, rowsOf(), func() (string, error) { return home, nil }, os.Stat,
		planWhyLogInto(nil), newDashUsageReader(town), dashboardClaudePlanUSD, 18, func() time.Time { return fixedClaudePlanNow })

	p := read()

	if p.Unavailable || p.Stale {
		t.Fatalf("the meter is unavailable=%v stale=%v, want a live reading", p.Unavailable, p.Stale)
	}
	if p.WeeklyPct != 38 {
		t.Errorf("weekly pct = %v, want 38 from the home series", p.WeeklyPct)
	}
}

// planWhyLogInto is a why-log whose lines are appended to logged, which may be
// nil.
func planWhyLogInto(logged *[]string) *dashWhyLog {
	return newDashWhyLog(func(format string, args ...any) {
		if logged != nil {
			*logged = append(*logged, fmt.Sprintf(format, args...))
		}
	})
}
