package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/landings"
)

func omTestRec(bead, verdict string, score float64, route string, at time.Time) omRecord {
	return omRecord{Record: landings.Record{Bead: bead, Rig: "gastown", OMVerdict: verdict, OMScore: score, Route: route, LandedAt: at}}
}

func TestOMStageDuration(t *testing.T) {
	t.Parallel()
	d := omStageDuration("lint 14s, gate 34s, om 1m56s")
	if d == nil || *d != 116*time.Second {
		t.Fatalf("om duration = %v", d)
	}
	if omStageDuration("lint 14s, gate 34s (timed out)") != nil {
		t.Error("a stages line with no om stage must have no om time")
	}
	if d := omStageDuration("lint 1s, om 90s (timed out)"); d == nil || *d != 90*time.Second {
		t.Errorf("timed-out om stage = %v", d)
	}
}

func TestOMStageTimes(t *testing.T) {
	t.Parallel()
	is := func(d *time.Duration, want time.Duration) bool { return d != nil && *d == want }

	// A cut-over rig's candidate gate runs in CI: the ci stage IS the gate.
	_, gate, om := omStageTimes("ci 2m46s, om 21s")
	if !is(gate, 2*time.Minute+46*time.Second) {
		t.Errorf("ci stage gate = %v", gate)
	}
	if !is(om, 21*time.Second) {
		t.Errorf("ci stage om = %v", om)
	}

	// An old gate-only line is unchanged.
	lint, gate, om := omStageTimes("lint 16s, gate 36s, om 1m56s")
	if !is(lint, 16*time.Second) || !is(gate, 36*time.Second) || !is(om, 116*time.Second) {
		t.Errorf("gate-only line = %v %v %v", lint, gate, om)
	}

	// A line that names both prefers the explicit gate, in either order.
	if _, gate, _ := omStageTimes("gate 5s, ci 9s"); !is(gate, 5*time.Second) {
		t.Errorf("gate before ci = %v", gate)
	}
	if _, gate, _ := omStageTimes("ci 9s, gate 5s"); !is(gate, 5*time.Second) {
		t.Errorf("ci before gate = %v", gate)
	}

	// An unknown stage name is still ignored.
	if l, g, o := omStageTimes("deploy 3s"); l != nil || g != nil || o != nil {
		t.Errorf("unknown stage = %v %v %v", l, g, o)
	}
}

func TestOMParseLogLine(t *testing.T) {
	t.Parallel()
	r := &omReader{}
	r.parseLogLine("2026/10/03 16:41:42 landing_worker: [land] gt-y3pgh.2.13.1: stages: lint 16s, gate 36s, om 1m56s")
	r.parseLogLine("2026/10/03 12:24:29 landing_worker: [land] gt-6u1qd: rejected (review): om requested changes (score 0.55, 5 finding(s))")
	r.parseLogLine("2026/10/03 12:24:29 landing_worker: gastown: gt-6u1qd: landing rejected (review): om requested changes (score 0.55, 5 finding(s))")
	r.parseLogLine("2026/10/03 11:36:36 landing_worker: [land] gt-ik4a1.2: rejected (conflict): merging x into main conflicts")
	if len(r.stages) != 1 || r.stages[0].Bead != "gt-y3pgh.2.13.1" {
		t.Fatalf("stages = %+v", r.stages)
	}
	if len(r.rejects) != 2 {
		t.Fatalf("rejects = %+v (the second wording of the same rejection must not double-count)", r.rejects)
	}
	if rj := r.rejects[0]; rj.Kind != "review" || rj.Score == nil || *rj.Score != 0.55 {
		t.Errorf("review rejection = %+v", rj)
	}
	if r.rejects[1].Kind != "conflict" || r.rejects[1].Score != nil {
		t.Errorf("conflict rejection = %+v", r.rejects[1])
	}
}

// A Forgejo-path landing logs its candidate gate as a ci stage; the gate time
// it reads must reach the Landings row and the trend series like a gate stage.
func TestCIPathLandingCarriesItsGateTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 18, 0, 0, 0, time.Local)
	r := &omReader{}
	r.parseLogLine("2026/10/04 17:58:00 landing_worker: [land] ma-7js: stages: ci 2m46s, om 21s")
	if len(r.stages) != 1 || r.stages[0].Gate == nil || *r.stages[0].Gate != 2*time.Minute+46*time.Second {
		t.Fatalf("ci stage = %+v, want gate 2m46s", r.stages)
	}

	stages := r.stages
	recs := []omRecord{{Record: landings.Record{Bead: "ma-7js", Rig: "mango", Branch: "polecat/x/ma-7js+y",
		OMVerdict: "approve", OMScore: 0.9, Route: "daemon", LandedAt: now.Add(-time.Minute)}}}

	rows := buildRecentLandings(now, recs, stages, nil, nil, nil, 30)
	if len(rows) != 1 || rows[0].GateSecs == nil || *rows[0].GateSecs != 166 {
		t.Fatalf("landing row = %+v, want gate 166s", rows)
	}
	tr := buildTrend(now, nil, stages, nil, nil)
	if len(tr.Stages) != 1 || tr.Stages[0].GateSecs != 166 {
		t.Errorf("trend gate = %+v, want 166s", tr.Stages)
	}
}

func TestScanLogIsIncrementalAndSurvivesRotation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	line := "2026/10/03 16:41:42 landing_worker: [land] gt-a: stages: lint 1s, om 10s\n"
	if err := os.WriteFile(path, []byte(line+"2026/10/03 16:42:00 landing_worker: [land] gt-b: stages: lint 1s, om 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &omReader{logPath: path}
	if err := r.scanLog(); err != nil {
		t.Fatal(err)
	}
	if len(r.stages) != 1 {
		t.Fatalf("a line still being written must wait: %d stages", len(r.stages))
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("0s\n")
	_ = f.Close()
	if err := r.scanLog(); err != nil || len(r.stages) != 2 || *r.stages[1].OM != 20*time.Second {
		t.Fatalf("after completing the line: %v %+v", err, r.stages)
	}
	if err := r.scanLog(); err != nil || len(r.stages) != 2 {
		t.Fatalf("a rescan must add nothing: %v %d", err, len(r.stages))
	}
	// rotation: a shorter file is read from the top
	if err := os.WriteFile(path, []byte("2026/10/04 01:00:00 landing_worker: [land] gt-c: stages: om 5s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.scanLog(); err != nil || len(r.stages) != 3 {
		t.Fatalf("after rotation: %v %d", err, len(r.stages))
	}
}

func TestBuildOM(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	h := func(n int) time.Time { return now.Add(-time.Duration(n) * time.Hour) }
	recs := []omRecord{
		omTestRec("gt-1", "approve", 0.8, "daemon", h(1)),
		omTestRec("gt-2", "approve", 0.6, "daemon", h(30)),
		omTestRec("gt-3", "skipped", 0, "overseer-manual", h(2)),
		omTestRec("gt-4", "error:om review did not run: context deadline exceeded", 0, "daemon", h(3)),
		omTestRec("gt-5", "overseer:abc123", 0, "daemon", h(4)),
	}
	recs[0].RiskPaths = []string{"internal/config/x.go"}
	om30s, om90s := 30*time.Second, 90*time.Second
	stages := []omStage{
		{At: h(1).Add(-time.Minute), Bead: "gt-1", OM: &om30s},
		{At: h(30).Add(-time.Minute), Bead: "gt-2", OM: &om90s},
		{At: h(60), Bead: "gt-old", OM: &om90s},
	}
	score := 0.55
	rejs := []omRejection{
		{At: h(5), Bead: "gt-6", Kind: "review", Score: &score},
		{At: h(6), Bead: "gt-7", Kind: "conflict"},
	}
	thr := 0.6
	om := buildOM(now, recs, stages, rejs, omConfig{Backend: []string{"/x/claude-deepseek-flash", "-p"}, Threshold: &thr, Depth: "standard", Timeout: 300})

	if om.Backend != "claude-deepseek-flash" || om.Depth != "standard" {
		t.Errorf("config = %q %q", om.Backend, om.Depth)
	}
	d, all := om.Windows[2], om.Windows[4]
	if d.Label != "24h" || d.Landed != 4 || d.Approved != 1 || d.Skipped != 2 || d.Errors != 1 || d.Rejected != 2 {
		t.Errorf("24h = %+v", d)
	}
	if all.Landed != 5 || all.Approved != 2 {
		t.Errorf("all = %+v", all)
	}
	if all.MedianSecs == nil || *all.MedianSecs != 90 {
		t.Errorf("median over [30 90 90] = %v", all.MedianSecs)
	}
	if d.MedianSecs == nil || *d.MedianSecs != 30 {
		t.Errorf("24h median = %v", d.MedianSecs)
	}
	// avg of approved 0.8, 0.6 and the review rejection 0.55
	if all.AvgScore == nil || *all.AvgScore < 0.649 || *all.AvgScore > 0.651 {
		t.Errorf("avg score = %v", all.AvgScore)
	}
	if om.Rejects["review"] != 1 || om.Rejects["conflict"] != 1 || om.RiskPaths != 1 {
		t.Errorf("rejects/risk = %v %d", om.Rejects, om.RiskPaths)
	}
	if om.Scores[8] != 1 || om.Scores[6] != 1 || om.Scores[5] != 1 {
		t.Errorf("score deciles = %v", om.Scores)
	}
	if om.Routes["daemon"] != 4 || om.Routes["overseer-manual"] != 1 {
		t.Errorf("routes = %v", om.Routes)
	}
	if len(om.Recent) == 0 || om.Recent[0].Bead != "gt-1" || om.Recent[0].Secs == nil || *om.Recent[0].Secs != 30 {
		t.Errorf("recent[0] = %+v", om.Recent[0])
	}
	if len(om.Days) == 0 || om.Days[0].Day != now.Format("2006-01-02") {
		t.Errorf("days = %+v", om.Days)
	}
}

func TestOMWindowBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	recs := []omRecord{
		omTestRec("gt-30m", "approve", 0.7, "daemon", now.Add(-30*time.Minute)),
		omTestRec("gt-5h", "approve", 0.5, "daemon", now.Add(-5*time.Hour)),
	}
	om := buildOM(now, recs, nil, nil, omConfig{})
	labels := []string{"1h", "6h", "24h", "7d", "all"}
	if len(om.Windows) != len(labels) {
		t.Fatalf("windows = %d, want %d", len(om.Windows), len(labels))
	}
	for i, l := range labels {
		if om.Windows[i].Label != l {
			t.Errorf("window %d = %q, want %q", i, om.Windows[i].Label, l)
		}
	}
	// 30 minutes ago is in 1h, 6h, 24h; 5 hours ago is in 6h and 24h but not 1h.
	for i, want := range []int{1, 2, 2, 2, 2} {
		if got := om.Windows[i].Landed; got != want {
			t.Errorf("%s landed = %d, want %d", labels[i], got, want)
		}
	}
}

func TestOMDaysAreTheLastFourCalendarDays(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	agoDay := func(n int) time.Time { return now.AddDate(0, 0, -n) }
	recs := []omRecord{
		omTestRec("gt-today", "approve", 0.8, "daemon", now.Add(-time.Hour)),
		omTestRec("gt-1d", "approve", 0.6, "daemon", agoDay(1)),
		// agoDay(2) has no reviews: its row must still appear.
		omTestRec("gt-3d", "error:om review did not run", 0, "daemon", agoDay(3)),
		omTestRec("gt-4d", "approve", 0.9, "daemon", agoDay(4)),
	}
	om := buildOM(now, recs, nil, nil, omConfig{})
	if len(om.Days) != 4 {
		t.Fatalf("days = %d (%+v)", len(om.Days), om.Days)
	}
	for i := 0; i < 4; i++ {
		if want := agoDay(i).Format("2006-01-02"); om.Days[i].Day != want {
			t.Errorf("days[%d] = %q, want %q", i, om.Days[i].Day, want)
		}
	}
	if d := om.Days[2]; d.Approved != 0 || d.Skipped != 0 || d.Errors != 0 || d.Rejected != 0 || d.AvgSecs != nil {
		t.Errorf("a day with no reviews must be a zero row: %+v", d)
	}
	if om.Days[0].Approved != 1 || om.Days[1].Approved != 1 || om.Days[3].Errors != 1 {
		t.Errorf("counts landed on the wrong rows: %+v", om.Days)
	}
}

func TestPercentile(t *testing.T) {
	t.Parallel()
	v := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if percentile(v, 0.5) != 50 || percentile(v, 0.95) != 100 || percentile(nil, 0.5) != 0 {
		t.Errorf("percentile wrong: %v %v", percentile(v, 0.5), percentile(v, 0.95))
	}
}
