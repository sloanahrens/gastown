package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// tierSweepTestReader is a reader over fixture daemon-log lines, with no clock
// and no real log: read() takes its now, so a test never waits.
func tierSweepTestReader(t *testing.T, lines ...string) (*tierSweepReader, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &tierSweepReader{logPath: path, stateDir: filepath.Join(dir, "tier-sweep"),
		rigs: func() ([]string, error) { return []string{"gastown"}, nil },
		last: map[string]tierSweepEvent{}, start: map[string]tierSweepStarted{}}, path
}

func TestTierSweepReadsACycleFromTheLog(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t,
		"2026/10/03 22:51:48 tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:",
		"tier-sweep: shell GREEN passed=6 failed=0 skipped=0 (logs /tmp/tier-sweep.x) in 1m4s",
		"2026/10/03 23:00:19 tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration GREEN, race GREEN) in 9m36s",
		`2026/10/03 23:00:19 dog_cycle: tier_sweep outcome=ran steps=[gastown=done] reason=""`,
	)
	ts := r.read(time.Date(2026, 10, 3, 23, 5, 0, 0, time.Local))
	if ts.Unavailable || ts.Running != nil {
		t.Fatalf("a closed cycle reads idle, not unavailable: %+v", ts)
	}
	if len(ts.Sweeps) != 1 {
		t.Fatalf("sweeps = %+v", ts.Sweeps)
	}
	row := ts.Sweeps[0]
	if row.Rig != "gastown" || row.SHA != "a9be03e4" || row.Secs == nil || *row.Secs != 576 {
		t.Fatalf("row = %+v", row)
	}
	if len(row.Stages) != 3 || row.Stages[1].Tier != "integration" || row.Stages[2].Verdict != "GREEN" {
		t.Errorf("stages = %+v", row.Stages)
	}
}

// The daemon's "sweep started" line states the cycle exactly: the rig is
// running from that timestamp, over every tier the line names, until a swept
// line closes it (gt-rntre).
func TestTierSweepRunningFromTheSweepStartedLine(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 4, 14, 0, 0, 0, time.Local)
	r, _ := tierSweepTestReader(t,
		"2026/10/04 14:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell, integration, race)",
		// The shell stage finishes while the integration stage runs: the pane
		// still reads the start line's whole list, not the fallback's single
		// inferred stage.
		"2026/10/04 14:01:05 tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:",
	)
	ts := r.read(start.Add(4 * time.Minute))
	if ts.Running == nil {
		t.Fatalf("a started cycle with no swept line is running: %+v", ts)
	}
	run := ts.Running
	if run.Rig != "gastown" || run.ElapsedSec != 240 {
		t.Errorf("running = %+v, want gastown at 4m", run)
	}
	if len(run.Tiers) != 3 || run.Tiers[0] != "shell" || run.Tiers[2] != "race" {
		t.Errorf("running tiers = %+v, want the line's list", run.Tiers)
	}
	if ts := r.read(start.Add(tierSweepStaleAfter + time.Minute)); ts.Running != nil {
		t.Errorf("a start older than the stage budget is not a live sweep: %+v", ts.Running)
	}
}

// An odd-hour cycle is the shell tier alone, and the pane says so rather than
// inferring an integration stage that will never run (gt-rntre).
func TestTierSweepRunningShellOnlyCycle(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 4, 15, 0, 0, 0, time.Local)
	r, _ := tierSweepTestReader(t, "2026/10/04 15:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell)")
	ts := r.read(start.Add(time.Minute))
	if ts.Running == nil {
		t.Fatalf("a started cycle with no swept line is running: %+v", ts)
	}
	if run := ts.Running; len(run.Tiers) != 1 || run.Tiers[0] != "shell" {
		t.Errorf("running tiers = %+v, want shell alone", run.Tiers)
	}
}

// A swept line closes the cycle its start opened: the rig reads idle, and the
// sweep appears in the rows.
func TestTierSweepSweptClosesTheStartedCycle(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t,
		"2026/10/04 14:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell, integration, race)",
		"2026/10/04 14:09:36 tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration GREEN, race GREEN) in 9m36s",
	)
	ts := r.read(time.Date(2026, 10, 4, 14, 20, 0, 0, time.Local))
	if ts.Running != nil {
		t.Errorf("a started-then-swept cycle still reads running: %+v", ts.Running)
	}
	if len(ts.Sweeps) != 1 || ts.Sweeps[0].Rig != "gastown" {
		t.Errorf("sweeps = %+v", ts.Sweeps)
	}
}

// A log from a daemon that logs no cycle start is still read: the sweep reads
// as running from the shell stage's finished line until the swept line closes
// the cycle, the fallback for an older daemon or a restart mid-sweep.
func TestTierSweepRunningWhileASweepIsInFlight(t *testing.T) {
	t.Parallel()
	shellDone := time.Date(2026, 10, 3, 22, 51, 48, 0, time.Local)
	r, _ := tierSweepTestReader(t,
		"2026/10/03 22:51:48 tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:",
		"tier-sweep: shell GREEN passed=6 failed=0 skipped=0 (logs /tmp/tier-sweep.x) in 1m4s",
	)
	ts := r.read(shellDone.Add(3 * time.Minute))
	if ts.Running == nil {
		t.Fatalf("a finished shell stage with no swept line is a sweep in flight: %+v", ts)
	}
	if run := ts.Running; run.Rig != "gastown" || run.Tier != "integration" || run.ElapsedSec != 180 {
		t.Errorf("running = %+v", run)
	}
	if ts := r.read(shellDone.Add(tierSweepStaleAfter + time.Minute)); ts.Running != nil {
		t.Errorf("a shell line older than the stage budget is not a live sweep: %+v", ts.Running)
	}
	r.parseLogLine("2026/10/03 23:00:19 tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration GREEN, race GREEN) in 9m36s")
	if ts := r.read(shellDone.Add(9 * time.Minute)); ts.Running != nil {
		t.Errorf("a swept cycle still reads running: %+v", ts.Running)
	}
}

func TestTierSweepLineWithoutADuration(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t, "2026/10/01 09:00:00 tier_sweep: gastown: swept deadbeef (shell GREEN)")
	ts := r.read(time.Date(2026, 10, 3, 0, 0, 0, 0, time.Local))
	if len(ts.Sweeps) != 1 || ts.Sweeps[0].Secs != nil {
		t.Fatalf("a swept line from before gt-iqzr0 has no duration: %+v", ts.Sweeps)
	}
	if s := ts.Sweeps[0].Stages; len(s) != 1 || s[0].Verdict != "GREEN" {
		t.Errorf("stages = %+v", s)
	}
}

func TestTierSweepRedNamesComeFromTheRecord(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t, "2026/10/03 23:00:19 tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration RED, race GREEN) in 9m36s")
	writeTierSweepRecord(t, r.stateDir, "gastown", `{"last_sha":"a9be03e4ffff","tiers":{"integration":{"verdict":"RED","passed":1,"failed":2,"failed_names":["./internal/cmd","./internal/daemon"]}}}`)
	ts := r.read(time.Date(2026, 10, 3, 23, 5, 0, 0, time.Local))
	got := ts.Sweeps[0].Stages[1]
	if got.Verdict != "RED" || len(got.Failed) != 2 || got.Failed[0] != "./internal/cmd" {
		t.Fatalf("red tier = %+v", got)
	}

	// The record holds one run per rig, so a record for another sha names nothing.
	other, _ := tierSweepTestReader(t, "2026/10/03 23:00:19 tier_sweep: gastown: swept a9be03e4 (integration RED) in 1m")
	writeTierSweepRecord(t, other.stateDir, "gastown", `{"last_sha":"0000ffff","tiers":{"integration":{"verdict":"RED","failed_names":["x"]}}}`)
	if got := other.read(time.Date(2026, 10, 3, 23, 5, 0, 0, time.Local)).Sweeps[0].Stages[0]; len(got.Failed) != 0 {
		t.Errorf("another sweep's record named this row's failures: %+v", got)
	}
}

// A cycle that stopped early left the tier it never reached at whatever tip
// that tier was last judged at, so the record can name more than one. The names
// belong to the row for their own tip: attaching a red nobody re-ran to this
// cycle's tip would date it to a commit it was never seen at (gt-euxf1). The
// swept line is the one the daemon formats for such a cycle: the tier that did
// not run is named with no verdict, so the row has no stage for it and its
// carried names go unread.
func TestTierSweepCarriedTierNamesStayWithTheirOwnTip(t *testing.T) {
	t.Parallel()
	swept := daemon.TierSweepSweptLine("gastown", "tipb0001ffff", [][]string{{"shell"}, {"integration", "race"}},
		map[string]string{"shell": "GREEN", "race": "RED"}, 4*time.Minute)
	r, _ := tierSweepTestReader(t, "2026/10/05 10:00:19 "+swept)
	writeTierSweepRecord(t, r.stateDir, "gastown", `{"last_sha":"tipb0001ffff","partial":true,"tiers":{`+
		`"shell":{"verdict":"GREEN","sha":"tipb0001ffff"},`+
		`"integration":{"verdict":"RED","failed_names":["./internal/daemon"],"sha":"tipa0002ffff"},`+
		`"race":{"verdict":"RED","failed_names":["./internal/cmd"],"sha":"tipb0001ffff"}}}`)
	stages := r.read(time.Date(2026, 10, 5, 10, 5, 0, 0, time.Local)).Sweeps[0].Stages
	if len(stages) != 2 {
		t.Fatalf("stages = %+v, want shell and race: integration did not run", stages)
	}
	for _, st := range stages {
		if st.Tier == "integration" {
			t.Errorf("carried integration shows as %+v: those failing units are tipa0002's", st)
		}
	}
	if got := stages[1]; got.Tier != "race" || got.Verdict != "RED" || len(got.Failed) != 1 || got.Failed[0] != "./internal/cmd" {
		t.Errorf("race = %+v, want this row's own RED with its names", got)
	}
}

// A stage the daemon started beside a busy CI runner is annotated "under load"
// in the swept line. The annotation is not part of the verdict, so the panel
// still reads GREEN and still attaches a RED tier's failing names (gt-5ejux).
func TestTierSweepUnderLoadAnnotationIsNotAVerdict(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t, "2026/10/04 16:00:19 tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration GREEN under load, race RED under load) in 9m36s")
	writeTierSweepRecord(t, r.stateDir, "gastown", `{"last_sha":"a9be03e4ffff","tiers":{"race":{"verdict":"RED","failed_names":["./internal/cmd"]}}}`)
	ts := r.read(time.Date(2026, 10, 4, 16, 5, 0, 0, time.Local))
	if len(ts.Sweeps) != 1 {
		t.Fatalf("sweeps = %+v", ts.Sweeps)
	}
	stages := ts.Sweeps[0].Stages
	if len(stages) != 3 {
		t.Fatalf("stages = %+v", stages)
	}
	for i, want := range []string{"GREEN", "GREEN", "RED"} {
		if stages[i].Verdict != want {
			t.Errorf("stage %d verdict = %q, want %q", i, stages[i].Verdict, want)
		}
	}
	if len(stages[2].Failed) != 1 || stages[2].Failed[0] != "./internal/cmd" {
		t.Errorf("red stage = %+v, want the record's failing names", stages[2])
	}
}

// A tier marked loaded whose run did not happen is logged as "integration  under
// load" with no verdict between the name and the annotation. The panel must not
// read "under" as the verdict: the stage was never run, so it is left out.
func TestTierSweepUnderLoadWithoutVerdictIsNotAStage(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t, "2026/10/04 16:00:19 tier_sweep: gastown: swept abcdef12 (shell GREEN, integration  under load) in 1m2s")
	ts := r.read(time.Date(2026, 10, 4, 16, 5, 0, 0, time.Local))
	if len(ts.Sweeps) != 1 {
		t.Fatalf("sweeps = %+v", ts.Sweeps)
	}
	stages := ts.Sweeps[0].Stages
	if len(stages) != 1 || stages[0].Tier != "shell" || stages[0].Verdict != "GREEN" {
		t.Fatalf("stages = %+v, want only shell GREEN: integration never ran", stages)
	}
}

func writeTierSweepRecord(t *testing.T, dir, rig, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rig+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTierSweepUnreadableLog(t *testing.T) {
	t.Parallel()
	r := &tierSweepReader{logPath: filepath.Join(t.TempDir(), "missing.log"), stateDir: t.TempDir(),
		last: map[string]tierSweepEvent{}, start: map[string]tierSweepStarted{}}
	ts := r.read(time.Now())
	if !ts.Unavailable {
		t.Fatalf("an unreadable log must not read as a log with no sweeps: %+v", ts)
	}
}

func TestTierSweepScanIsIncremental(t *testing.T) {
	t.Parallel()
	r, path := tierSweepTestReader(t, "2026/10/03 22:00:00 tier_sweep: gastown: swept 11111111 (shell GREEN) in 1m")
	now := time.Date(2026, 10, 3, 23, 0, 0, 0, time.Local)
	if ts := r.read(now); len(ts.Sweeps) != 1 {
		t.Fatalf("sweeps = %+v", ts.Sweeps)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("2026/10/03 23:00:00 tier_sweep: gastown: swept 22222222 (shell GREEN, race GREEN) in 2m\n")
	_ = f.Close()
	ts := r.read(now)
	if len(ts.Sweeps) != 2 || ts.Sweeps[0].SHA != "22222222" {
		t.Fatalf("an appended line is read once, newest first: %+v", ts.Sweeps)
	}
	if ts := r.read(now); len(ts.Sweeps) != 2 {
		t.Fatalf("a rescan adds nothing: %+v", ts.Sweeps)
	}
}

// TestTierSweepIgnoresARigTheRegistryDoesNotKnow: the rig reaches the record
// path straight from a daemon-log line, so a name the town's registry does not
// hold — here one carrying a path separator — must not be turned into a path
// (gt-2czgm).
func TestTierSweepIgnoresARigTheRegistryDoesNotKnow(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t, "2026/10/03 23:00:19 tier_sweep: ../escaped: swept a9be03e4 (shell GREEN, integration RED) in 9m36s")
	// A record sitting one level above the state directory, where the
	// unvalidated join would look for it.
	body := `{"last_sha":"a9be03e4ffff","tiers":{"integration":{"verdict":"RED","failed_names":["./internal/cmd"]}}}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(r.stateDir), "escaped.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := r.read(time.Date(2026, 10, 3, 23, 5, 0, 0, time.Local))
	if len(ts.Sweeps) != 1 {
		t.Fatalf("sweeps = %+v", ts.Sweeps)
	}
	for _, stage := range ts.Sweeps[0].Stages {
		if len(stage.Failed) != 0 {
			t.Fatalf("stage %+v carries names from a record outside the state directory", stage)
		}
	}
}

// The daemon closes a cycle that started and swept nothing (no stage ran, or
// the record was not written) with an aborted line: the rig reads idle at once,
// not running until tierSweepStaleAfter expires. A later cycle of the same rig
// opens a new start, and another rig's open cycle is left alone.
func TestTierSweepReaderAbortedCycle(t *testing.T) {
	t.Parallel()
	aborted := daemon.TierSweepAbortedLine("gastown", "a9be03e4ffff", "no stage ran")
	r, _ := tierSweepTestReader(t,
		"2026/10/04 14:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell, integration, race)",
		"2026/10/04 14:00:01 tier_sweep: beads: sweep started 11111111 (shell)",
		"2026/10/04 14:00:02 "+aborted,
	)
	ts := r.read(time.Date(2026, 10, 4, 14, 5, 0, 0, time.Local))
	if ts.Running == nil || ts.Running.Rig != "beads" {
		t.Fatalf("running = %+v, want only beads: gastown's cycle aborted", ts.Running)
	}
	if _, open := r.start["gastown"]; open {
		t.Errorf("gastown still has an open start after its aborted line")
	}

	r2, _ := tierSweepTestReader(t,
		"2026/10/04 14:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell, integration, race)",
		"2026/10/04 14:00:02 "+aborted,
	)
	if ts := r2.read(time.Date(2026, 10, 4, 14, 5, 0, 0, time.Local)); ts.Running != nil {
		t.Errorf("an aborted cycle still reads running: %+v", ts.Running)
	}
}

// An aborted line closes the cycle it names by sha. One for a cycle already
// replaced (the log shows it after the newer start) leaves the running cycle
// of the same rig open.
func TestTierSweepReaderAbortedLineForAnotherShaKeepsTheOpenCycle(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t,
		"2026/10/04 14:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell)",
		"2026/10/04 14:01:00 tier_sweep: gastown: sweep started b7c1d2e3 (shell)",
		"2026/10/04 14:01:01 "+daemon.TierSweepAbortedLine("gastown", "a9be03e4ffff", "no stage ran"),
	)
	ts := r.read(time.Date(2026, 10, 4, 14, 2, 0, 0, time.Local))
	if ts.Running == nil || ts.Running.Rig != "gastown" {
		t.Fatalf("running = %+v, want gastown's newer cycle still open", ts.Running)
	}
	if open := r.start["gastown"]; open.sha != "b7c1d2e3" {
		t.Errorf("open start sha = %q, want b7c1d2e3", open.sha)
	}
}

// The wall trend is one series per rig, oldest first, and each point names the
// sweep's sha, the time every tier took, and the cycle's total: the verdicts
// the table shows say nothing about whether devops test speed moved. A tier's
// wall comes off its stage line, because the swept line that closes a cycle
// carries only the total. A known rig with no sweeps keeps an empty series so
// the page can say so rather than leaving the rig out.
func TestTierSweepTrendPerRigCarriesTierWalls(t *testing.T) {
	t.Parallel()
	r, _ := tierSweepTestReader(t,
		"2026/10/03 20:00:00 tier_sweep: gastown: sweep started a9be03e4 (shell, integration, race)",
		"2026/10/03 20:01:05 tier_sweep: gastown: shell finished (exit 0) in 1m5s; last lines:",
		"2026/10/03 20:04:05 tier_sweep: gastown: integration finished (exit 0) in 3m0s; last lines:",
		"2026/10/03 20:05:35 tier_sweep: gastown: race finished (exit 0) in 1m30s; last lines:",
		"2026/10/03 20:09:36 tier_sweep: gastown: swept a9be03e4 (shell GREEN, integration GREEN, race GREEN) in 9m36s",
		"2026/10/03 21:00:00 tier_sweep: beads: sweep started b4c5d6e7 (shell, integration)",
		"2026/10/03 21:02:00 tier_sweep: beads: shell finished (exit 0) in 2m0s; last lines:",
		"2026/10/03 21:06:00 tier_sweep: beads: integration finished (exit 0) in 4m0s; last lines:",
		"2026/10/03 21:08:00 tier_sweep: beads: swept b4c5d6e7 (shell GREEN, integration GREEN) in 8m0s",
		"2026/10/03 22:00:00 tier_sweep: longeye: sweep started c7d8e9f0 (shell)",
		"2026/10/03 22:01:30 tier_sweep: longeye: shell finished (exit 0) in 1m30s; last lines:",
		"2026/10/03 22:02:00 tier_sweep: longeye: swept c7d8e9f0 (shell GREEN) in 2m0s",
	)
	r.rigs = func() ([]string, error) { return []string{"gastown", "beads", "longeye", "quiet"}, nil }
	ts := r.read(time.Date(2026, 10, 3, 23, 0, 0, 0, time.Local))

	byRig := map[string]dashboard.TierSweepTrendRig{}
	for _, tr := range ts.Trends {
		byRig[tr.Rig] = tr
	}
	if len(ts.Trends) != 4 {
		t.Fatalf("trends = %+v, want one per known rig", ts.Trends)
	}
	if got := []string{ts.Trends[0].Rig, ts.Trends[1].Rig, ts.Trends[2].Rig, ts.Trends[3].Rig}; got[0] != "beads" || got[3] != "quiet" {
		t.Errorf("trend order = %v, want the rigs sorted", got)
	}

	g := byRig["gastown"]
	if len(g.Sweeps) != 1 {
		t.Fatalf("gastown sweeps = %+v", g.Sweeps)
	}
	p := g.Sweeps[0]
	if p.SHA != "a9be03e4" || p.TotalSecs == nil || *p.TotalSecs != 576 {
		t.Fatalf("gastown point = %+v, want a9be03e4 at 9m36s", p)
	}
	for tier, secs := range map[string]float64{"shell": 65, "integration": 180, "race": 90} {
		if p.Walls[tier] != secs {
			t.Errorf("gastown %s wall = %v, want %v", tier, p.Walls[tier], secs)
		}
	}
	if len(byRig["longeye"].Sweeps) != 1 || byRig["longeye"].Sweeps[0].Walls["shell"] != 90 {
		t.Errorf("longeye = %+v, want its one shell wall", byRig["longeye"])
	}
	if q, ok := byRig["quiet"]; !ok || len(q.Sweeps) != 0 {
		t.Errorf("quiet = %+v, want an empty trend for a rig with no sweeps", q)
	}
}

// A rig's trend keeps the newest 30 sweeps, not the first: the pane exists to
// show devops speed moving now, and an older window would freeze a month ago.
func TestTierSweepTrendKeepsTheNewest30(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := 0; i < 35; i++ {
		lines = append(lines, fmt.Sprintf("2026/10/03 10:%02d:00 tier_sweep: gastown: swept %08x (shell GREEN) in 1m", i, i+1))
	}
	r, _ := tierSweepTestReader(t, lines...)
	ts := r.read(time.Date(2026, 10, 3, 11, 0, 0, 0, time.Local))
	if len(ts.Trends) != 1 {
		t.Fatalf("trends = %+v, want one rig", ts.Trends)
	}
	got := ts.Trends[0].Sweeps
	if len(got) != tierSweepTrendPerRig {
		t.Fatalf("kept %d sweeps, want %d", len(got), tierSweepTrendPerRig)
	}
	if got[0].SHA != "00000006" || got[len(got)-1].SHA != "00000023" {
		t.Errorf("kept %s..%s, want the newest 30 (00000006..00000023)", got[0].SHA, got[len(got)-1].SHA)
	}
	if !got[0].At.Before(got[len(got)-1].At) {
		t.Error("the trend is not oldest first")
	}
}
