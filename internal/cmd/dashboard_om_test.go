package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/landings"
)

// omTestMergeLine is the merge line the landing worker logs when it starts
// gating a merged tree, with the three commits it names (internal/land/land.go
// logs "merged <head> onto <remote>/<target> (<tip>) as <cand>"). om is handed
// the candidate as --head (internal/land/review.go), so the candidate is what
// joins a running review back to its bead.
const omTestMergeLine = "2026/10/12 19:57:31 landing_worker: [land] gt-7qz2h: merged 49f6dbfd onto origin/main (8d0d9b28) as 1a2b3c4d; pushing the merge candidate and waiting for its CI verdict, then om review"

// omTestProc is a process as ps lists it: pid, elapsed time, argv.
func omTestProc(pid int, elapsed time.Duration, argv string) omProc {
	return omProc{PID: pid, Elapsed: elapsed, Args: strings.Fields(argv)}
}

func TestParsePsEtime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tok  string
		want time.Duration
		ok   bool
	}{
		{"00:03:12", 3*time.Minute + 12*time.Second, true},
		{"02:27:05", 2*time.Hour + 27*time.Minute + 5*time.Second, true},
		{"12:34", 12*time.Minute + 34*time.Second, true},
		{"3-02:03:04", 3*24*time.Hour + 2*time.Hour + 3*time.Minute + 4*time.Second, true},
		{"", 0, false},
		{"not-a-time", 0, false},
		{"1:2:3:4", 0, false},
		{"-1", 0, false},
	}
	for _, c := range cases {
		got, err := parsePsEtime(c.tok)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parsePsEtime(%q) = %v, %v; want %v", c.tok, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parsePsEtime(%q) = %v; want an error", c.tok, got)
		}
	}
}

// A ps listing is untrusted text: a line that is not a process, and a line
// whose elapsed time is not one, are dropped rather than read as rows.
func TestParseOMProcsDropsWhatIsNotAProcess(t *testing.T) {
	t.Parallel()
	out := []byte("    1 02:27:05 /sbin/launchd\n" +
		"  501 00:03:12 om review -C /tmp/gt-landing-501/fractals/land-1/wt --head 1a2b3c4d\n" +
		"not a process line\n" +
		"  502 not-a-time om review -C /x\n" +
		"  503 3-02:03:04 om review\n")
	procs := parseOMProcs(out)
	if len(procs) != 3 {
		t.Fatalf("procs = %+v, want the three whole lines", procs)
	}
	if procs[0].PID != 1 || procs[0].Elapsed != 2*time.Hour+27*time.Minute+5*time.Second {
		t.Errorf("procs[0] = %+v", procs[0])
	}
	if procs[1].PID != 501 || procs[1].Elapsed != 3*time.Minute+12*time.Second {
		t.Errorf("procs[1] = %+v", procs[1])
	}
	if len(procs[1].Args) != 6 || procs[1].Args[0] != "om" || procs[1].Args[1] != "review" {
		t.Errorf("procs[1] argv = %v", procs[1].Args)
	}
	if procs[2].PID != 503 || procs[2].Elapsed != 3*24*time.Hour+2*time.Hour+3*time.Minute+4*time.Second {
		t.Errorf("procs[2] = %+v", procs[2])
	}
}

// The merge line names the landing's three commits. The candidate is the one
// om is handed as --head, so the reader keeps all three.
func TestOMMergeLineKeepsTheLandingsCommits(t *testing.T) {
	t.Parallel()
	r := &omReader{}
	r.parseLogLine(omTestMergeLine)
	m, ok := r.merges["gt-7qz2h"]
	if !ok {
		t.Fatalf("merges = %+v", r.merges)
	}
	if m.Head != "49f6dbfd" || m.Tip != "8d0d9b28" || m.Cand != "1a2b3c4d" {
		t.Errorf("merge = %+v, want head/tip/cand", m)
	}
	if m.Done {
		t.Error("a merge with no landed line after it is still in flight")
	}
	// A merge line that does not name them still begins a merge: the running
	// row survives a wording change, only the join is lost.
	r.parseLogLine("2026/10/12 19:58:00 landing_worker: [land] gt-other: merged something else entirely")
	if m := r.merges["gt-other"]; m.Bead != "gt-other" || m.Cand != "" || m.Done {
		t.Errorf("merge without the commit tail = %+v", m)
	}
	// A rig that lands through a pull request logs a second line for the same
	// landing, naming no commits. It must not erase the candidate the first
	// line named: the running review is joined by it.
	r.parseLogLine("2026/10/12 19:59:00 landing_worker: [land] gt-7qz2h: merged 1a2b3c4d on main through pull request #213")
	if m := r.merges["gt-7qz2h"]; m.Head != "49f6dbfd" || m.Tip != "8d0d9b28" || m.Cand != "1a2b3c4d" {
		t.Errorf("the pull-request line erased the landing's commits: %+v", m)
	}
}

// A running review is read off its process's argv, and a process that is not
// one om review is not a review.
func TestOMReviewArgvOf(t *testing.T) {
	t.Parallel()
	a, ok := omReviewArgvOf(strings.Fields("om review -C /tmp/gt-landing-501/fractals/land-1/wt --base 8d0d9b28 --head 1a2b3c4d --out /tmp/v.json"))
	if !ok || a.Rig != "fractals" || a.Dir != "/tmp/gt-landing-501/fractals/land-1/wt" || a.Base != "8d0d9b28" || a.Head != "1a2b3c4d" {
		t.Errorf("review argv = %+v, %v", a, ok)
	}
	notReviews := []string{
		"om dashboard",
		"om review --base a --head b",      // no -C
		"om review -C /tmp/wt --head b",    // not a landing worktree
		"om review -C /tmp/gt-landing-501", // a root with no rig under it
		"om review -C",                     // a -C left without a value
		"/sbin/launchd",
	}
	for _, argv := range notReviews {
		if a, ok := omReviewArgvOf(strings.Fields(argv)); ok {
			t.Errorf("omReviewArgvOf(%q) = %+v; want not a review", argv, a)
		}
	}
	// A review still in flight to its flags carries no head yet, and is
	// listed with none: it is a review running, not a malformed line.
	if a, ok := omReviewArgvOf(strings.Fields("om review -C /tmp/gt-landing-501/x")); !ok || a.Head != "" || a.Rig != "x" {
		t.Errorf("a review with no --head yet = %+v, %v", a, ok)
	}
}

// omTestLogFile is a daemon.log holding lines, for a reader to scan.
func omTestLogFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A running review is listed from the process that runs it: the rig and the
// commits off its argv, the bead off the merge line that names its --head as
// the merge candidate, and the polecat off the caller's bead read. A review no
// merge line names yet is still listed, with no bead. One ps read serves the
// whole panel read, and the rows ride in its JSON.
func TestOMReadListsTheReviewsRunningNow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.Local)
	calls := 0
	r := &omReader{
		logPath:  omTestLogFile(t, omTestMergeLine),
		landings: &dashLandings{},
		procs: func(context.Context) ([]omProc, error) {
			calls++
			return []omProc{
				omTestProc(501, 3*time.Minute, "om review -C /tmp/gt-landing-501/fractals/land-1/wt --base 8d0d9b28c1e2f3a4 --head 1a2b3c4d5e6f7081 --out /tmp/v.json"),
				omTestProc(502, 90*time.Second, "om review -C /var/folders/xy/T/gt-landing-501/gastown/land-2/wt --base aaaa --head bbbb --out /tmp/v.json"),
				omTestProc(7, 10*time.Hour, "/sbin/launchd"),
			}, nil
		},
		polecatFor: func(rig, bead string) string { return rig + ":" + bead },
	}
	om := r.read(now)
	if om == nil {
		t.Fatal("the log has a merge line; the read must not come back empty")
	}
	if calls != 1 {
		t.Fatalf("the lister was called %d times; one read lists the processes once", calls)
	}
	rows := om.Running
	if len(rows) != 2 {
		t.Fatalf("running = %+v, want the two reviews and not launchd", rows)
	}
	// Newest first: the minute-and-a-half review started after the other.
	unmatched := rows[0]
	if unmatched.PID != 502 || unmatched.Rig != "gastown" || unmatched.Bead != "" || unmatched.Polecat != "" {
		t.Errorf("unmatched review = %+v; want its rig, no bead and no polecat", unmatched)
	}
	if unmatched.Started != now.Add(-90*time.Second) || unmatched.Base != "aaaa" || unmatched.Head != "bbbb" {
		t.Errorf("unmatched review = %+v", unmatched)
	}
	joined := rows[1]
	if joined.PID != 501 || joined.Bead != "gt-7qz2h" || joined.Rig != "fractals" || joined.Polecat != "fractals:gt-7qz2h" {
		t.Errorf("joined review = %+v; want the merge line's bead", joined)
	}
	if joined.Started != now.Add(-3*time.Minute) || joined.Base != "8d0d9b28" || joined.Head != "1a2b3c4d" {
		t.Errorf("joined review = %+v; the row names what om was handed, short", joined)
	}
	b, err := json.Marshal(om)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"running":[`) {
		t.Errorf("the running reviews are missing from the JSON: %s", b)
	}
	// Nothing reviewed in this read, so the panel's last-five list is left out
	// rather than drawn empty.
	if strings.Contains(string(b), `"last":`) {
		t.Errorf("a panel with no review ran must leave last out: %s", b)
	}
}

// A host with no ps, one whose ps fails, and a reader that lists nothing cost
// the panel its running rows and nothing else: the rest of the om read is
// still the panel.
func TestOMRunningRowsSurviveAListerFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.Local)
	logPath := omTestLogFile(t, omTestMergeLine)
	r := &omReader{logPath: logPath, landings: &dashLandings{},
		procs: func(context.Context) ([]omProc, error) { return nil, errors.New("ps: not found") }}
	if om := r.read(now); om == nil || len(om.Running) != 0 {
		t.Errorf("a failed lister must leave the panel and drop only its rows: %+v", om)
	}
	r = &omReader{logPath: logPath, landings: &dashLandings{}}
	if om := r.read(now); om == nil || len(om.Running) != 0 {
		t.Errorf("a reader with no lister must still read the panel: %+v", om)
	}
	if rows := (&omReader{}).runningRows(map[string]string{"abc12345": "gt-1"}, nil, now); len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
}

// Last is the newest reviews om actually ran, newest first and capped: a
// skipped landing was never reviewed, and is not one.
func TestOMLastListsOnlyReviewsOmRan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 18, 0, 0, 0, time.Local)
	at := func(min int) time.Time { return now.Add(-time.Duration(min) * time.Minute) }
	recs := []omRecord{
		omTestRec("gt-skip", "skipped", 0, "overseer-manual", at(1)),
		omTestRec("gt-1", "approve", 0.9, "daemon", at(2)),
		omTestRec("gt-2", "approve", 0.4, "daemon", at(3)),
		omTestRec("gt-3", "error:om review did not run", 0, "daemon", at(4)),
		omTestRec("gt-4", "approve", 0.7, "daemon", at(5)),
		omTestRec("gt-5", "approve", 0.7, "daemon", at(6)),
		omTestRec("gt-6", "approve", 0.7, "daemon", at(7)),
	}
	om := buildOM(now, recs, nil, nil, omConfig{})
	if len(om.Last) != omLastRows {
		t.Fatalf("last = %+v, want %d rows", om.Last, omLastRows)
	}
	var beads []string
	for _, rv := range om.Last {
		beads = append(beads, rv.Bead)
	}
	want := []string{"gt-1", "gt-2", "gt-3", "gt-4", "gt-5"}
	if strings.Join(beads, ",") != strings.Join(want, ",") {
		t.Errorf("last = %v, want %v (newest first, no skip, no sixth)", beads, want)
	}
	if len(om.Recent) != len(recs) {
		t.Errorf("recent = %d rows; every landing is still in the tail", len(om.Recent))
	}
}

// A review rejection is a review that ran: it is listed with the rig its bead's
// landing record names and om's own wall time, and one with no score is still
// listed. A rejection decided before om ran is not a review that ran.
func TestOMLastGivesARejectionItsRigAndSeconds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 18, 0, 0, 0, time.Local)
	landed := omTestRec("gt-rej", "approve", 0.5, "daemon", now.Add(-time.Hour))
	landed.Rig = "fractals"
	om90 := 90 * time.Second
	stages := []omStage{{At: now.Add(-31 * time.Minute), Bead: "gt-rej", OM: &om90}}
	score := 0.55
	rejs := []omRejection{
		{At: now.Add(-30 * time.Minute), Bead: "gt-rej", Kind: "review", Score: &score},
		{At: now.Add(-25 * time.Minute), Bead: "gt-noscore", Kind: "review"},
		{At: now.Add(-20 * time.Minute), Bead: "gt-gate", Kind: "gate"},
	}
	om := buildOM(now, []omRecord{landed}, stages, rejs, omConfig{})
	byBead := map[string]dashboard.OMReview{}
	for _, rv := range om.Last {
		byBead[rv.Bead+"/"+rv.Outcome] = rv
	}
	if len(om.Last) != 3 {
		t.Fatalf("last = %+v, want the landing and the two reviews", om.Last)
	}
	if om.Last[0].Bead != "gt-noscore" || !om.Last[0].At.Equal(now.Add(-25*time.Minute)) {
		t.Errorf("last[0] = %+v; the newest review ran 25 minutes ago", om.Last[0])
	}
	if rj := byBead["gt-rej/rejected"]; rj.Rig != "fractals" || rj.Route != "daemon" || rj.Score == nil || *rj.Score != 0.55 {
		t.Errorf("review rejection = %+v; want its rig, route and score", rj)
	} else if rj.Secs == nil || *rj.Secs != 90 {
		t.Errorf("review rejection secs = %v, want om's 90s", rj.Secs)
	}
	if rj := byBead["gt-noscore/rejected"]; rj.Score != nil || rj.Rig != "" || rj.Secs != nil {
		t.Errorf("a rejection without a score is still a review that ran: %+v", rj)
	}
	if _, ok := byBead["gt-gate/rejected"]; ok {
		t.Error("a gate rejection was decided before om ran, and is not the panel's last")
	}
	if _, ok := byBead["gt-rej/approved"]; !ok {
		t.Error("the landing itself is a review om ran too")
	}
	// Recent keeps its longer tail, and its rejection rows are unchanged.
	found := false
	for _, rv := range om.Recent {
		if rv.Bead == "gt-rej" && rv.Outcome == "rejected" {
			found = true
			if rv.Rig != "" || rv.Route != "" {
				t.Errorf("recent rejection row = %+v; the tail row is unchanged", rv)
			}
		}
	}
	if !found {
		t.Error("the rejection is missing from the tail")
	}
}

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

	rows := buildRecentLandings(now, recs, stages, nil, nil, nil, nil, nil, 30)
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

// A landing pass that stops on an infrastructure failure logs no [land] line,
// only the worker's "landing failed" line, and that line must end the merge the
// pass began: the pane showed the bead as running beside its backoff row
// (gt-7lcem). The lines go through scanLog, whose prefilter decides which of
// them the parser ever sees.
func TestScanLogEndsAMergeAtTheLandingsOutcome(t *testing.T) {
	t.Parallel()
	const (
		failed     = "2026/10/12 19:58:10 landing_worker: gastown: gt-7qz2h: landing failed (1 in a row, next try in 30s): landing failed at gate: ci unreachable"
		landed     = "2026/10/12 19:58:10 landing_worker: [land] gt-7qz2h: landed 1a2b3c4d on main"
		rejected   = "2026/10/12 19:58:10 landing_worker: [land] gt-7qz2h: rejected (gate): the candidate gate failed"
		otherFails = "2026/10/12 19:58:10 landing_worker: gastown: gt-7qz2h: reading the branch tip failed (1 in a row, next try in 30s): no ref"
		otherBead  = "2026/10/12 19:58:10 landing_worker: gastown: gt-other: landing failed (1 in a row, next try in 30s): landing failed at push: refused"
		earlier    = "2026/10/12 19:50:00 landing_worker: gastown: gt-7qz2h: landing failed (2 in a row, next try in 1m0s): landing failed at review: timeout"
	)
	tests := []struct {
		name  string
		lines []string
		done  bool
	}{
		{"merge with nothing after it is running", []string{omTestMergeLine}, false},
		{"infra failure ends the merge", []string{omTestMergeLine, failed}, true},
		{"landed ends the merge", []string{omTestMergeLine, landed}, true},
		{"rejection ends the merge", []string{omTestMergeLine, rejected}, true},
		{"another stage's failure leaves the merge running", []string{omTestMergeLine, otherFails}, false},
		{"another bead's failure leaves the merge running", []string{omTestMergeLine, otherBead}, false},
		{"an earlier attempt's failure leaves the newer merge running", []string{omTestMergeLine, earlier}, false},
		{"a retry's merge runs again after the failure", []string{omTestMergeLine, failed, "2026/10/12 20:01:00 landing_worker: [land] gt-7qz2h: merged 49f6dbfd onto origin/main (8d0d9b28) as 5e6f7a8b; pushing the merge candidate"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &omReader{logPath: omTestLogFile(t, tt.lines...)}
			if err := r.scanLog(); err != nil {
				t.Fatal(err)
			}
			m, ok := r.merges["gt-7qz2h"]
			if !ok {
				t.Fatalf("merges = %+v, want gt-7qz2h", r.merges)
			}
			if m.Done != tt.done {
				t.Errorf("Done = %v, want %v", m.Done, tt.done)
			}
			if got := len(r.running(time.Date(2026, 10, 12, 0, 0, 0, 0, time.Local))); got != map[bool]int{true: 0, false: 1}[tt.done] {
				t.Errorf("running rows = %d for Done=%v", got, tt.done)
			}
		})
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

	// The backend is a command line; the panel names the model it runs, which
	// for this wrapper is the family in its own name (gt-bj47s).
	if om.Model != "flash" || om.Depth != "standard" {
		t.Errorf("config = %q %q", om.Model, om.Depth)
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

// TestReadOMRecordsReadsPastALongLine: a line longer than the scanner's buffer
// must not hide every record after it (gt-2czgm).
func TestReadOMRecordsReadsPastALongLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "om.jsonl")
	body := `{"bead":"gt-first"}` + "\n" +
		strings.Repeat("x", 5*1024*1024) + "\n" +
		`{"bead":"gt-last"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var beads []string
	for _, rec := range readOMRecords(path) {
		beads = append(beads, rec.Bead)
	}
	if len(beads) != 2 || beads[0] != "gt-first" || beads[1] != "gt-last" {
		t.Fatalf("records = %v; a long line must not hide the records after it", beads)
	}
}
