package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/landing"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/lock"
)

func TestJevLandingPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_landing") {
		t.Error("jev_landing must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_landing") {
		t.Error("jev_landing must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevLanding: &JevLandingConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_landing") {
		t.Error("an explicit enabled=false must keep jev_landing off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevLanding: &JevLandingConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_landing") {
		t.Error("an explicit enabled=true must turn jev_landing on")
	}
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// so a later enable is due at once, and it logs nothing.
func TestJevLandingTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevLanding()

	if d.jevLandingRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_landing"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and says why, without
// reaching Jev, git or the rig's beads.
func TestJevLandingRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevLanding: &JevLandingConfig{Enabled: true}},
		},
	}

	d.runJevLanding()

	if !strings.Contains(logs.String(), "jev_landing: error:") ||
		!strings.Contains(logs.String(), "price per billion tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

func TestJevLandingConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_landing":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevLanding
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_landing = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// stubLandings lists the landings a pass reads.
type stubLandings struct{ recs []land.LandingRecord }

func (s stubLandings) Since(time.Time) ([]land.LandingRecord, error) { return s.recs, nil }

// stubLandingBeads and stubLandingGit read a placeholder bead and an empty
// diff: the daemon test is about when a pass runs and what it logs, not about
// the state it builds.
type stubLandingBeads struct{}

func (stubLandingBeads) Show(id string) (*beads.Issue, error) {
	return &beads.Issue{ID: id, Title: "stub " + id}, nil
}

type stubLandingGit struct{}

func (stubLandingGit) Numstat(context.Context, string, string) (string, error) { return "", nil }

// countingAsker answers every Ask and counts them.
type countingAsker struct{ n int }

func (c *countingAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	c.n++
	return &jev.Answers{
		Model: "jev-1.13.0",
		Noul: map[string]float64{
			landing.QNeedsReviewNow: 0.5, landing.QTouchesTownMachinery: 0.5,
			landing.QBeyondGoal: 0.5, landing.QTestsCoverChange: 0.5,
		},
		Score: map[string]jev.ScoreAnswer{landing.QRisk: {Score: 1, Confidence: 0.5}},
	}, nil
}

// jevLandingRig is an enabled daemon over a temp town whose passes open
// through a fake, so no pass reaches Jev, git or the rig's beads.
type jevLandingRig struct {
	d      *Daemon
	clock  *clockwork.FakeClock
	town   string
	logs   *bytes.Buffer
	opened int
	got    landing.Settings
	pass   *landing.Pass
	asker  *countingAsker
}

func newJevLandingRig(t *testing.T) *jevLandingRig {
	t.Helper()
	town := t.TempDir()
	r := &jevLandingRig{
		town: town, logs: &bytes.Buffer{}, asker: &countingAsker{},
		clock: clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)),
	}
	r.d = &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(r.logs, "", 0),
		clock:  r.clock,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevLanding: &JevLandingConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	// The cap is the real one, read from the usage ledger the test writes.
	dayCap := battery.Cap{Ledger: jev.UsageLedgerPath(town), MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}
	r.pass = &landing.Pass{
		Asker:      r.asker,
		Landings:   stubLandings{},
		Beads:      stubLandingBeads{},
		Git:        stubLandingGit{},
		LogPath:    landing.LogPath(town),
		CursorPath: landing.CursorPath(town),
		Allows:     func() (bool, error) { return dayCap.Allows(r.clock.Now) },
		Now:        func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
	r.d.jevLandingOpen = func(_ string, s landing.Settings, _ io.Writer) (*landing.Pass, error) {
		r.opened++
		r.got = s
		return r.pass, nil
	}
	return r
}

// setSpendToday overwrites the usage ledger with one record of today's spend:
// inputTokens input tokens, which the rig prices at $3 per billion.
func (r *jevLandingRig) setSpendToday(t *testing.T, inputTokens int) {
	t.Helper()
	path := jev.UsageLedgerPath(r.town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := fmt.Sprintf(`{"ts":%q,"caller":"jev-dedupe","input_tokens":%d,"output_tokens":0}`,
		r.clock.Now().Add(-time.Hour).Format(time.RFC3339), inputTokens)
	if err := os.WriteFile(path, []byte(rec+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedCursor starts the pass's cursor so its next run reads landings.
func (r *jevLandingRig) seedCursor(t *testing.T, at time.Time) {
	t.Helper()
	if err := battery.WriteCursor(r.pass.CursorPath, battery.Cursor{Time: at}); err != nil {
		t.Fatal(err)
	}
}

func waitJevLandingIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevLandingRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_landing pass did not finish")
		}
		runtime.Gosched()
	}
}

// With the patrol enabled and due, the trigger runs one pass on its own
// goroutine with the configured settings, records the run, and then is not due
// again until the 15-minute interval passes.
func TestJevLandingTriggerRunsOnePassWhenEnabledAndDue(t *testing.T) {
	t.Parallel()
	r := newJevLandingRig(t)

	r.d.triggerJevLanding()
	waitJevLandingIdle(t, r.d)

	if r.opened != 1 {
		t.Fatalf("pass opened %d times, want 1", r.opened)
	}
	if want := (landing.Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}); r.got != want {
		t.Errorf("settings = %+v, want %+v from daemon.json", r.got, want)
	}
	if _, err := os.Stat(landing.CursorPath(r.town)); err != nil {
		t.Errorf("the first pass left no cursor: %v", err)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_landing"); !found {
		t.Error("no last-run record after the pass")
	}

	if jevLandingInterval != 15*time.Minute {
		t.Fatalf("interval = %s, want 15m", jevLandingInterval)
	}
	r.clock.Advance(jevLandingInterval - 2*time.Minute)
	r.d.triggerJevLanding()
	waitJevLandingIdle(t, r.d)
	if r.opened != 1 {
		t.Errorf("a trigger inside the interval opened a pass (%d opens)", r.opened)
	}

	r.clock.Advance(2 * time.Minute)
	r.d.triggerJevLanding()
	waitJevLandingIdle(t, r.d)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after the interval, want 2", r.opened)
	}
}

// A trigger that arrives while a pass is in flight is turned away.
func TestJevLandingTriggerSkipsWhileAPassIsRunning(t *testing.T) {
	t.Parallel()
	r := newJevLandingRig(t)
	r.d.jevLandingRunning.Store(true)

	r.d.triggerJevLanding()

	if r.opened != 0 {
		t.Errorf("a trigger opened a pass while one was in flight")
	}
	if !r.d.jevLandingRunning.Load() {
		t.Error("the trigger released a guard it did not take")
	}
}

// An operator-disabled patrol stays off even with the config on.
func TestJevLandingTriggerHonorsDisabledPatrols(t *testing.T) {
	t.Parallel()
	r := newJevLandingRig(t)
	r.d.disabledPatrols = map[string]bool{"jev_landing": true}

	r.d.triggerJevLanding()

	if r.opened != 0 {
		t.Error("a disabled patrol opened a pass")
	}
}

func riskLanding(bead string, at time.Time) land.LandingRecord {
	return land.LandingRecord{
		BeadID: bead, Rig: "gastown", Head: "h-" + bead, Base: "b-" + bead,
		LandedAt: at, RiskPaths: []string{"internal/daemon/x.go"},
	}
}

// A pass that reaches a landing logs one line for it and says so.
func TestJevLandingRunReportsTheLandingsAsked(t *testing.T) {
	t.Parallel()
	r := newJevLandingRig(t)
	r.seedCursor(t, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	r.pass.Landings = stubLandings{recs: []land.LandingRecord{
		riskLanding("gt-new", time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)),
	}}

	r.d.runJevLanding()

	if !strings.Contains(r.logs.String(), "jev_landing: asked 1 landings") {
		t.Errorf("log = %q, want the asked count", r.logs)
	}
	lines, err := landing.ReadRecent(r.pass.LogPath, 5)
	if err != nil || len(lines) != 1 || lines[0].Bead != "gt-new" {
		t.Errorf("log lines = %+v, %v, want one line for gt-new", lines, err)
	}
}

// When the cap check says the day's spend is used up, the pass asks nothing,
// logs nothing and says the cap was reached; the landings wait for a later
// pass.
func TestJevLandingRunStopsForTheDayWhenTheCapSaysSo(t *testing.T) {
	t.Parallel()
	r := newJevLandingRig(t)
	r.seedCursor(t, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	r.pass.Landings = stubLandings{recs: []land.LandingRecord{
		riskLanding("gt-new", time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)),
	}}
	r.setSpendToday(t, 1_000_000_000) // $3 spent against the $2.5 cap

	r.d.runJevLanding()

	if !strings.Contains(r.logs.String(), "asked 0 landings") || !strings.Contains(r.logs.String(), "daily cap reached: true") {
		t.Errorf("log = %q, want the cap reached", r.logs)
	}
	if r.asker.n != 0 {
		t.Errorf("a capped pass made %d Jev calls", r.asker.n)
	}
	if _, err := os.Stat(r.pass.LogPath); !os.IsNotExist(err) {
		t.Errorf("a capped pass wrote a log: %v", err)
	}

	r.setSpendToday(t, 0)
	r.d.runJevLanding()
	if r.asker.n != 1 {
		t.Errorf("the pass after the cap reopened made %d Jev calls, want 1", r.asker.n)
	}
}

// A manual pass holding the run lock makes the daemon's pass step aside with a
// line, not an error, and the run still advances the last-run time.
func TestJevLandingRunStepsAsideForAManualPass(t *testing.T) {
	t.Parallel()
	r := newJevLandingRig(t)
	if err := os.MkdirAll(filepath.Dir(r.pass.CursorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	release, ok, err := lock.FlockTryAcquire(r.pass.CursorPath + ".lock")
	if err != nil || !ok {
		t.Fatalf("taking the run lock: ok=%v err=%v", ok, err)
	}
	defer release()

	r.d.triggerJevLanding()
	waitJevLandingIdle(t, r.d)

	if !strings.Contains(r.logs.String(), "jev_landing: skipped:") || strings.Contains(r.logs.String(), "error") {
		t.Errorf("log = %q, want a skipped line and no error", r.logs)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_landing"); !found {
		t.Error("no last-run record after a skipped pass")
	}
}

// An open that fails (no price, no beads directory) is logged once per
// interval, not once per heartbeat.
func TestJevLandingTriggerLogsAFailedOpenOncePerInterval(t *testing.T) {
	t.Parallel()
	r := newJevLandingRig(t)
	r.d.jevLandingOpen = func(string, landing.Settings, io.Writer) (*landing.Pass, error) {
		r.opened++
		return nil, errors.New("no beads directory for rig gastown")
	}

	r.d.triggerJevLanding()
	waitJevLandingIdle(t, r.d)
	r.d.triggerJevLanding()
	waitJevLandingIdle(t, r.d)

	if r.opened != 1 {
		t.Errorf("pass opened %d times in one interval, want 1", r.opened)
	}
	if n := strings.Count(r.logs.String(), "jev_landing: error: no beads directory"); n != 1 {
		t.Errorf("error logged %d times, want 1: %q", n, r.logs)
	}
}

// TestJevLandingPassStopsBetweenCallsWhenEstopped covers gt-tevxz: the E-stop
// kept a pass from starting, but one already in flight went on paying for every
// landing it reached. The call in flight finishes and is recorded; the landings
// behind it wait for a pass run after the E-stop lifts.
func TestJevLandingPassStopsBetweenCallsWhenEstopped(t *testing.T) {
	t.Parallel()
	r := newJevLandingRig(t)
	r.seedCursor(t, time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	r.pass.Landings = stubLandings{recs: []land.LandingRecord{
		riskLanding("gt-a", time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)),
		riskLanding("gt-b", time.Date(2026, 10, 20, 11, 1, 0, 0, time.UTC)),
		riskLanding("gt-c", time.Date(2026, 10, 20, 11, 2, 0, 0, time.UTC)),
	}}
	asker := &estoppingAsker{t: t, town: r.town}
	r.pass.Asker = asker

	r.d.runJevLanding()

	if asker.calls != 1 {
		t.Errorf("Jev was asked about %d landings, want only the one in flight when the E-stop went up", asker.calls)
	}
	lines, err := landing.ReadRecent(r.pass.LogPath, 5)
	if err != nil || len(lines) != 1 || lines[0].Bead != "gt-a" {
		t.Errorf("log lines = %+v, %v, want the one gt-a line recorded", lines, err)
	}
	if !strings.Contains(r.logs.String(), "jev_landing: stopped:") {
		t.Errorf("log = %q, want the pass to say it stopped for the E-stop", r.logs)
	}
}
