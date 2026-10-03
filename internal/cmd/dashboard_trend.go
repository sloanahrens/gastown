package cmd

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// The trend panel is the last 24 hours: landings and rejections by local hour,
// the gate and om time of the newest landings, and the host load seen in each
// hour. The landings and stage lines come from the readers the om panel
// already uses; the load history is the one thing the dashboard writes.

const (
	trendHours     = 24
	trendMaxStages = 150
	trendLoadKeep  = 24 * time.Hour
)

// localHour is the local hour t falls in. Landings and stage lines are stamped
// in UTC, so the conversion has to come first: truncating in the writer's own
// zone would bucket those records by their UTC hour.
func localHour(t time.Time) time.Time {
	t = t.In(time.Local)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.Local)
}

// trendWindowStart is the oldest moment the 24 hourly buckets cover.
func trendWindowStart(now time.Time) time.Time {
	return localHour(now).Add(-time.Duration(trendHours-1) * time.Hour)
}

// buildTrend folds the landings, the landing worker's stage lines and
// rejections, and the host's load samples into the last 24 hours. It is pure:
// a record outside the window is not counted, an empty hour is present with
// zeros, and only the newest trendMaxStages stage points are kept.
func buildTrend(now time.Time, recs []omRecord, stages []omStage, rejs []omRejection, loads []dashboard.LoadPoint) *dashboard.Trend {
	base := localHour(now)
	hours := make([]dashboard.TrendHour, trendHours)
	idx := make(map[time.Time]int, trendHours)
	for i := range hours {
		hours[i].Hour = base.Add(time.Duration(i-(trendHours-1)) * time.Hour)
		idx[hours[i].Hour] = i
	}
	bucket := func(at time.Time) (int, bool) {
		i, ok := idx[localHour(at)]
		return i, ok
	}

	for _, rec := range recs {
		if i, ok := bucket(rec.LandedAt); ok {
			hours[i].Landed++
		}
	}
	for _, rj := range rejs {
		if i, ok := bucket(rj.At); ok {
			hours[i].Rejected++
		}
	}

	type loadAcc struct {
		sum, max float64
		n        int
	}
	acc := map[int]*loadAcc{}
	for _, p := range loads {
		i, ok := bucket(p.At)
		if !ok {
			continue
		}
		a := acc[i]
		if a == nil {
			a = &loadAcc{max: p.Load}
			acc[i] = a
		}
		a.sum += p.Load
		a.n++
		if p.Load > a.max {
			a.max = p.Load
		}
	}
	for i, a := range acc {
		avg, max := a.sum/float64(a.n), a.max
		hours[i].LoadAvg, hours[i].LoadMax = &avg, &max
	}

	tr := &dashboard.Trend{Hours: hours}
	for _, st := range stages {
		if _, ok := bucket(st.At); !ok {
			continue
		}
		tr.Stages = append(tr.Stages, dashboard.TrendPoint{
			At: st.At, LintSecs: durSecs(st.Lint), GateSecs: durSecs(st.Gate), OMSecs: secsPtr(st.OM),
		})
	}
	sort.SliceStable(tr.Stages, func(i, j int) bool { return tr.Stages[i].At.Before(tr.Stages[j].At) })
	if n := len(tr.Stages); n > trendMaxStages {
		tr.Stages = append([]dashboard.TrendPoint(nil), tr.Stages[n-trendMaxStages:]...)
	}
	return tr
}

func durSecs(d *time.Duration) float64 {
	if d == nil {
		return 0
	}
	return d.Seconds()
}

func secsPtr(d *time.Duration) *float64 {
	if d == nil {
		return nil
	}
	v := d.Seconds()
	return &v
}

// loadSample is one line of the load history file.
type loadSample struct {
	At   time.Time `json:"at"`
	Load float64   `json:"load"`
}

// dashLoads is the dashboard's record of the host's load: one sample per
// machine poll, appended to <town>/.runtime/dashboard-load.jsonl so a restart
// keeps the 24-hour trend. That file is the only thing the dashboard writes. A
// missing or unreadable file is no history, never an error the page shows.
type dashLoads struct {
	path string
	now  func() time.Time

	mu  sync.Mutex
	pts []dashboard.LoadPoint
}

func newDashLoads(townRoot string, now func() time.Time) *dashLoads {
	l := &dashLoads{path: filepath.Join(constants.TownRuntimePath(townRoot), "dashboard-load.jsonl"), now: now}
	read := l.read()
	l.pts = l.trim(read)
	if len(read) > 0 {
		l.rewrite(l.pts)
	}
	return l
}

// append records one machine sample in memory and appends it to the file. A
// write that fails leaves the in-memory history serving the page.
func (l *dashLoads) append(at time.Time, load float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pts = l.trim(append(l.pts, dashboard.LoadPoint{At: at, Load: load}))
	if len(l.pts) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(loadLine(nil, l.pts[len(l.pts)-1]))
}

// points returns the load history, oldest first.
func (l *dashLoads) points() []dashboard.LoadPoint {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]dashboard.LoadPoint(nil), l.pts...)
}

// read parses the file's JSON lines. A line that is not a sample is skipped.
func (l *dashLoads) read() []dashboard.LoadPoint {
	f, err := os.Open(l.path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []dashboard.LoadPoint
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var s loadSample
		if json.Unmarshal(sc.Bytes(), &s) == nil && !s.At.IsZero() {
			out = append(out, dashboard.LoadPoint{At: s.At, Load: s.Load})
		}
	}
	return out
}

// trim drops the samples older than the trend window.
func (l *dashLoads) trim(pts []dashboard.LoadPoint) []dashboard.LoadPoint {
	cutoff := l.now().Add(-trendLoadKeep)
	var kept []dashboard.LoadPoint
	for _, p := range pts {
		if !p.At.Before(cutoff) {
			kept = append(kept, p)
		}
	}
	return kept
}

// rewrite replaces the file with the kept samples, trimming what a previous
// run left behind.
func (l *dashLoads) rewrite(pts []dashboard.LoadPoint) {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	var b []byte
	for _, p := range pts {
		b = loadLine(b, p)
	}
	_ = os.WriteFile(l.path, b, 0o644)
}

func loadLine(b []byte, p dashboard.LoadPoint) []byte {
	line, err := json.Marshal(loadSample{At: p.At, Load: p.Load})
	if err != nil {
		return b
	}
	return append(append(b, line...), '\n')
}
