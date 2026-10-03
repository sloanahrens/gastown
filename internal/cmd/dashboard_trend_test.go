package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

func dur(d time.Duration) *time.Duration { return &d }

// An empty town still has a full set of local hours, oldest first, all zero.
func TestBuildTrendEmptyHasEveryHour(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 30, 0, 0, time.Local)
	tr := buildTrend(now, nil, nil, nil, nil)
	if len(tr.Hours) != trendHours {
		t.Fatalf("hours = %d, want %d", len(tr.Hours), trendHours)
	}
	if want := time.Date(2026, 10, 2, 19, 0, 0, 0, time.Local); !tr.Hours[0].Hour.Equal(want) {
		t.Errorf("first hour = %v, want %v", tr.Hours[0].Hour, want)
	}
	if want := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local); !tr.Hours[trendHours-1].Hour.Equal(want) {
		t.Errorf("last hour = %v, want %v", tr.Hours[trendHours-1].Hour, want)
	}
	for i, h := range tr.Hours {
		if h.Landed != 0 || h.Rejected != 0 || h.LoadAvg != nil || h.LoadMax != nil {
			t.Errorf("hour %d = %+v, want zeroes and no load", i, h)
		}
	}
	if len(tr.Stages) != 0 {
		t.Errorf("stages = %+v, want none", tr.Stages)
	}
}

// Landings and rejections land in their own local hour, on both sides of
// midnight, and one older than the window is left out.
func TestBuildTrendCountsHourAndSpansMidnight(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 1, 30, 0, 0, time.Local)
	at := func(hour int, day int, minute int) time.Time {
		return time.Date(2026, 10, day, hour, minute, 0, 0, time.Local)
	}
	recs := []omRecord{
		omTestRec("gt-a", "approve", 0.9, "daemon", at(23, 2, 30)), // previous evening
		omTestRec("gt-b", "approve", 0.9, "daemon", at(0, 3, 10)),
		omTestRec("gt-c", "approve", 0.9, "daemon", at(1, 3, 5)),
		omTestRec("gt-old", "approve", 0.9, "daemon", at(12, 1, 0)), // outside the window
	}
	rejs := []omRejection{{At: at(0, 3, 20), Bead: "gt-r", Kind: "review"}}

	tr := buildTrend(now, recs, nil, rejs, nil)
	byHour := map[int]dashboard.TrendHour{}
	for _, h := range tr.Hours {
		byHour[h.Hour.Hour()] = h
	}
	if h := byHour[23]; h.Landed != 1 || h.Rejected != 0 {
		t.Errorf("hour 23 (previous day) = %+v", h)
	}
	if h := byHour[0]; h.Landed != 1 || h.Rejected != 1 {
		t.Errorf("hour 0 = %+v", h)
	}
	if h := byHour[1]; h.Landed != 1 || h.Rejected != 0 {
		t.Errorf("hour 1 = %+v", h)
	}
	total := 0
	for _, h := range tr.Hours {
		total += h.Landed
	}
	if total != 3 {
		t.Errorf("counted %d landings, want 3 (the window drops the rest)", total)
	}
}

// The same instant recorded in local time and in UTC must be one bucket: the
// landings files and the daemon log stamp in UTC, the hours are local.
func TestBuildTrendCountsAUTCInstantInItsLocalHour(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 30, 0, 0, time.Local)
	local := time.Date(2026, 10, 3, 18, 10, 0, 0, time.Local)
	tr := buildTrend(now, []omRecord{
		omTestRec("gt-a", "approve", 0.9, "daemon", local),
		omTestRec("gt-b", "approve", 0.9, "daemon", local.UTC()),
	}, nil, nil, nil)
	if got := tr.Hours[trendHours-1].Landed; got != 2 {
		t.Errorf("landed in the last hour = %d, want 2 (a UTC record counts in its local hour)", got)
	}
}

// Only the newest stage points survive, loads average and max per hour, and an
// hour with no sample keeps nil load fields.
func TestBuildTrendStageLimitAndLoad(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	var stages []omStage
	for i := 0; i < 200; i++ {
		stages = append(stages, omStage{At: now.Add(-time.Duration(i) * time.Minute), Bead: "gt-x", Lint: dur(2 * time.Second), Gate: dur(40 * time.Second), OM: dur(90 * time.Second)})
	}
	stages = append(stages, omStage{At: now.Add(-25 * time.Hour), Bead: "gt-old", Gate: dur(5 * time.Second)})
	hour := localHour(now)
	loads := []dashboard.LoadPoint{
		{At: hour.Add(-time.Hour).Add(10 * time.Minute), Load: 2},
		{At: hour.Add(-time.Hour).Add(40 * time.Minute), Load: 6},
		{At: hour, Load: 9},
	}

	tr := buildTrend(now, nil, stages, nil, loads)
	if len(tr.Stages) != trendMaxStages {
		t.Fatalf("stages = %d, want the %d newest", len(tr.Stages), trendMaxStages)
	}
	if want := now.Add(-149 * time.Minute); !tr.Stages[0].At.Equal(want) {
		t.Errorf("oldest kept stage = %v, want %v (the newest %d)", tr.Stages[0].At, want, trendMaxStages)
	}
	if !tr.Stages[len(tr.Stages)-1].At.Equal(now) {
		t.Errorf("newest kept stage = %v, want %v", tr.Stages[len(tr.Stages)-1].At, now)
	}
	if s := tr.Stages[0]; s.GateSecs != 40 || s.LintSecs != 2 || s.OMSecs == nil || *s.OMSecs != 90 {
		t.Errorf("stage point = %+v", s)
	}
	byHour := map[int]dashboard.TrendHour{}
	for _, h := range tr.Hours {
		byHour[h.Hour.Hour()] = h
	}
	if h := byHour[17]; h.LoadAvg == nil || *h.LoadAvg != 4 || h.LoadMax == nil || *h.LoadMax != 6 {
		t.Errorf("hour 17 load = avg %v max %v, want 4 and 6", h.LoadAvg, h.LoadMax)
	}
	if h := byHour[18]; h.LoadAvg == nil || *h.LoadAvg != 9 || h.LoadMax == nil || *h.LoadMax != 9 {
		t.Errorf("hour 18 load = avg %v max %v, want 9 and 9", h.LoadAvg, h.LoadMax)
	}
	if h := byHour[12]; h.LoadAvg != nil || h.LoadMax != nil {
		t.Errorf("an hour with no sample must have nil load, got %+v", h)
	}
}

// The load history file is trimmed at startup, tolerates garbage and a missing
// file, and gains a line per append.
func TestBuildTrendLoadHistoryFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	l := newDashLoads(dir, func() time.Time { return now })
	if pts := l.points(); len(pts) != 0 {
		t.Fatalf("a missing file must be no history, got %+v", pts)
	}

	path := filepath.Join(dir, ".runtime", "dashboard-load.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-30 * time.Hour)
	recent := now.Add(-time.Hour)
	body := "not a sample\n" +
		`{"at":"` + old.Format(time.RFC3339) + `","load":1.5}` + "\n" +
		`{"at":"` + recent.Format(time.RFC3339) + `","load":3.25}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	l = newDashLoads(dir, func() time.Time { return now })
	pts := l.points()
	if len(pts) != 1 || pts[0].Load != 3.25 || !pts[0].At.Equal(recent) {
		t.Fatalf("points = %+v, want only the recent sample", pts)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "not a sample") || strings.Contains(string(after), old.Format(time.RFC3339)) {
		t.Errorf("startup must trim the file, got %q", after)
	}

	l.append(now.Add(-time.Minute), 5.5)
	pts = l.points()
	if len(pts) != 2 || pts[1].Load != 5.5 {
		t.Fatalf("after append = %+v", pts)
	}
	after, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `"load":5.5`) {
		t.Errorf("append must write the file, got %q", after)
	}
}
