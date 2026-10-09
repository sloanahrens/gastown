package cmd

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// recentLandingRows is how many finished landings and rejections the Landings
// table shows. Ten fits the pane at a glance; the live rows, running and failing
// in backoff, sit above it and are not counted in it.
const recentLandingRows = 10

// buildRecentLandings joins the last 24 hours of landings and rejections with
// the stage lines that say what each cost, newest first, capped at limit. live
// is the landings in flight, which go above the cap rather than inside it: a
// landing running now is not one of the day's finished rows. The title of a
// bead is asked for only once the rows to show are known, so the lookups are
// bounded by limit, not by the day's traffic. ship reads a landing's ship
// reading off the deploy tracker, by its rig because what ships a landing
// depends on the rig; a rejection never carries one.
func buildRecentLandings(now time.Time, recs []omRecord, stages []omStage, rejs []omRejection, live []dashboard.LandingRow, ship func(rig, bead string) tailShip, title func(rig, bead string) string, limit int) []dashboard.LandingRow {
	since := now.Add(-trendHours * time.Hour)
	var rows []dashboard.LandingRow
	rigOf := map[string]string{} // a rejection line names no rig; a landing of the same bead does
	for _, rec := range recs {
		rigOf[rec.Bead] = rec.Rig
	}
	timing := func(r *dashboard.LandingRow, st *omStage) {
		if st == nil {
			return
		}
		r.LintSecs, r.GateSecs, r.OMSecs = secsPtr(st.Lint), secsPtr(st.Gate), secsPtr(st.OM)
	}
	for _, rec := range recs {
		if rec.LandedAt.Before(since) {
			continue
		}
		row := dashboard.LandingRow{
			At: rec.LandedAt, Bead: rec.Bead, Rig: rec.Rig, Polecat: polecatOfBranch(rec.Branch),
			Outcome: "landed", Verdict: omOutcome(rec.OMVerdict), Commit: shortSHA(rec.LandedCommit),
			Route: rec.Route, Risk: len(rec.RiskPaths) > 0,
		}
		if rec.OMScore > 0 {
			sc := rec.OMScore
			row.Score = &sc
		}
		if row.Verdict == "error" {
			row.Detail = truncateRunes(rec.OMVerdict, 140)
		}
		if ship != nil {
			s := ship(rec.Rig, rec.Bead)
			row.ShipSecs, row.ShipPending = s.Secs, s.Pending
			row.ShipVia, row.ShipFailed, row.ShipRunState = s.Via, s.Failed, s.RunState
		}
		timing(&row, omStageFor(stages, rec.Bead, rec.LandedAt))
		rows = append(rows, row)
	}
	for _, rj := range rejs {
		if rj.At.Before(since) {
			continue
		}
		rig := rigOf[rj.Bead]
		if rig == "" {
			rig = "gastown" // the only rig with a landing worker running
		}
		row := dashboard.LandingRow{At: rj.At, Bead: rj.Bead, Rig: rig, Outcome: "rejected", Kind: rj.Kind, Score: rj.Score, Detail: truncateRunes(rj.Detail, 160)}
		timing(&row, omStageFor(stages, rj.Bead, rj.At))
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.After(rows[j].At) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if len(live) > 0 {
		rows = append(append([]dashboard.LandingRow(nil), live...), rows...)
	}
	if title != nil {
		seen := map[string]string{}
		for i := range rows {
			key := rows[i].Rig + "/" + rows[i].Bead
			t, ok := seen[key]
			if !ok {
				t = title(rows[i].Rig, rows[i].Bead)
				seen[key] = t
			}
			rows[i].Title = t
		}
	}
	return rows
}

// runningRows is the landings in flight as Landings rows, newest merge first.
// A live row carries what the merge line holds and nothing the landing has not
// logged yet: no verdict, score or stage times, because the stages line is
// written only once the landing ends. rigFor resolves a bead that has no
// landings record yet to the rig its prefix routes to; polecatFor names who
// holds the bead, read from the bead store once per live row.
func runningRows(live []omMerge, rigFor func(bead string) string, polecatFor func(rig, bead string) string) []dashboard.LandingRow {
	var rows []dashboard.LandingRow
	for _, m := range live {
		rig := ""
		if rigFor != nil {
			rig = rigFor(m.Bead)
		}
		row := dashboard.LandingRow{At: m.At, Bead: m.Bead, Rig: rig, Outcome: "running"}
		if polecatFor != nil {
			row.Polecat = polecatFor(rig, m.Bead)
		}
		rows = append(rows, row)
	}
	return rows
}

// polecatOfAssignee names the polecat a bead is assigned to, off the
// "<rig>/polecats/<name>" assignee a dispatched bead carries. It is empty for
// any other holder: a crew member, or a bead no one has taken.
func polecatOfAssignee(assignee string) string {
	parts := strings.Split(strings.TrimSpace(assignee), "/")
	if len(parts) == 3 && parts[1] == "polecats" {
		return parts[2]
	}
	return ""
}

// landingRig resolves a bead's rig the way a daemon.log line is routed: through
// the town's routes table, read once by the caller and kept. A bead no route
// claims resolves to "", leaving the row's rig blank rather than naming a rig
// that does not hold it.
func landingRig(townRoot string) func(bead string) string {
	rigs := loadTailRigs(townRoot)
	return func(bead string) string {
		if rig := rigs.bead(bead); rig != "town" {
			return rig
		}
		return ""
	}
}

// polecatOfBranch names who built a landing from its branch: the polecat in a
// "polecat/<name>/<bead>+<nonce>" branch, or the first path element of any
// other branch ("sloan/dashboard" is sloan's, a crew or operator landing). It
// is empty when the branch says nothing.
func polecatOfBranch(branch string) string {
	parts := strings.Split(branch, "/")
	switch {
	case len(parts) >= 3 && parts[0] == "polecat":
		return parts[1]
	case len(parts) >= 2 && parts[0] != "polecat" && parts[0] != "":
		return parts[0]
	}
	return ""
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
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
