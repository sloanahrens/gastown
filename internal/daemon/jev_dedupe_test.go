package daemon

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
	"github.com/steveyegge/gastown/internal/lock"
)

func TestJevDedupePatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_dedupe") {
		t.Error("jev_dedupe must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_dedupe") {
		t.Error("jev_dedupe must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDedupe: &JevDedupeConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_dedupe") {
		t.Error("an explicit enabled=false must keep jev_dedupe off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDedupe: &JevDedupeConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_dedupe") {
		t.Error("an explicit enabled=true must turn jev_dedupe on")
	}
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// so a later enable is due at once, and it logs nothing.
func TestJevDedupeTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevDedupe()

	if d.jevDedupeRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_dedupe"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and says why, without
// reaching Jev or hm.
func TestJevDedupeRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevDedupe: &JevDedupeConfig{Enabled: true}},
		},
	}

	d.runJevDedupe()

	if !strings.Contains(logs.String(), "jev_dedupe: error:") ||
		!strings.Contains(logs.String(), "usd_per_billion_tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

func TestJevDedupeConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_dedupe":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevDedupe
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_dedupe = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// stubDedupeBeads lists the beads a pass reads; the pass only ever reads them.
type stubDedupeBeads struct{ all []*beads.Issue }

func (s stubDedupeBeads) Since(time.Time) ([]*beads.Issue, error) { return s.all, nil }
func (s stubDedupeBeads) Show(id string) (*beads.Issue, error) {
	return nil, errors.New("no bead " + id)
}

// noHits is an hm that finds nothing, so a pass logs its finding without
// asking Jev.
type noHits struct{}

func (noHits) Search(context.Context, string) ([]dedupe.Hit, error) { return nil, nil }

// failAsker fails the test if a pass reaches Jev.
type failAsker struct{ t *testing.T }

func (f failAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	f.t.Error("the daemon test reached Jev")
	return nil, errors.New("unexpected Jev call")
}

// batteryAsker answers every question asked, so a pass logs its line without
// Jev.
type batteryAsker struct{}

func (batteryAsker) Ask(_ context.Context, _ any, qs map[string]jev.Question) (*jev.Answers, error) {
	out := &jev.Answers{
		Model:  "jev-1.13.0",
		Choice: map[string]jev.ChoiceAnswer{},
		Noul:   map[string]float64{},
		Score:  map[string]jev.ScoreAnswer{},
	}
	for id, q := range qs {
		switch q.Type {
		case "choice":
			out.Choice[id] = jev.ChoiceAnswer{Choice: "none", Confidence: 0.9}
		case "noul":
			out.Noul[id] = 0.5
		case "score":
			out.Score[id] = jev.ScoreAnswer{Score: 1, Confidence: 0.5}
		}
	}
	return out, nil
}

// jevDedupeRig is an enabled daemon over a temp town whose passes open through
// a fake, so no pass reaches Jev, hm or the rig's beads.
type jevDedupeRig struct {
	d      *Daemon
	clock  *clockwork.FakeClock
	town   string
	logs   *bytes.Buffer
	opened int
	got    dedupe.Settings
	pass   *dedupe.Pass
}

func newJevDedupeRig(t *testing.T) *jevDedupeRig {
	t.Helper()
	town := t.TempDir()
	r := &jevDedupeRig{town: town, logs: &bytes.Buffer{}, clock: clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))}
	r.d = &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(r.logs, "", 0),
		clock:  r.clock,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDedupe: &JevDedupeConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	r.pass = &dedupe.Pass{
		Asker:      failAsker{t},
		Searcher:   noHits{},
		Beads:      stubDedupeBeads{},
		LogPath:    dedupe.LogPath(town),
		CursorPath: dedupe.CursorPath(town),
		Allows:     func() (bool, error) { return true, nil },
		Now:        func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
	r.d.jevDedupeOpen = func(_ string, s dedupe.Settings, _ io.Writer) (*dedupe.Pass, error) {
		r.opened++
		r.got = s
		return r.pass, nil
	}
	return r
}

func waitJevDedupeIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevDedupeRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_dedupe pass did not finish")
		}
		runtime.Gosched()
	}
}

// With the patrol enabled and due, the trigger runs one pass on its own
// goroutine with the configured settings, records the run, and then is not due
// again until the interval passes.
func TestJevDedupeTriggerRunsOnePassWhenEnabledAndDue(t *testing.T) {
	t.Parallel()
	r := newJevDedupeRig(t)

	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)

	if r.opened != 1 {
		t.Fatalf("pass opened %d times, want 1", r.opened)
	}
	if want := (dedupe.Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}); r.got != want {
		t.Errorf("settings = %+v, want %+v from daemon.json", r.got, want)
	}
	if _, err := os.Stat(dedupe.CursorPath(r.town)); err != nil {
		t.Errorf("the first pass left no cursor: %v", err)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_dedupe"); !found {
		t.Error("no last-run record after the pass")
	}

	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)
	if r.opened != 1 {
		t.Errorf("a second trigger inside the interval opened a pass (%d opens)", r.opened)
	}

	r.clock.Advance(jevDedupeInterval)
	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after the interval, want 2", r.opened)
	}
}

// A last-run save that fails still suppresses the next heartbeat's rerun for
// the interval. With no record on disk every check read the patrol as due, so a
// town whose daemon directory cannot be written opened a pass per heartbeat —
// each one reading the beads and reaching Jev for the findings its cursor had
// not paid for yet (gt-4nqej).
func TestJevDedupeFailedSaveSuppressesTheRerun(t *testing.T) {
	t.Parallel()
	r := newJevDedupeRig(t)
	// The last-run record cannot be written: its directory is a regular file.
	if err := os.WriteFile(filepath.Join(r.town, "daemon"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)
	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)

	if r.opened != 1 {
		t.Errorf("pass opened %d times inside one interval after a failed save, want 1", r.opened)
	}
	if !strings.Contains(r.logs.String(), "cannot persist last-run time") {
		t.Errorf("log = %q, want the failed save named", r.logs)
	}

	r.clock.Advance(jevDedupeInterval)
	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after the interval, want 2", r.opened)
	}
}

// A failed save plus a restart reruns the pass inside the interval: the
// in-memory time that held the interval died with the process, and the disk has
// no record to stand in for it. That is the intended direction for a patrol
// (recordJevDedupeRun) — a missing record reads as "run the pass" — so the
// test pins it rather than leave it as a comment.
func TestJevDedupeFailedSaveThenRestartRerunsThePass(t *testing.T) {
	t.Parallel()
	r := newJevDedupeRig(t)
	if err := os.WriteFile(filepath.Join(r.town, "daemon"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)
	if r.opened != 1 {
		t.Fatalf("pass opened %d times, want 1", r.opened)
	}

	// A new process over the same town and clock: nothing in memory, and the
	// failed save left nothing on disk.
	restarted := &Daemon{
		config:        r.d.config,
		logger:        r.d.logger,
		clock:         r.clock,
		patrolConfig:  r.d.patrolConfig,
		jevDedupeOpen: r.d.jevDedupeOpen,
	}
	restarted.triggerJevDedupe()
	waitJevDedupeIdle(t, restarted)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after the restart, want 2 — no record survived the failed save", r.opened)
	}
}

// A trigger that arrives while a pass is in flight is turned away.
func TestJevDedupeTriggerSkipsWhileAPassIsRunning(t *testing.T) {
	t.Parallel()
	r := newJevDedupeRig(t)
	r.d.jevDedupeRunning.Store(true)

	r.d.triggerJevDedupe()

	if r.opened != 0 {
		t.Errorf("a trigger opened a pass while one was in flight")
	}
	if !r.d.jevDedupeRunning.Load() {
		t.Error("the trigger released a guard it did not take")
	}
}

// An operator-disabled patrol stays off even with the config on.
func TestJevDedupeTriggerHonorsDisabledPatrols(t *testing.T) {
	t.Parallel()
	r := newJevDedupeRig(t)
	r.d.disabledPatrols = map[string]bool{"jev_dedupe": true}

	r.d.triggerJevDedupe()

	if r.opened != 0 {
		t.Error("a disabled patrol opened a pass")
	}
}

// A pass that reaches a finding logs one line for it and says so, with the
// battery logged even when the search offers no candidate.
func TestJevDedupeRunReportsTheFindingsAsked(t *testing.T) {
	t.Parallel()
	r := newJevDedupeRig(t)
	r.pass.Asker = batteryAsker{}
	created := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	r.pass.Beads = stubDedupeBeads{all: []*beads.Issue{{
		ID: "gt-new", Title: "Found: something", Status: "open", CreatedAt: created.Format(time.RFC3339),
	}}}
	cursor := `{"time":"2026-10-20T10:00:00Z"}`
	if err := os.MkdirAll(filepath.Dir(r.pass.CursorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.pass.CursorPath, []byte(cursor), 0o600); err != nil {
		t.Fatal(err)
	}

	r.d.runJevDedupe()

	if !strings.Contains(r.logs.String(), "jev_dedupe: asked 1 findings") {
		t.Errorf("log = %q, want the asked count", r.logs)
	}
	lines, err := dedupe.ReadRecent(r.pass.LogPath, 5)
	if err != nil || len(lines) != 1 || lines[0].Finding != "gt-new" || lines[0].Outcome != dedupe.OutcomeNoCandidates {
		t.Errorf("log lines = %+v, %v, want one no_candidates line for gt-new", lines, err)
	}
}

// A manual pass holding the run lock makes the daemon's pass step aside with a
// line, not an error, and the run still advances the last-run time.
func TestJevDedupeRunStepsAsideForAManualPass(t *testing.T) {
	t.Parallel()
	r := newJevDedupeRig(t)
	if err := os.MkdirAll(filepath.Dir(r.pass.CursorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	release, ok, err := lock.FlockTryAcquire(r.pass.CursorPath + ".lock")
	if err != nil || !ok {
		t.Fatalf("taking the run lock: ok=%v err=%v", ok, err)
	}
	defer release()

	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)

	if !strings.Contains(r.logs.String(), "jev_dedupe: skipped:") || strings.Contains(r.logs.String(), "error") {
		t.Errorf("log = %q, want a skipped line and no error", r.logs)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_dedupe"); !found {
		t.Error("no last-run record after a skipped pass")
	}
}

// An open that fails (no price, no beads directory) is logged once per
// interval, not once per heartbeat.
func TestJevDedupeTriggerLogsAFailedOpenOncePerInterval(t *testing.T) {
	t.Parallel()
	r := newJevDedupeRig(t)
	r.d.jevDedupeOpen = func(string, dedupe.Settings, io.Writer) (*dedupe.Pass, error) {
		r.opened++
		return nil, errors.New("no beads directory for rig gastown")
	}

	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)
	r.d.triggerJevDedupe()
	waitJevDedupeIdle(t, r.d)

	if r.opened != 1 {
		t.Errorf("pass opened %d times in one interval, want 1", r.opened)
	}
	if n := strings.Count(r.logs.String(), "jev_dedupe: error: no beads directory"); n != 1 {
		t.Errorf("error logged %d times, want 1: %q", n, r.logs)
	}
}

// estoppingAsker answers every question and raises the town E-stop during its
// first call, standing in for an operator hitting the switch while a pass is
// billing findings.
type estoppingAsker struct {
	t     *testing.T
	town  string
	calls int
}

func (a *estoppingAsker) Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	a.calls++
	if a.calls == 1 {
		if err := estop.Activate(a.town, estop.TriggerManual, "test E-stop mid-pass"); err != nil {
			a.t.Fatalf("raise the E-stop: %v", err)
		}
	}
	return (batteryAsker{}).Ask(ctx, state, qs)
}

// TestJevDedupePassStopsBetweenCallsWhenEstopped covers gt-0h4i0: the E-stop
// kept a pass from starting, but one already in flight went on paying for every
// finding it reached. The pass now checks the E-stop between billed calls: the
// call in flight finishes and is recorded, and the findings behind it wait for a
// pass run after the E-stop lifts.
func TestJevDedupePassStopsBetweenCallsWhenEstopped(t *testing.T) {
	t.Parallel()
	r := newJevDedupeRig(t)
	created := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	var all []*beads.Issue
	for i, id := range []string{"gt-a", "gt-b", "gt-c"} {
		all = append(all, &beads.Issue{
			ID: id, Title: "Found: something", Status: "open",
			CreatedAt: created.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		})
	}
	r.pass.Beads = stubDedupeBeads{all: all}
	asker := &estoppingAsker{t: t, town: r.town}
	r.pass.Asker = asker
	if err := os.MkdirAll(filepath.Dir(r.pass.CursorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	cursor := `{"time":"2026-10-20T10:00:00Z"}`
	if err := os.WriteFile(r.pass.CursorPath, []byte(cursor), 0o600); err != nil {
		t.Fatal(err)
	}

	r.d.runJevDedupe()

	if asker.calls != 1 {
		t.Errorf("Jev was asked about %d findings, want only the one in flight when the E-stop went up", asker.calls)
	}
	lines, err := dedupe.ReadRecent(r.pass.LogPath, 5)
	if err != nil || len(lines) != 1 || lines[0].Finding != "gt-a" {
		t.Errorf("log lines = %+v, %v, want the one gt-a line recorded", lines, err)
	}
	if !strings.Contains(r.logs.String(), "jev_dedupe: stopped:") {
		t.Errorf("log = %q, want the pass to say it stopped for the E-stop", r.logs)
	}
}

// gateCountingAsker counts the calls that reach it.
type gateCountingAsker struct{ calls int }

func (a *gateCountingAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	a.calls++
	return &jev.Answers{}, nil
}

// Once the gate has seen the E-stop it refuses every later call, so a Run that
// ignores the cancelled context cannot bill again if the E-stop lifts mid-pass.
func TestEstopGateStaysStoppedWhenTheEstopLifts(t *testing.T) {
	t.Parallel()
	inner := &gateCountingAsker{}
	up := false
	cancelled := 0
	gate := &estopGatedAsker{
		inner:  inner,
		cancel: func() { cancelled++ },
		active: func() bool { return up },
	}

	if _, err := gate.Ask(context.Background(), nil, nil); err != nil {
		t.Fatalf("Ask with no E-stop: %v", err)
	}
	up = true
	if _, err := gate.Ask(context.Background(), nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask under the E-stop = %v, want context.Canceled", err)
	}
	up = false
	if _, err := gate.Ask(context.Background(), nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Ask after the E-stop lifted = %v, want context.Canceled", err)
	}
	if !gate.stopped {
		t.Error("gate.stopped = false after the E-stop")
	}
	if inner.calls != 1 || cancelled != 1 {
		t.Errorf("inner calls = %d, cancels = %d, want 1 and 1", inner.calls, cancelled)
	}
}
