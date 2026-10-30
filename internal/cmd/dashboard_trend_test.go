package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/polecat/spans"
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

// Loads average and max per hour, and an hour with no sample keeps nil load
// fields.
func TestBuildTrendLoadPerHour(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	hour := localHour(now)
	loads := []dashboard.LoadPoint{
		{At: hour.Add(-time.Hour).Add(10 * time.Minute), Load: 2},
		{At: hour.Add(-time.Hour).Add(40 * time.Minute), Load: 6},
		{At: hour, Load: 9},
	}

	tr := buildTrend(now, nil, nil, nil, loads)
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

// More stage points than the chart draws are thinned across the whole 24 hours,
// not cut to the newest few hours: every bucket keeps its slowest point, and the
// slowest point of the window survives.
func TestBuildTrendThinsStagesAcrossTheWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	start := localHour(now).Add(-(trendHours - 1) * time.Hour)
	span := trendHours * time.Hour
	const n, slowIdx = 400, 250
	var stages []omStage
	for i := range n {
		gate := 40 * time.Second
		if i == slowIdx {
			gate = 300 * time.Second
		}
		stages = append(stages, omStage{At: start.Add(time.Duration(i) * span / n), Bead: "gt-x", Gate: dur(gate), OM: dur(90 * time.Second)})
	}
	slowAt := start.Add(time.Duration(slowIdx) * span / n)

	tr := buildTrend(now, nil, stages, nil, nil)
	if len(tr.Stages) != trendMaxStages {
		t.Fatalf("stages = %d, want %d (one per bucket)", len(tr.Stages), trendMaxStages)
	}
	bucket := span / trendMaxStages
	if at := tr.Stages[0].At; at.Sub(start) >= bucket {
		t.Errorf("oldest kept stage = %v, want in the first bucket from %v", at, start)
	}
	if at := tr.Stages[len(tr.Stages)-1].At; at.Sub(start) < span-bucket {
		t.Errorf("newest kept stage = %v, want in the last bucket of the window", at)
	}
	slowKept := false
	for i, p := range tr.Stages {
		if i > 0 && !p.At.After(tr.Stages[i-1].At) {
			t.Errorf("stage %d at %v is not after the one before it at %v", i, p.At, tr.Stages[i-1].At)
		}
		slowKept = slowKept || p.At.Equal(slowAt)
	}
	if !slowKept {
		t.Errorf("the slowest stage at %v was thinned away", slowAt)
	}
}

// Under the cap every stage point is returned as it was, oldest first.
func TestBuildTrendKeepsEveryStageUnderTheCap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	var stages []omStage
	for i := range 100 {
		stages = append(stages, omStage{At: now.Add(-time.Duration(i) * 10 * time.Minute), Bead: "gt-x", Gate: dur(time.Duration(i+1) * time.Second)})
	}

	tr := buildTrend(now, nil, stages, nil, nil)
	if len(tr.Stages) != len(stages) {
		t.Fatalf("stages = %d, want all %d", len(tr.Stages), len(stages))
	}
	for i, p := range tr.Stages {
		want := now.Add(-time.Duration(99-i) * 10 * time.Minute)
		if !p.At.Equal(want) || p.GateSecs != float64(100-i) {
			t.Fatalf("stage %d = %v gate %v, want %v gate %d", i, p.At, p.GateSecs, want, 100-i)
		}
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

func TestBuildRecentLandingsJoinsStagesAndOrdersNewestFirst(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	d := func(s int) *time.Duration { v := time.Duration(s) * time.Second; return &v }
	recs := []omRecord{
		{Record: landings.Record{Bead: "gt-old", Rig: "gastown", Branch: "polecat/agate/gt-old+x", LandedCommit: "0123456789abcdef", OMVerdict: "approve", OMScore: 0.8, Route: "daemon", LandedAt: now.Add(-3 * time.Hour)}},
		{Record: landings.Record{Bead: "gt-new", Rig: "gastown", Branch: "polecat/basalt/gt-new+y", LandedCommit: "fedcba9876543210", OMVerdict: "approve", OMScore: 0.9, Route: "daemon", LandedAt: now.Add(-10 * time.Minute)}, RiskPaths: []string{"internal/git/git.go"}},
		{Record: landings.Record{Bead: "gt-manual", Rig: "gastown", Branch: "sloan/dashboard", OMVerdict: "skipped", Route: "overseer-manual", LandedAt: now.Add(-5 * time.Hour)}},
		{Record: landings.Record{Bead: "gt-ancient", Rig: "gastown", OMVerdict: "approve", LandedAt: now.Add(-30 * time.Hour)}},
	}
	stages := []omStage{{At: now.Add(-11 * time.Minute), Bead: "gt-new", Lint: d(12), Gate: d(34), OM: d(100)}}
	score := 0.55
	rejs := []omRejection{{At: now.Add(-20 * time.Minute), Bead: "gt-new", Kind: "review", Detail: "om requested changes (score 0.55, 5 finding(s))", Score: &score}}

	var asked []string
	rows := buildRecentLandings(now, recs, stages, rejs, nil, nil, func(rig, id string) string { asked = append(asked, rig+"/"+id); return "title of " + id }, nil, nil, nil, 30)

	if len(rows) != 4 {
		t.Fatalf("%d rows, want 4 (the 30-hour-old landing is outside the day): %+v", len(rows), rows)
	}
	order := []string{rows[0].Bead + ":" + rows[0].Outcome, rows[1].Bead + ":" + rows[1].Outcome, rows[2].Bead, rows[3].Bead}
	if order[0] != "gt-new:landed" || order[1] != "gt-new:rejected" || order[2] != "gt-old" || order[3] != "gt-manual" {
		t.Errorf("order = %v, want newest first", order)
	}
	r := rows[0]
	if r.Polecat != "basalt" || r.Commit != "fedcba98" || !r.Risk || r.Verdict != "approved" || r.Title != "title of gt-new" {
		t.Errorf("landed row = %+v", r)
	}
	if r.GateSecs == nil || *r.GateSecs != 34 || r.OMSecs == nil || *r.OMSecs != 100 || r.LintSecs == nil || *r.LintSecs != 12 {
		t.Errorf("stage times not joined: %+v", r)
	}
	rj := rows[1]
	if rj.Kind != "review" || rj.Rig != "gastown" || rj.Score == nil || *rj.Score != 0.55 || rj.Detail == "" {
		t.Errorf("rejected row = %+v", rj)
	}
	if rows[3].Polecat != "sloan" || rows[3].Verdict != "skipped" {
		t.Errorf("a crew landing names its builder, and landed without review: %+v", rows[3])
	}
	if len(asked) != 3 {
		t.Errorf("titles asked %d times (%v), want once per bead: gt-new, gt-old, gt-manual", len(asked), asked)
	}
}

// Every landing in the window, up to the table's row bound, is a row, newest
// first, so the pane can scroll all of them; a title is looked up only for the
// first recentTitleRows of them, since each lookup can be a bd read (gt-1kttu).
func TestBuildRecentLandingsKeepsEveryRowAndAsksTitlesOnlyForTheFirstTen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	var recs []omRecord
	for i := 0; i < 50; i++ {
		recs = append(recs, omRecord{Record: landings.Record{Bead: fmt.Sprintf("gt-%d", i), Rig: "gastown", OMVerdict: "approve", LandedAt: now.Add(-time.Duration(i) * time.Minute)}})
	}
	asked := 0
	rows := buildRecentLandings(now, recs, nil, nil, nil, nil, func(rig, id string) string { asked++; return "" }, nil, nil, nil, recentLandingRows)
	if len(rows) != 50 {
		t.Fatalf("rows = %d, want every landing in the window (50): the pane scrolls them, nothing is cut at ten", len(rows))
	}
	if asked != recentTitleRows {
		t.Fatalf("title lookups = %d, want %d: a title read can cost a bd call, so only the first rows are looked up", asked, recentTitleRows)
	}
	if rows[0].Bead != "gt-0" || rows[49].Bead != "gt-49" {
		t.Errorf("not newest first: first %s last %s", rows[0].Bead, rows[49].Bead)
	}
}

// TestBuildRecentLandingsFillsShipTime covers the four rows the Ship column
// tells apart: a deployed landing shows its dispatch-to-deploy seconds, a
// landed bead with a dispatch line shows pending, a hand-slung bead with no
// dispatch line shows neither, and a rejection never carries a ship time. The
// tracker runs on a fixed clock and a fake ancestry, so nothing waits on a
// real clock or a real git.
func TestBuildRecentLandingsFillsShipTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	track := newTailDeploys(func() time.Time { return now }, fakeTailAncestry("1111aaaa 2222bbbb", "4444dddd 2222bbbb"), now.Add(-24*time.Hour))
	track.observe([]tailLine{
		daemonAt("2026-10-03T17:00:00Z", "spec_dispatch: dispatched: gt-deployed: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-10-03T17:03:00Z", "landing_worker: [land] gt-deployed: landed 1111aaaa on origin/main (patch-id 9e9e)"),
		daemonAt("2026-10-03T17:04:00Z", "spec_dispatch: dispatched: gt-pending: slung to gastown/jade on x (seat 1/3)"),
		daemonAt("2026-10-03T17:06:00Z", "landing_worker: [land] gt-pending: landed 3333cccc on origin/main (patch-id 9e9e)"),
		daemonAt("2026-10-03T17:07:00Z", "landing_worker: [land] gt-slung: landed 4444dddd on origin/main (patch-id 9e9e)"),
		// One restart installs gt-deployed and the hand-slung gt-slung; it does
		// not contain gt-pending's commit, so that bead stays waiting.
		daemonAt("2026-10-03T17:10:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
		daemonAt("2026-10-03T17:15:00Z", "landing_worker: [land] gt-rejected: rejected (review): om requested changes"),
	})
	recs := []omRecord{
		omTestRec("gt-deployed", "approve", 0.9, "daemon", at("2026-10-03T17:03:00Z")),
		omTestRec("gt-pending", "approve", 0.9, "daemon", at("2026-10-03T17:06:00Z")),
		omTestRec("gt-slung", "skipped", 0, "overseer-manual", at("2026-10-03T17:07:00Z")),
	}
	rejs := []omRejection{{At: at("2026-10-03T17:15:00Z"), Bead: "gt-rejected", Kind: "review"}}

	rows := buildRecentLandings(now, recs, nil, rejs, nil, track.shipStatus, func(string, string) string { return "" }, nil, nil, nil, 30)
	byBead := map[string]dashboard.LandingRow{}
	for _, r := range rows {
		byBead[r.Bead+":"+r.Outcome] = r
	}
	if r := byBead["gt-deployed:landed"]; r.ShipSecs == nil || *r.ShipSecs != 600 || r.ShipPending {
		t.Errorf("deployed row ship = %v pending %v, want 600s", r.ShipSecs, r.ShipPending)
	}
	if r := byBead["gt-pending:landed"]; r.ShipSecs != nil || !r.ShipPending {
		t.Errorf("pending row ship = %v pending %v, want pending", r.ShipSecs, r.ShipPending)
	}
	if r := byBead["gt-slung:landed"]; r.ShipSecs != nil || r.ShipPending {
		t.Errorf("hand-slung row ship = %v pending %v, want neither", r.ShipSecs, r.ShipPending)
	}
	if r := byBead["gt-rejected:rejected"]; r.ShipSecs != nil || r.ShipPending {
		t.Errorf("rejected row ship = %v pending %v, want neither", r.ShipSecs, r.ShipPending)
	}
}

// TestBuildRecentLandingsShipCellPerRig covers what the Ship cell says for each
// ship definition: gastown's restart time reads as dispatched-to-deployed, an
// app rig's staging time as dispatched-to-staging-deployed, an app landing
// whose staging run failed carries the run's state, and a rig with no ship
// definition (devops) shows a dash and is never pending (gt-lqqjj).
func TestBuildRecentLandingsShipCellPerRig(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	track := newTailDeploys(func() time.Time { return now }, fakeTailAncestry("1111aaaa 2222bbbb"), now.Add(-24*time.Hour))
	track.setShipDefinitions("gastown",
		shipRigOf(map[string]string{
			"gt-shipped": "gastown", "fr-shipped": "fractals", "bv-broken": "beaver", "dv-nothing": "devops",
		}),
		fakeStaging(map[string]dashboard.StagingShip{
			"fractals\x00aaaa1111": {State: dashboard.StagingDeployed, At: at("2026-10-03T17:20:00Z")},
			"beaver\x00bbbb2222":   {State: dashboard.StagingFailed, RunState: "failure"},
		}),
	)
	track.observe([]tailLine{
		daemonAt("2026-10-03T17:00:00Z", "spec_dispatch: dispatched: gt-shipped: slung to gastown/opal on x (seat 1/3)"),
		daemonAt("2026-10-03T17:03:00Z", "landing_worker: [land] gt-shipped: landed 1111aaaa on origin/main (patch-id 9e9e)"),
		daemonAt("2026-10-03T17:04:00Z", "upgrade-restart: running 2222bbbb covers marker 2222bbbb; cleared"),
		daemonAt("2026-10-03T17:00:00Z", "spec_dispatch: dispatched: fr-shipped: slung to fractals/opal on x (seat 1/3)"),
		daemonAt("2026-10-03T17:02:00Z", "landing_worker: [land] fr-shipped: landed aaaa1111 on origin/main (patch-id 1)"),
		daemonAt("2026-10-03T17:00:00Z", "spec_dispatch: dispatched: bv-broken: slung to beaver/opal on x (seat 1/3)"),
		daemonAt("2026-10-03T17:02:00Z", "landing_worker: [land] bv-broken: landed bbbb2222 on origin/main (patch-id 2)"),
		daemonAt("2026-10-03T17:00:00Z", "spec_dispatch: dispatched: dv-nothing: slung to devops/opal on x (seat 1/3)"),
		daemonAt("2026-10-03T17:02:00Z", "landing_worker: [land] dv-nothing: landed dddd3333 on origin/main (patch-id 3)"),
	})
	recs := []omRecord{
		{Record: landings.Record{Bead: "gt-shipped", Rig: "gastown", OMVerdict: "approve", LandedAt: at("2026-10-03T17:03:00Z")}},
		{Record: landings.Record{Bead: "fr-shipped", Rig: "fractals", OMVerdict: "approve", LandedAt: at("2026-10-03T17:02:00Z")}},
		{Record: landings.Record{Bead: "bv-broken", Rig: "beaver", OMVerdict: "approve", LandedAt: at("2026-10-03T17:02:00Z")}},
		{Record: landings.Record{Bead: "dv-nothing", Rig: "devops", OMVerdict: "approve", LandedAt: at("2026-10-03T17:02:00Z")}},
	}

	rows := buildRecentLandings(now, recs, nil, nil, nil, track.shipStatus, func(string, string) string { return "" }, nil, nil, nil, 30)
	byBead := map[string]dashboard.LandingRow{}
	for _, r := range rows {
		byBead[r.Bead] = r
	}
	if r := byBead["gt-shipped"]; r.ShipSecs == nil || r.ShipVia != "deploy" || r.ShipPending {
		t.Errorf("gt-shipped ship = %v via %q pending %v, want a deploy time", r.ShipSecs, r.ShipVia, r.ShipPending)
	}
	if r := byBead["fr-shipped"]; r.ShipSecs == nil || *r.ShipSecs != 1200 || r.ShipVia != "staging" || r.ShipPending {
		t.Errorf("fr-shipped ship = %v via %q pending %v, want 1200s via staging", r.ShipSecs, r.ShipVia, r.ShipPending)
	}
	if r := byBead["bv-broken"]; r.ShipSecs != nil || !r.ShipPending || !r.ShipFailed || r.ShipRunState != "failure" {
		t.Errorf("bv-broken ship = %v pending %v failed %v state %q, want staging failed", r.ShipSecs, r.ShipPending, r.ShipFailed, r.ShipRunState)
	}
	if r := byBead["dv-nothing"]; r.ShipSecs != nil || r.ShipPending || r.ShipFailed || r.ShipVia != "" {
		t.Errorf("dv-nothing ship = %+v, want a dash", r)
	}
}

func TestPolecatOfBranch(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"polecat/agate/gt-1+abc": "agate", "polecat/x": "", "sloan/dashboard": "sloan", "": "", "polecat/mica/gt-2.1+q": "mica", "main": "",
		"crew/heron/dv-zqw7.4": "heron", "crew/jay/gt-byiyo": "jay", "crew/heron": "heron", "crew": "",
		"crew//gt-1": "", "crew/": "",
	} {
		if got := polecatOfBranch(in); got != want {
			t.Errorf("polecatOfBranch(%q) = %q, want %q", in, got, want)
		}
	}
}

// A rejected landing's By column names the polecat or crew member that
// submitted it: the one on its last submitted branch, "polecat/<name>/..." or
// "crew/<name>/...". A bead whose submission names no branch, and one with no
// submission at all, draws the dash — never a model name, which is not a
// submitter (gt-gclus).
func TestBuildRecentLandingsNamesARejectedRowFromItsBranch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 20, 30, 0, 0, time.UTC)
	subs := []land.SubmissionRecord{
		// gt-resubmitted was submitted twice: the newest branch is the name.
		{Rig: "gastown", Bead: "gt-resubmitted", Branch: "polecat/mica/gt-resubmitted+old", SeenAt: now.Add(-3 * time.Hour)},
		{Rig: "gastown", Bead: "gt-resubmitted", Branch: "polecat/agate/gt-resubmitted+new", SeenAt: now.Add(-time.Hour)},
		{Rig: "gastown", Bead: "gt-crew", Branch: "crew/heron/gt-js6di-merge", SeenAt: now.Add(-2 * time.Hour)},
		{Rig: "gastown", Bead: "gt-nobranch", Branch: "", SeenAt: now.Add(-2 * time.Hour)},
	}
	rejs := []omRejection{
		{At: now.Add(-50 * time.Minute), Bead: "gt-resubmitted", Kind: "review"},
		{At: now.Add(-50 * time.Minute), Bead: "gt-crew", Kind: "conflict"},
		{At: now.Add(-50 * time.Minute), Bead: "gt-nobranch", Kind: "review"},
		{At: now.Add(-50 * time.Minute), Bead: "gt-unknown", Kind: "review"},
	}

	rows := buildRecentLandings(now, nil, nil, rejs, nil, nil, nil, landingNameLookup(subs), nil, nil, 30)
	byBead := map[string]dashboard.LandingRow{}
	for _, r := range rows {
		byBead[r.Bead] = r
	}
	for bead, want := range map[string]string{
		"gt-resubmitted": "agate",
		"gt-crew":        "heron",
		"gt-nobranch":    "",
		"gt-unknown":     "",
	} {
		if got := byBead[bead].Polecat; got != want {
			t.Errorf("%s By = %q, want %q", bead, got, want)
		}
	}

	// A caller with no lookup names no one: the rejection is still a row, and
	// nothing is read to fill its By in.
	for _, r := range buildRecentLandings(now, nil, nil, rejs, nil, nil, nil, nil, nil, nil, 30) {
		if r.Polecat != "" {
			t.Errorf("%s By = %q with no lookup, want a dash", r.Bead, r.Polecat)
		}
	}
}

// A rejection reassigns the bead, so by the time the row is drawn the bead's
// holder is a different polecat: the running row the caller built names the new
// holder (mica), and the rejected row still names the agent on the last
// submitted branch (agate). Reading the bead instead would name the wrong one
// (gt-gclus).
func TestAReassignedBeadsRejectedRowStillNamesItsSubmitter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 20, 30, 0, 0, time.UTC)
	subs := []land.SubmissionRecord{
		{Rig: "gastown", Bead: "gt-reassigned", Branch: "polecat/agate/gt-reassigned+1", SeenAt: now.Add(-2 * time.Hour)},
	}
	// agate submits, the rejection reassigns the bead to mica: production reads
	// the holder as polecatOfAssignee over the bead's assignee, so the running
	// row names mica while the rejected row keeps naming agate.
	assignee := "gastown/polecats/agate"
	polecatFor := func(rig, bead string) string { return polecatOfAssignee(assignee) }
	if got := polecatFor("gastown", "gt-reassigned"); got != "agate" {
		t.Fatalf("holder before the reassignment = %q, want agate", got)
	}
	assignee = "gastown/polecats/mica"
	live := runningRows(
		[]omMerge{{At: now.Add(-10 * time.Minute), Bead: "gt-reassigned"}},
		func(string) string { return "gastown" },
		polecatFor,
	)
	rejs := []omRejection{{At: now.Add(-time.Hour), Bead: "gt-reassigned", Kind: "review"}}

	rows := buildRecentLandings(now, nil, nil, rejs, live, nil, nil, landingNameLookup(subs), nil, nil, 30)
	var rejected, running *dashboard.LandingRow
	for i := range rows {
		switch rows[i].Outcome {
		case "rejected":
			rejected = &rows[i]
		case "running":
			running = &rows[i]
		}
	}
	if running == nil || running.Polecat != "mica" {
		t.Fatalf("running row = %+v, want the bead's new holder mica", running)
	}
	if rejected == nil || rejected.Polecat != "agate" {
		t.Fatalf("rejected row = %+v, want the submitter on the last submitted branch, agate", rejected)
	}
}

// A landed row's By is unchanged: it keeps the polecat or crew member its own
// landing record's branch names, and takes no name from the submission log,
// which may name an earlier submission of the bead. A landed record with no
// branch stays a dash (gt-gclus).
func TestBuildRecentLandingsLeavesALandedRowsByAlone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 20, 30, 0, 0, time.UTC)
	subs := []land.SubmissionRecord{
		{Rig: "gastown", Bead: "gt-landed", Branch: "polecat/agate/gt-landed+1", SeenAt: now.Add(-2 * time.Hour)},
	}
	recs := []omRecord{
		{Record: landings.Record{Bead: "gt-landed", Rig: "gastown", Branch: "crew/heron/gt-landed", OMVerdict: "approve", LandedAt: now.Add(-time.Hour)}},
		{Record: landings.Record{Bead: "gt-nobranch", Rig: "gastown", OMVerdict: "approve", LandedAt: now.Add(-time.Hour)}},
	}

	rows := buildRecentLandings(now, recs, nil, nil, nil, nil, nil, landingNameLookup(subs), nil, nil, 30)
	byBead := map[string]dashboard.LandingRow{}
	for _, r := range rows {
		byBead[r.Bead] = r
	}
	if got := byBead["gt-landed"].Polecat; got != "heron" {
		t.Errorf("landed row By = %q, want heron from its own record's branch", got)
	}
	if got := byBead["gt-nobranch"].Polecat; got != "" {
		t.Errorf("landed row with no branch By = %q, want a dash", got)
	}
}

// The names the dashboard shows come from the first-sight submission log, at
// the path the landing worker writes it to. The beads are never read, so no
// reassignment can change what a row says. The om review log is not a source:
// its agent field is the model the seat ran on, which the By column must not
// show as the submitter (gt-gclus).
func TestDashSubmittersReadsTheSubmissionLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	subPath := land.SubmissionsSeenPath(town)
	if err := os.MkdirAll(filepath.Dir(subPath), 0o700); err != nil {
		t.Fatal(err)
	}
	subs := `{"seen_at":"2026-10-26T19:00:00Z","bead":"gt-rejected","rig":"gastown","branch":"crew/heron/gt-rejected","head":"1111aaaa"}
{"seen_at":"2026-10-26T19:30:00Z","bead":"gt-other","rig":"gastown","branch":"polecat/mica/gt-other+1","head":"2222bbbb"}
`
	if err := os.WriteFile(subPath, []byte(subs), 0o600); err != nil {
		t.Fatal(err)
	}
	// A review record names the model gt-agent's seat ran on. It is not a
	// submitter, so the lookup names no one for that bead.
	revFile := land.TownReviewsFile(town)
	if err := os.MkdirAll(filepath.Dir(revFile.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	revs := `{"bead":"gt-agent","rig":"gastown","attempt":1,"score":0.4,"verdict":"request_changes","agent":"claude-haiku","findings":2,"bar":0.8,"at":"2026-10-26T19:30:00Z"}
`
	if err := os.WriteFile(revFile.Path, []byte(revs), 0o600); err != nil {
		t.Fatal(err)
	}

	submitters := newDashSubmitters(town, nil)
	who := submitters.get()
	for bead, want := range map[string]string{
		"gt-rejected": "heron",
		"gt-other":    "mica",
		"gt-agent":    "",
		"gt-nobody":   "",
	} {
		if got := who("gastown", bead); got != want {
			t.Errorf("%s By = %q, want %q", bead, got, want)
		}
	}
	// The same records say which landings a crew session built: heron's branch
	// is a crew branch, mica's a polecat's, and a bead the log never saw has
	// neither (gt-kmp7c).
	crew := submitters.crewLookup()
	for bead, want := range map[string]bool{
		"gt-rejected": true,
		"gt-other":    false,
		"gt-nobody":   false,
	} {
		if got := crew("gastown", bead); got != want {
			t.Errorf("%s crew = %v, want %v", bead, got, want)
		}
	}
}

// The lookup follows the log: once a bead is resubmitted the row names the
// newest submitter, so a cached lookup that ignored the file's growth would
// keep showing the old name (gt-gclus).
func TestDashSubmittersRereadsWhenTheLogGrows(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	subPath := land.SubmissionsSeenPath(town)
	if err := os.MkdirAll(filepath.Dir(subPath), 0o700); err != nil {
		t.Fatal(err)
	}
	first := `{"seen_at":"2026-10-26T19:00:00Z","bead":"gt-rework","rig":"gastown","branch":"polecat/agate/gt-rework+1","head":"1111aaaa"}
`
	if err := os.WriteFile(subPath, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newDashSubmitters(town, nil)
	if got := s.get()("gastown", "gt-rework"); got != "agate" {
		t.Fatalf("By = %q after the first submission, want agate", got)
	}

	// The bead comes back from rework and mica resubmits it: the last submitted
	// branch is now mica's.
	second := first + `{"seen_at":"2026-10-26T20:00:00Z","bead":"gt-rework","rig":"gastown","branch":"polecat/mica/gt-rework+2","head":"3333cccc"}
`
	if err := os.WriteFile(subPath, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.get()("gastown", "gt-rework"); got != "mica" {
		t.Errorf("By = %q after the resubmission, want mica: the lookup must re-read a log that grew", got)
	}
}

// writeUnreadableSubmissionLog puts a submission log at path whose one line is
// past the reader's line buffer, so reading it fails without the file going
// missing or locked: a read error a later poll can find cured on the same
// stamp.
func writeUnreadableSubmissionLog(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	broken := strings.Repeat("x", 2<<20) + "\n"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	return broken
}

// A submission log that cannot be read is reported, not dropped, and names
// "?": a dash would claim the submitter is unknown, and an unreadable log does
// not know that (gt-gclus, gt-72xqp).
func TestDashSubmittersReportsAnUnreadableLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	subPath := land.SubmissionsSeenPath(town)
	writeUnreadableSubmissionLog(t, subPath)

	var lines []string
	submitters := newDashSubmitters(town, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if got := submitters.get()("gastown", "gt-rework"); got != "?" {
		t.Errorf("By = %q with an unreadable log, want \"?\"", got)
	}
	// The records that would say a landing was a crew's are unreadable too, so
	// the row draws a dash rather than claiming a crew built it (gt-kmp7c).
	if submitters.crewLookup()("gastown", "gt-rework") {
		t.Errorf("crew = true with an unreadable log, want false")
	}
	if len(lines) != 1 {
		t.Fatalf("reported %d line(s) %q, want one saying the read failed", len(lines), lines)
	}
	if !strings.Contains(lines[0], subPath) {
		t.Errorf("reported %q, want the log's path %s named", lines[0], subPath)
	}
}

// A log that stays unreadable is reported once, not on every poll: the page
// asks for the lookup on each publish, and the same failure again says nothing
// new. A failure that moved, a log that becomes readable, and a failure after
// a good read are all news (gt-72xqp).
func TestDashSubmittersReportsAPersistentFailureOnce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	subPath := land.SubmissionsSeenPath(town)
	writeUnreadableSubmissionLog(t, subPath)

	var lines []string
	s := newDashSubmitters(town, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	for i := 0; i < 3; i++ {
		if got := s.get()("gastown", "gt-rework"); got != "?" {
			t.Fatalf("poll %d By = %q with an unreadable log, want \"?\"", i, got)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("reported %d line(s) %q over three polls of one unreadable log, want one", len(lines), lines)
	}

	// The log grew and is still unreadable: a failure at a new stamp is news.
	if err := os.WriteFile(subPath, []byte(strings.Repeat("y", 3<<20)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.get()("gastown", "gt-rework"); got != "?" {
		t.Fatalf("By = %q with the moved log still unreadable, want \"?\"", got)
	}
	if len(lines) != 2 {
		t.Fatalf("reported %d line(s) %q, want a second for the failure at the new stamp", len(lines), lines)
	}

	good := `{"seen_at":"2026-10-26T19:00:00Z","bead":"gt-rework","rig":"gastown","branch":"polecat/agate/gt-rework+1","head":"1111aaaa"}`
	if err := os.WriteFile(subPath, []byte(good+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.get()("gastown", "gt-rework"); got != "agate" {
		t.Fatalf("By = %q once the log is readable, want agate", got)
	}
	if len(lines) != 2 {
		t.Fatalf("reported %d line(s) %q, want none for a read that succeeded", len(lines), lines)
	}

	// Broken again after a good read: that failure is reported too, never
	// swallowed as the one before it.
	if err := os.WriteFile(subPath, []byte(strings.Repeat("z", 2<<20)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.get()("gastown", "gt-rework"); got != "?" {
		t.Fatalf("By = %q with the log unreadable again, want \"?\"", got)
	}
	if len(lines) != 3 {
		t.Fatalf("reported %d line(s) %q, want the failure after a good read reported again", len(lines), lines)
	}
}

// A read that failed is not remembered: the next poll reads the log again even
// though the file did not move, so a log that becomes readable on the same
// stamp is picked up instead of reading "?" until something writes it
// (gt-gclus).
func TestDashSubmittersRetriesAFailedRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	subPath := land.SubmissionsSeenPath(town)
	broken := writeUnreadableSubmissionLog(t, subPath)
	// Pin the mtime, and cure the log in place below with the broken one's
	// size: its stamp then does not move, so only a read that was never cached
	// can name the submitter.
	pinned := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(subPath, pinned, pinned); err != nil {
		t.Fatal(err)
	}

	s := newDashSubmitters(town, nil)
	if got := s.get()("gastown", "gt-rework"); got != "?" {
		t.Fatalf("By = %q with an unreadable log, want \"?\"", got)
	}

	good := `{"seen_at":"2026-10-26T19:00:00Z","bead":"gt-rework","rig":"gastown","branch":"polecat/agate/gt-rework+1","head":"1111aaaa"}`
	// The readable log is the record on one line and blank lines after it: the
	// reader skips a blank line, so the size matches the broken log's while
	// every line is short enough to read.
	cured := good + "\n" + strings.Repeat("\n", len(broken)-len(good)-1)
	if err := os.WriteFile(subPath, []byte(cured), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(subPath, pinned, pinned); err != nil {
		t.Fatal(err)
	}

	if got := s.get()("gastown", "gt-rework"); got != "agate" {
		t.Errorf("By = %q once the log is readable, want agate: a failed read must not be cached", got)
	}
}

// TestRunningLandingsLifecycle drives the daemon.log merge bookkeeping through
// the four states a bead's landing can be in: merged and still running, landed
// (no longer running), rejected (no longer running), and merged before a
// daemon restart (a ghost: the restart killed the pass, so nothing is running).
func TestRunningLandingsLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 19, 40, 0, 0, time.Local)
	since := now.Add(-trendHours * time.Hour)
	r := &omReader{}
	for _, line := range []string{
		"2026/10/04 19:36:21 landing_worker: [land] gt-live: merged adbe8221 onto origin/main (2c1888b5) as c3b7fe9c; gating the merged tree, then om review",
		"2026/10/04 19:30:27 landing_worker: [land] gt-done: merged 408a979e onto origin/main (ba00cbb7) as b11b439c; gating the merged tree, then om review",
		"2026/10/04 19:30:29 landing_worker: [land] gt-done: landed b11b439c on origin/main (patch-id 4b174513)",
		"2026/10/04 19:24:00 landing_worker: [land] gt-rej: merged 66ce5867 onto origin/main (c0e6fd98) as 7dc230af; pushing the merge candidate",
		"2026/10/04 19:25:00 landing_worker: [land] gt-rej: rejected (review): om requested changes",
		// Two merges for one bead: a requeued landing re-merges, and only its
		// newest merge is the one in flight.
		"2026/10/04 19:20:00 landing_worker: [land] gt-retry: merged 1111aaaa onto origin/main (ba00cbb7) as 2222bbbb; gating the merged tree",
		"2026/10/04 19:28:00 landing_worker: [land] gt-retry: rejected (gate): gate failed on the merged tree",
		"2026/10/04 19:33:00 landing_worker: [land] gt-retry: merged 1111aaaa onto origin/main (ba00cbb7) as 3333cccc; gating the merged tree",
	} {
		r.parseLogLine(line)
	}
	got := r.running(since)
	if len(got) != 2 {
		t.Fatalf("running = %+v, want the live merge and the requeued one", got)
	}
	if got[0].Bead != "gt-live" || got[1].Bead != "gt-retry" {
		t.Errorf("running beads = %s, %s; want gt-live then gt-retry (newest merge first)", got[0].Bead, got[1].Bead)
	}
	if want := time.Date(2026, 10, 4, 19, 36, 21, 0, time.Local); !got[0].At.Equal(want) {
		t.Errorf("gt-live merge at %v, want %v", got[0].At, want)
	}

	// A restart closes every merge in flight: the pass is gone with the process.
	r.parseLogLine("2026/10/04 19:38:00 Daemon starting (PID 4242)")
	if got := r.running(since); len(got) != 0 {
		t.Errorf("running after a daemon start = %+v, want none", got)
	}
	// A landing merged after the start is live again.
	r.parseLogLine("2026/10/04 19:39:00 landing_worker: [land] gt-next: merged 4444dddd onto origin/main (ba00cbb7) as 5555eeee; gating the merged tree")
	if got := r.running(since); len(got) != 1 || got[0].Bead != "gt-next" {
		t.Errorf("running after a fresh merge = %+v, want gt-next", got)
	}
}

// TestRunningLandingsDropOutOfWindow keeps a merge the log still holds but the
// table's window has passed out of the running set, so an old ghost cannot be
// revived by a merge line that aged out of the scan.
func TestRunningLandingsDropOutOfWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 19, 40, 0, 0, time.Local)
	r := &omReader{}
	r.parseLogLine("2026/10/03 09:00:00 landing_worker: [land] gt-old: merged 1111aaaa onto origin/main (2222bbbb) as 3333cccc; gating the merged tree")
	if got := r.running(now.Add(-trendHours * time.Hour)); len(got) != 0 {
		t.Errorf("running = %+v, want none: the merge is a day old", got)
	}
}

// TestBuildRecentLandingsPutsRunningRowsOnTopUncapped is the table's contract
// for a live landing: it is a row above every finished one, and the cap counts
// finished landings only, so a busy day cannot hide a landing in flight. The
// cap keeps the newest finished rows; the limit here is the test's own, and
// what production passes is pinned by
// TestBuildRecentLandingsBoundsTheTableAtTheProductionCap (gt-1j1bf).
func TestBuildRecentLandingsPutsRunningRowsOnTopUncapped(t *testing.T) {
	const capRows = 10
	t.Parallel()
	now := time.Date(2026, 10, 4, 19, 40, 0, 0, time.UTC)
	var recs []omRecord
	for i := 0; i < 40; i++ {
		recs = append(recs, omTestRec(fmt.Sprintf("gt-%d", i), "approve", 0.9, "daemon", now.Add(-time.Duration(i+1)*time.Minute)))
	}
	live := []dashboard.LandingRow{{At: now.Add(-time.Minute), Bead: "gt-live", Rig: "gastown", Outcome: "running"}}
	rows := buildRecentLandings(now, recs, nil, nil, live, nil, nil, nil, nil, nil, capRows)

	if len(rows) != capRows+1 {
		t.Fatalf("rows = %d, want %d: the live row is not one of the capped finished rows", len(rows), capRows+1)
	}
	if rows[0].Bead != "gt-live" || rows[0].Outcome != "running" {
		t.Errorf("row 0 = %+v, want the running landing on top", rows[0])
	}
	for i, r := range rows[1:] {
		if r.Outcome != "landed" {
			t.Fatalf("row %d = %+v, want a finished landing below the live one", i+1, r)
		}
	}
	if rows[1].Bead != "gt-0" || rows[capRows].Bead != fmt.Sprintf("gt-%d", capRows-1) {
		t.Errorf("finished rows run %s..%s, want the newest cap (gt-0..gt-%d)", rows[1].Bead, rows[capRows].Bead, capRows-1)
	}
}

// TestBuildRecentLandingsBoundsTheTableAtTheProductionCap pins the row bound
// production passes: a non-positive value would bound nothing, and a day with
// more rows than the cap keeps the newest ones rather than growing the table
// with the log (gt-1j1bf).
func TestBuildRecentLandingsBoundsTheTableAtTheProductionCap(t *testing.T) {
	t.Parallel()
	if recentLandingRows <= 0 {
		t.Fatalf("recentLandingRows = %d: a non-positive cap bounds nothing", recentLandingRows)
	}
	now := time.Date(2026, 10, 4, 19, 40, 0, 0, time.UTC)
	const over = 5
	recs := make([]omRecord, 0, recentLandingRows+over)
	for i := range recentLandingRows + over {
		recs = append(recs, omTestRec(fmt.Sprintf("gt-%d", i), "approve", 0.9, "daemon", now.Add(-time.Duration(i)*time.Second)))
	}
	rows := buildRecentLandings(now, recs, nil, nil, nil, nil, nil, nil, nil, nil, recentLandingRows)
	if len(rows) != recentLandingRows {
		t.Fatalf("rows = %d, want %d: the table keeps its bound", len(rows), recentLandingRows)
	}
	last := recentLandingRows - 1
	if rows[0].Bead != "gt-0" || rows[last].Bead != fmt.Sprintf("gt-%d", last) {
		t.Errorf("rows run %s..%s, want the newest %d (gt-0..gt-%d)", rows[0].Bead, rows[last].Bead, recentLandingRows, last)
	}
}

// TestRunningRowsCarryRigAndPolecat checks the two facts a live row has no
// landing record to read: the rig its bead's prefix routes to, and the polecat
// holding the bead.
func TestRunningRowsCarryRigAndPolecat(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 4, 19, 36, 21, 0, time.Local)
	rows := runningRows([]omMerge{{At: at, Bead: "ma-7js"}},
		func(bead string) string { return map[string]string{"ma-7js": "mango"}[bead] },
		func(rig, bead string) string {
			if rig != "mango" || bead != "ma-7js" {
				t.Errorf("polecat lookup for %s/%s, want the rig the row resolved", rig, bead)
			}
			return "opal"
		})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	if r := rows[0]; r.Bead != "ma-7js" || r.Rig != "mango" || r.Polecat != "opal" || r.Outcome != "running" || !r.At.Equal(at) {
		t.Errorf("live row = %+v, want mango/opal running at %v", r, at)
	}
}

// TestLandingRigFollowsTheRoutesFile covers every rig with a landing worker,
// not just the one that logs the most: the rig comes from the town's routes, so
// a bead of any rig resolves, and one no route claims stays blank.
func TestLandingRigFollowsTheRoutesFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"gt-","path":"gastown/mayor/rig"}
{"prefix":"ma-","path":"mango/mayor/rig"}
`
	if err := os.WriteFile(filepath.Join(beadsDir, beads.RoutesFileName), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	rigFor := landingRig(townRoot)
	for bead, want := range map[string]string{"gt-xeiwt": "gastown", "ma-7js": "mango", "zz-1": ""} {
		if got := rigFor(bead); got != want {
			t.Errorf("landingRig(%q) = %q, want %q", bead, got, want)
		}
	}
}

func TestPolecatOfAssignee(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"gastown/polecats/mica": "mica", "gastown/crew/sloan": "", "": "", "mica": "", "gastown/polecats/": "",
	} {
		if got := polecatOfAssignee(in); got != want {
			t.Errorf("polecatOfAssignee(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDashLoadsTrimsTheFilePeriodically: a long-lived dashboard samples the
// host about every 10s, so the file must not grow with every sample ever
// taken. Past a bounded number of appends the writer rewrites it with just
// the kept window, which is the only thing that removes a sample the window
// has rolled past (gt-2czgm).
func TestDashLoadsTrimsTheFilePeriodically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	cur := start
	l := newDashLoads(dir, func() time.Time { return cur })
	path := filepath.Join(dir, ".runtime", "dashboard-load.jsonl")

	// Three samples land inside the window.
	stale := []time.Time{start, start.Add(-time.Hour), start.Add(-2 * time.Hour)}
	for i, at := range stale {
		l.append(at, float64(i)+1)
	}

	// Two days pass: those samples are now outside the 24h window, so only a
	// rewrite can clear them from the file.
	cur = start.Add(48 * time.Hour)
	for i := 0; i < loadRewriteEvery; i++ {
		l.append(cur.Add(time.Duration(i)*time.Second), 9.5)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range stale {
		if strings.Contains(string(body), at.Format(time.RFC3339)) {
			t.Errorf("the file still holds the trimmed sample %s; a periodic rewrite must drop it", at.Format(time.RFC3339))
		}
	}
	// The rewrite is a temp file renamed into place, so nothing is left over.
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".runtime", "dashboard-load-*.jsonl")); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode = %v, want 0644", fi.Mode().Perm())
	}
}

// TestBuildRecentLandingsCarriesThePolecatTime covers the column's row data: a
// landing's record carries the time its bead's polecat seats spent hooked on
// it, with the attempts that make it up and the bound the record marks, lower
// or — for a time read from the bead's dispatch — upper; a record with no time
// of its own (a crew landing, or one written before the reading existed)
// carries none, so the page draws a dash (gt-90wi1, gt-v7ptp).
func TestBuildRecentLandingsCarriesThePolecatTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	recs := []omRecord{
		{Record: landings.Record{
			Bead: "gt-timed", Rig: "gastown", Branch: "polecat/opal/gt-timed+x", OMVerdict: "approve", LandedAt: now.Add(-time.Minute),
			PolecatSecs: 1380, PolecatLowerBound: true,
			PolecatAttempts: []land.PolecatAttempt{{Seat: "gastown/opal", Secs: 840}, {Seat: "gastown/jade", Secs: 540}},
		}},
		{Record: landings.Record{
			Bead: "gt-upper", Rig: "gastown", Branch: "polecat/morsov/gt-upper+z", OMVerdict: "approve", LandedAt: now.Add(-time.Minute),
			PolecatSecs: 1200, PolecatUpperBound: true,
			PolecatAttempts: []land.PolecatAttempt{{Seat: "gastown/morsov", Secs: 1200, UpperBound: true, From: land.PolecatFromSling}},
		}},
		{Record: landings.Record{
			Bead: "gt-open", Rig: "gastown", Branch: "polecat/onyx/gt-open+y", OMVerdict: "approve", LandedAt: now.Add(-2 * time.Minute),
			PolecatLowerBound: true,
			PolecatAttempts:   []land.PolecatAttempt{{Seat: "gastown/onyx", LowerBound: true}},
		}},
		{Record: landings.Record{
			Bead: "gt-crew", Rig: "gastown", Branch: "sloan/dashboard", OMVerdict: "skipped", LandedAt: now.Add(-3 * time.Minute),
		}},
	}

	rows := buildRecentLandings(now, recs, nil, nil, nil, nil, nil, nil, nil, nil, 30)
	byBead := map[string]dashboard.LandingRow{}
	for _, r := range rows {
		byBead[r.Bead] = r
	}

	if r := byBead["gt-timed"]; r.PolecatSecs == nil || *r.PolecatSecs != 1380 || !r.PolecatLowerBound {
		t.Errorf("timed row polecat time = %v lower bound %v, want 1380 and a lower bound", r.PolecatSecs, r.PolecatLowerBound)
	}
	if got, want := byBead["gt-timed"].PolecatAttempts, []dashboard.PolecatAttempt{{Seat: "gastown/opal", Secs: 840}, {Seat: "gastown/jade", Secs: 540}}; !reflect.DeepEqual(got, want) {
		t.Errorf("timed row attempts = %+v, want %+v", got, want)
	}
	// A time read from the bead's dispatch reaches the row marked an upper
	// bound, not downgraded to a floor or dropped (gt-v7ptp).
	if r := byBead["gt-upper"]; r.PolecatSecs == nil || *r.PolecatSecs != 1200 || !r.PolecatUpperBound || r.PolecatLowerBound {
		t.Errorf("upper row polecat time = %v upper %v lower %v, want 1200 and an upper bound", r.PolecatSecs, r.PolecatUpperBound, r.PolecatLowerBound)
	}
	if got, want := byBead["gt-upper"].PolecatAttempts, []dashboard.PolecatAttempt{{Seat: "gastown/morsov", Secs: 1200, UpperBound: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("upper row attempts = %+v, want %+v", got, want)
	}
	// An open attempt adds no time to the record: the column draws a dash and
	// the hover still lists the attempt, marked a floor as the record has it.
	if r := byBead["gt-open"]; r.PolecatSecs != nil || !r.PolecatLowerBound || len(r.PolecatAttempts) != 1 || !r.PolecatAttempts[0].LowerBound {
		t.Errorf("open-attempt row = %+v, want no time and one lower-bound attempt", r)
	}
	if r := byBead["gt-crew"]; r.PolecatSecs != nil || r.PolecatLowerBound || r.PolecatAttempts != nil {
		t.Errorf("crew row carries a polecat time: %+v", r)
	}
}

// A rejected row carries the polecat time its bead's spans log reads the way a
// landed record reads it, and a bead with no reading keeps a dash. The reader
// answers a running row's question with a different total, so a rejected row
// that took that reading would show it instead (gt-kmp7c).
func TestBuildRecentLandingsReadsARejectedRowsPolecatTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	rejs := []omRejection{
		{At: now.Add(-10 * time.Minute), Bead: "gt-rej", Kind: "review"},
		{At: now.Add(-20 * time.Minute), Bead: "gt-none", Kind: "gate"},
	}
	read := func(bead string, asOf time.Time) (float64, []land.PolecatAttempt, bool) {
		if bead != "gt-rej" {
			return 0, nil, false
		}
		if !asOf.IsZero() {
			// What a landing still running would read: the time so far, a floor.
			return 1500, []land.PolecatAttempt{{Seat: "gastown/opal", Secs: 1500, LowerBound: true}}, true
		}
		return 1380, []land.PolecatAttempt{{Seat: "gastown/opal", Secs: 1380}}, false
	}

	rows := buildRecentLandings(now, nil, nil, rejs, nil, nil, nil, nil, nil, read, 30)
	byBead := map[string]dashboard.LandingRow{}
	for _, r := range rows {
		byBead[r.Bead] = r
	}
	if r := byBead["gt-rej"]; r.PolecatSecs == nil || *r.PolecatSecs != 1380 || r.PolecatLowerBound || len(r.PolecatAttempts) != 1 || r.PolecatAttempts[0].Seat != "gastown/opal" {
		t.Errorf("rejected row polecat time = %+v, want the 1380s reading and its seat, not the 1500s one a running row reads", r)
	}
	if r := byBead["gt-none"]; r.PolecatSecs != nil || r.PolecatAttempts != nil {
		t.Errorf("rejected row with no reading = %+v, want no polecat time", r)
	}
}

// A running row carries the time its bead's seats have spent so far, with the
// lower-bound mark: the landing has not ended, so the total is a floor. The
// reader counts a hooked seat from its start to the as-of time, so the row's
// total says which time the row was read at (gt-kmp7c).
func TestBuildRecentLandingsReadsARunningRowsTimeSoFar(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	started := now.Add(-23 * time.Minute)
	live := []dashboard.LandingRow{{At: now.Add(-time.Minute), Bead: "gt-run", Rig: "gastown", Outcome: "running"}}
	read := func(bead string, asOf time.Time) (float64, []land.PolecatAttempt, bool) {
		if asOf.IsZero() {
			// The landed reading: an attempt still open adds no time.
			return 0, []land.PolecatAttempt{{Seat: "gastown/onyx", LowerBound: true}}, true
		}
		secs := asOf.Sub(started).Seconds()
		return secs, []land.PolecatAttempt{{Seat: "gastown/onyx", Secs: secs, LowerBound: true}}, true
	}

	rows := buildRecentLandings(now, nil, nil, nil, live, nil, nil, nil, nil, read, 30)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the one running row", rows)
	}
	if r := rows[0]; r.PolecatSecs == nil || *r.PolecatSecs != 1380 || !r.PolecatLowerBound {
		t.Errorf("running row polecat time = %v lower bound %v, want 1380s (its seat's time up to now) and the lower-bound mark", r.PolecatSecs, r.PolecatLowerBound)
	}
	if len(rows[0].PolecatAttempts) != 1 || rows[0].PolecatAttempts[0].Secs != 1380 {
		t.Errorf("running row attempts = %+v, want the seat's 1380s so far", rows[0].PolecatAttempts)
	}
}

// A landing built on a crew session's branch is marked crew, and a polecat's
// landing is not: the page reads crew in place of a dash for the first (gt-kmp7c).
func TestBuildRecentLandingsMarksACrewBranchRow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	recs := []omRecord{
		{Record: landings.Record{Bead: "gt-crew", Rig: "gastown", Branch: "crew/sloan/gt-crew", OMVerdict: "approve", LandedAt: now.Add(-time.Minute)}},
		{Record: landings.Record{Bead: "gt-pc", Rig: "gastown", Branch: "polecat/opal/gt-pc+x", OMVerdict: "approve", LandedAt: now.Add(-2 * time.Minute)}},
	}

	rows := buildRecentLandings(now, recs, nil, nil, nil, nil, nil, nil, nil, nil, 30)
	byBead := map[string]dashboard.LandingRow{}
	for _, r := range rows {
		byBead[r.Bead] = r
	}
	if !byBead["gt-crew"].Crew {
		t.Errorf("crew branch row is not marked crew: %+v", byBead["gt-crew"])
	}
	if byBead["gt-pc"].Crew {
		t.Errorf("polecat branch row is marked crew: %+v", byBead["gt-pc"])
	}
}

// spanTestClock is the spans store's clock, pinned so that the logs these tests
// write carry the times they name rather than the wall clock.
type spanTestClock struct{ at time.Time }

func (c spanTestClock) Now() time.Time { return c.at }

// The polecat-time reader the dashboard builds reads the spans log of the town
// it was handed — the landing worker's own store — so a rejected row and a
// landed record read one source. A seat still hooked adds no time to a landed
// reading, and its time so far once the row is read as of now (gt-kmp7c).
func TestSpanPolecatTimesReadsTheTownsSpansLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	start := time.Date(2026, 10, 26, 13, 50, 0, 0, time.UTC)
	store := spans.New(spans.DefaultDir(town), spanTestClock{at: start})
	if err := store.Start("gt-hooked", "gastown/opal"); err != nil {
		t.Fatal(err)
	}
	if err := store.Start("gt-done", "gastown/onyx"); err != nil {
		t.Fatal(err)
	}
	if err := store.End("gt-done", "gastown/onyx", spans.ReasonDone, start.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}

	read := spanPolecatTimes(town)

	secs, attempts, lower := read("gt-done", time.Time{})
	if secs != 660 || len(attempts) != 1 || attempts[0].Seat != "gastown/onyx" || lower {
		t.Errorf("landed bead read = %v secs %+v lower %v, want 660s from gastown/onyx", secs, attempts, lower)
	}
	if secs, attempts, lower := read("gt-hooked", time.Time{}); secs != 0 || len(attempts) != 1 || attempts[0].Secs != 0 || !lower {
		t.Errorf("hooked bead's landed reading = %v secs %+v lower %v, want no time and a lower bound", secs, attempts, lower)
	}
	secs, attempts, lower = read("gt-hooked", start.Add(10*time.Minute))
	if secs != 600 || len(attempts) != 1 || attempts[0].Secs != 600 || !lower {
		t.Errorf("hooked bead's time so far = %v secs %+v lower %v, want 600s and a lower bound", secs, attempts, lower)
	}
	if secs, attempts, _ := read("gt-absent", start); secs != 0 || attempts != nil {
		t.Errorf("bead with no log = %v secs %+v, want nothing", secs, attempts)
	}
}

// The reader keeps a bead's summary only while its log is unchanged: a seat
// still hooked ends its attempt, and the next read shows the closed time rather
// than the reading the earlier poll kept (gt-kmp7c).
func TestSpanPolecatTimesRereadsALogThatMoved(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	start := time.Date(2026, 10, 26, 13, 50, 0, 0, time.UTC)
	store := spans.New(spans.DefaultDir(town), spanTestClock{at: start})
	if err := store.Start("gt-run", "gastown/opal"); err != nil {
		t.Fatal(err)
	}

	read := spanPolecatTimes(town)
	if secs, _, _ := read("gt-run", start.Add(10*time.Minute)); secs != 600 {
		t.Fatalf("time so far = %v secs, want 600", secs)
	}

	// The seat ends the attempt: the log grows, so the next read is a new one.
	if err := store.End("gt-run", "gastown/opal", spans.ReasonDone, start.Add(12*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if secs, attempts, lower := read("gt-run", start.Add(20*time.Minute)); secs != 720 || lower || len(attempts) != 1 || attempts[0].Secs != 720 {
		t.Errorf("closed reading = %v secs %+v lower %v, want the 720s attempt and no lower bound", secs, attempts, lower)
	}
}

// A poll answers from the last read while the bead's log has not moved: the
// Landings table asks for every rejected row's time on each publish, and a
// rejected bead's log never changes again, so going back to the disk each time
// would spend it on an answer that cannot have changed. The log here keeps its
// size and time and names a different seat, which only a fresh read would show
// (gt-kmp7c).
func TestSpanPolecatTimesAnswersAnUnchangedLogFromTheLastRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	start := time.Date(2026, 10, 26, 13, 50, 0, 0, time.UTC)
	store := spans.New(spans.DefaultDir(town), spanTestClock{at: start})
	if err := store.Start("gt-a", "gastown/opal"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spans.DefaultDir(town), "gt-a.jsonl")
	pinned := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, pinned, pinned); err != nil {
		t.Fatal(err)
	}

	read := spanPolecatTimes(town)
	if _, attempts, _ := read("gt-a", start.Add(5*time.Minute)); len(attempts) != 1 || attempts[0].Seat != "gastown/opal" {
		t.Fatalf("first read seats = %+v, want gastown/opal", attempts)
	}

	log, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(log), "gastown/opal", "gastown/onyx", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	if _, attempts, _ := read("gt-a", start.Add(5*time.Minute)); len(attempts) != 1 || attempts[0].Seat != "gastown/opal" {
		t.Errorf("second read seats = %+v, want the kept gastown/opal: a log whose size and time are unchanged is answered from the last read", attempts)
	}
}

// A log the store cannot read leaves the bead's time unknown rather than zero:
// the reader marks the total a lower bound, as the landing record does for the
// same log, so the row shows a floor instead of a dash that claims no seat ever
// worked the bead (gt-kmp7c).
func TestSpanPolecatTimesReportsAnUnreadableLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where the bead's log belongs: present, so the stamp reads, but
	// not the regular file the store reads and errors on.
	if err := os.MkdirAll(filepath.Join(spans.DefaultDir(town), "gt-bad.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	read := spanPolecatTimes(town)
	if secs, attempts, lower := read("gt-bad", time.Now()); secs != 0 || attempts != nil || !lower {
		t.Errorf("unreadable log read = %v secs %+v lower %v, want no time and a lower bound", secs, attempts, lower)
	}
}

// A row with no record of its own — one the worker is still landing, and one it
// rejected — takes crew from the submission lookup: its branch is not in the
// landings file yet, so without the lookup the Polecat cell would draw a dash
// for a crew build rather than saying crew (gt-kmp7c).
func TestBuildRecentLandingsMarksARejectedOrRunningCrewRow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	live := []dashboard.LandingRow{
		{At: now.Add(-time.Minute), Bead: "gt-running", Rig: "gastown", Outcome: "running"},
		{At: now.Add(-2 * time.Minute), Bead: "gt-running-pc", Rig: "gastown", Outcome: "running"},
	}
	rejs := []omRejection{{At: now.Add(-3 * time.Minute), Bead: "gt-rejected", Kind: "review"}}
	crew := func(rig, bead string) bool {
		return rig == "gastown" && (bead == "gt-running" || bead == "gt-rejected")
	}

	rows := buildRecentLandings(now, nil, nil, rejs, live, nil, nil, nil, crew, nil, 30)
	byBead := map[string]dashboard.LandingRow{}
	for _, r := range rows {
		byBead[r.Bead] = r
	}
	for bead, want := range map[string]bool{"gt-running": true, "gt-rejected": true, "gt-running-pc": false} {
		if got := byBead[bead].Crew; got != want {
			t.Errorf("%s Crew = %v, want %v", bead, got, want)
		}
	}
}

// A running landing whose bead is held by a crew member names the submitter,
// not the route: the By cell read the rig's name for gt-3hcp9 while it was
// landing (the bug this pins).
func TestLivePolecatNameNamesTheSubmitterWhenNoPolecatHoldsTheBead(t *testing.T) {
	t.Parallel()
	holder := func(rig, bead string) string { return "" }
	submitter := func(rig, bead string) string { return "overseer" }
	got := livePolecatName(holder, submitter)("gastown", "gt-3hcp9")
	if got != "overseer" {
		t.Errorf("By = %q, want the submitter overseer", got)
	}
}

func TestLivePolecatNamePrefersThePolecatHoldingTheBead(t *testing.T) {
	t.Parallel()
	holder := func(rig, bead string) string { return "bullet" }
	submitter := func(rig, bead string) string { return "overseer" }
	if got := livePolecatName(holder, submitter)("gastown", "gt-dv04y"); got != "bullet" {
		t.Errorf("By = %q, want the polecat bullet", got)
	}
}

func TestLivePolecatNameIsEmptyWhenNoOneNamesTheBead(t *testing.T) {
	t.Parallel()
	none := func(rig, bead string) string { return "" }
	if got := livePolecatName(none, none)("gastown", "gt-x"); got != "" {
		t.Errorf("By = %q, want empty", got)
	}
}
