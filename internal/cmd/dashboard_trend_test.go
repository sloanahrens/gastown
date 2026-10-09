package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/landings"
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
	rows := buildRecentLandings(now, recs, stages, rejs, nil, nil, func(rig, id string) string { asked = append(asked, rig+"/"+id); return "title of " + id }, 30)

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

func TestBuildRecentLandingsCapsRowsAndAsksTitlesOnlyForThoseShown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	var recs []omRecord
	for i := 0; i < 50; i++ {
		recs = append(recs, omRecord{Record: landings.Record{Bead: fmt.Sprintf("gt-%d", i), Rig: "gastown", OMVerdict: "approve", LandedAt: now.Add(-time.Duration(i) * time.Minute)}})
	}
	asked := 0
	rows := buildRecentLandings(now, recs, nil, nil, nil, nil, func(rig, id string) string { asked++; return "" }, recentLandingRows)
	if len(rows) != recentLandingRows || asked != recentLandingRows {
		t.Fatalf("rows=%d title lookups=%d, want %d and %d: a title read costs a bd call, so it is bounded by the rows shown", len(rows), asked, recentLandingRows, recentLandingRows)
	}
	if rows[0].Bead != "gt-0" || rows[recentLandingRows-1].Bead != fmt.Sprintf("gt-%d", recentLandingRows-1) {
		t.Errorf("not the newest %d: first %s last %s", recentLandingRows, rows[0].Bead, rows[recentLandingRows-1].Bead)
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

	rows := buildRecentLandings(now, recs, nil, rejs, nil, track.shipStatus, func(string, string) string { return "" }, 30)
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

	rows := buildRecentLandings(now, recs, nil, nil, nil, track.shipStatus, func(string, string) string { return "" }, 30)
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
	} {
		if got := polecatOfBranch(in); got != want {
			t.Errorf("polecatOfBranch(%q) = %q, want %q", in, got, want)
		}
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
// for a live landing: it is a row above every finished one, and the
// recentLandingRows cap counts finished landings only, so a busy day cannot
// hide a landing in flight. The cap keeps the newest finished rows.
func TestBuildRecentLandingsPutsRunningRowsOnTopUncapped(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 19, 40, 0, 0, time.UTC)
	var recs []omRecord
	for i := 0; i < 40; i++ {
		recs = append(recs, omTestRec(fmt.Sprintf("gt-%d", i), "approve", 0.9, "daemon", now.Add(-time.Duration(i+1)*time.Minute)))
	}
	live := []dashboard.LandingRow{{At: now.Add(-time.Minute), Bead: "gt-live", Rig: "gastown", Outcome: "running"}}
	rows := buildRecentLandings(now, recs, nil, nil, live, nil, nil, recentLandingRows)

	if len(rows) != recentLandingRows+1 {
		t.Fatalf("rows = %d, want %d: the live row is not one of the capped finished rows", len(rows), recentLandingRows+1)
	}
	if rows[0].Bead != "gt-live" || rows[0].Outcome != "running" {
		t.Errorf("row 0 = %+v, want the running landing on top", rows[0])
	}
	for i, r := range rows[1:] {
		if r.Outcome != "landed" {
			t.Fatalf("row %d = %+v, want a finished landing below the live one", i+1, r)
		}
	}
	if rows[1].Bead != "gt-0" || rows[recentLandingRows].Bead != fmt.Sprintf("gt-%d", recentLandingRows-1) {
		t.Errorf("finished rows run %s..%s, want the newest cap (gt-0..gt-%d)", rows[1].Bead, rows[recentLandingRows].Bead, recentLandingRows-1)
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
