package daemon

import (
	"bytes"
	"context"
	"encoding/json"
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
	jevdispatch "github.com/steveyegge/gastown/internal/jev/dispatch"
)

func TestJevDispatchPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_dispatch") {
		t.Error("jev_dispatch must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_dispatch") {
		t.Error("jev_dispatch must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDispatch: &JevDispatchConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_dispatch") {
		t.Error("an explicit enabled=false must keep jev_dispatch off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDispatch: &JevDispatchConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_dispatch") {
		t.Error("an explicit enabled=true must turn jev_dispatch on")
	}
	// Turning the dedupe patrol on leaves the dispatch patrol off.
	dedupeOnly := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDedupe: &JevDedupeConfig{Enabled: true}}}
	if IsPatrolEnabled(dedupeOnly, "jev_dispatch") {
		t.Error("enabling jev_dedupe must not enable jev_dispatch")
	}
}

func TestJevDispatchIntervalIsFifteenMinutes(t *testing.T) {
	t.Parallel()
	if jevDispatchInterval != 15*time.Minute {
		t.Errorf("jevDispatchInterval = %v, want 15m", jevDispatchInterval)
	}
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// so a later enable is due at once, and it logs nothing.
func TestJevDispatchTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevDispatch()

	if d.jevDispatchRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_dispatch"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and says why, without
// reaching Jev or the rig's beads.
func TestJevDispatchRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevDispatch: &JevDispatchConfig{Enabled: true}},
		},
	}

	d.runJevDispatch()

	if !strings.Contains(logs.String(), "jev_dispatch: error:") ||
		!strings.Contains(logs.String(), "usd_per_billion_tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

func TestJevDispatchConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_dispatch":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevDispatch
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_dispatch = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// stubPlaced lists the beads a pass reads; the pass only ever reads them.
type stubPlaced struct{ all []*beads.Issue }

func (s stubPlaced) Placed(time.Time) ([]*beads.Issue, error) { return s.all, nil }

// dispatchCountingAsker answers every battery the same way and counts the calls.
type dispatchCountingAsker struct{ calls int }

func (c *dispatchCountingAsker) Ask(_ context.Context, _ any, _ map[string]jev.Question) (*jev.Answers, error) {
	c.calls++
	return &jev.Answers{
		Model: "jev-test-1",
		Noul: map[string]float64{
			"lands_first_try": 0.9, "fits_twenty_minutes": 0.8, "acceptance_tickable": 0.7,
			"needs_strong_model": 0.2, "touches_daemon": 0.1,
		},
		Score: map[string]jev.ScoreAnswer{"size": {Score: 1, Confidence: 0.5}},
	}, nil
}

// jevDispatchRig is an enabled daemon over a temp town whose passes open
// through a fake, so no pass reaches Jev or the rig's beads.
type jevDispatchRig struct {
	d      *Daemon
	clock  *clockwork.FakeClock
	town   string
	logs   *bytes.Buffer
	opened int
	got    jevdispatch.Settings
	pass   *jevdispatch.Pass
	asker  *dispatchCountingAsker
}

func newJevDispatchRig(t *testing.T) *jevDispatchRig {
	t.Helper()
	town := t.TempDir()
	r := &jevDispatchRig{
		town:  town,
		logs:  &bytes.Buffer{},
		clock: clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)),
		asker: &dispatchCountingAsker{},
	}
	r.d = &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(r.logs, "", 0),
		clock:  r.clock,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDispatch: &JevDispatchConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	r.pass = &jevdispatch.Pass{
		Asker:      r.asker,
		Lister:     stubPlaced{},
		LogPath:    jevdispatch.LogPath(town),
		CursorPath: jevdispatch.CursorPath(town),
		RetryPath:  jevdispatch.RetryPath(town),
		SpentToday: func() (float64, error) { return 0, nil },
		CapUSD:     1,
		Now:        func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
	r.d.jevDispatchOpen = func(_ string, s jevdispatch.Settings, _ io.Writer) (*jevdispatch.Pass, error) {
		r.opened++
		r.got = s
		return r.pass, nil
	}
	return r
}

// placeBead puts one bead on a seat at 11:00, after a cursor written for 10:00.
func (r *jevDispatchRig) placeBead(t *testing.T) {
	t.Helper()
	r.pass.Lister = stubPlaced{all: []*beads.Issue{{
		ID: "gt-new", Title: "Do a thing", Status: "in_progress",
		Description: "attached_at: 2026-10-20T11:00:00Z\n\n## Goal\n\nDo a thing.\n",
	}}}
	if err := battery.WriteCursor(r.pass.CursorPath, battery.Cursor{Time: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
}

func waitJevDispatchIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevDispatchRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_dispatch pass did not finish")
		}
		runtime.Gosched()
	}
}

// With the patrol enabled and due, the trigger runs one pass on its own
// goroutine with the configured settings, records the run, and then is not due
// again until fifteen minutes pass.
func TestJevDispatchTriggerRunsOnePassWhenEnabledAndDue(t *testing.T) {
	t.Parallel()
	r := newJevDispatchRig(t)

	r.d.triggerJevDispatch()
	waitJevDispatchIdle(t, r.d)

	if r.opened != 1 {
		t.Fatalf("pass opened %d times, want 1", r.opened)
	}
	if want := (jevdispatch.Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}); r.got != want {
		t.Errorf("settings = %+v, want %+v from daemon.json", r.got, want)
	}
	if _, err := os.Stat(jevdispatch.CursorPath(r.town)); err != nil {
		t.Errorf("the first pass left no cursor: %v", err)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_dispatch"); !found {
		t.Error("no last-run record after the pass")
	}

	r.clock.Advance(14 * time.Minute)
	r.d.triggerJevDispatch()
	waitJevDispatchIdle(t, r.d)
	if r.opened != 1 {
		t.Errorf("a second trigger 14 minutes on opened a pass (%d opens)", r.opened)
	}

	r.clock.Advance(time.Minute)
	r.d.triggerJevDispatch()
	waitJevDispatchIdle(t, r.d)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after 15 minutes, want 2", r.opened)
	}
}

// A trigger that arrives while a pass is in flight is turned away.
func TestJevDispatchTriggerSkipsWhileAPassIsRunning(t *testing.T) {
	t.Parallel()
	r := newJevDispatchRig(t)
	r.d.jevDispatchRunning.Store(true)

	r.d.triggerJevDispatch()

	if r.opened != 0 {
		t.Errorf("a trigger opened a pass while one was in flight")
	}
	if !r.d.jevDispatchRunning.Load() {
		t.Error("the trigger released a guard it did not take")
	}
}

// An operator-disabled patrol stays off even with the config on.
func TestJevDispatchTriggerHonorsDisabledPatrols(t *testing.T) {
	t.Parallel()
	r := newJevDispatchRig(t)
	r.d.disabledPatrols = map[string]bool{"jev_dispatch": true}

	r.d.triggerJevDispatch()

	if r.opened != 0 {
		t.Error("a disabled patrol opened a pass")
	}
}

// A pass that reaches a placed bead logs one line for it and says so.
func TestJevDispatchRunReportsTheBeadsAsked(t *testing.T) {
	t.Parallel()
	r := newJevDispatchRig(t)
	r.placeBead(t)

	r.d.runJevDispatch()

	if !strings.Contains(r.logs.String(), "jev_dispatch: asked 1 beads, 0 failed") {
		t.Errorf("log = %q, want the asked count", r.logs)
	}
	lines, err := jevdispatch.ReadRecent(r.pass.LogPath, 5)
	if err != nil || len(lines) != 1 || lines[0].Bead != "gt-new" || lines[0].Model != "jev-test-1" {
		t.Errorf("log lines = %+v, %v, want one line for gt-new", lines, err)
	}
}

// A question Jev cannot answer is reported with its cause: the log line never
// carries one, so the daemon log is where a swallowed failure shows.
func TestJevDispatchRunLogsWhyAQuestionFailed(t *testing.T) {
	t.Parallel()
	r := newJevDispatchRig(t)
	r.placeBead(t)
	r.pass.Asker = dispatchFailingAsker{err: &jev.StatusError{Status: 413, Message: "state too large"}}

	r.d.runJevDispatch()

	if !strings.Contains(r.logs.String(), "gt-new failed, retrying next pass") ||
		!strings.Contains(r.logs.String(), "state too large") {
		t.Errorf("log = %q, want the bead and the cause", r.logs)
	}
}

// dispatchFailingAsker fails every battery with one error.
type dispatchFailingAsker struct{ err error }

func (f dispatchFailingAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	return nil, f.err
}

// The pass is under the shared cap: another battery's spend today, read from
// the one usage ledger, stops it before it asks anything.
func TestJevDispatchRunStopsUnderTheSharedCap(t *testing.T) {
	t.Parallel()
	r := newJevDispatchRig(t)
	r.placeBead(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ledger := jev.UsageLedgerPath(r.town)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o700); err != nil {
		t.Fatal(err)
	}
	rec, err := json.Marshal(jev.LedgerRecord{TS: now.Add(-time.Second), Caller: "jev-dedupe", InputTokens: 2_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, append(rec, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	town := battery.Cap{Ledger: ledger, MaxUSDPerDay: 1, USDPerBillionTokens: 1}
	r.pass.SpentToday = func() (float64, error) { return town.Spent(func() time.Time { return now }) }

	r.d.runJevDispatch()

	if r.asker.calls != 0 {
		t.Errorf("the pass asked %d times with the cap spent by another caller", r.asker.calls)
	}
	if !strings.Contains(r.logs.String(), "daily cap reached: true") {
		t.Errorf("log = %q, want the cap reported", r.logs)
	}
	if lines, _ := jevdispatch.ReadRecent(r.pass.LogPath, 5); len(lines) != 0 {
		t.Errorf("log lines = %+v, want none", lines)
	}
}

// An open that fails (no price, no beads directory) is logged once per
// interval, not once per heartbeat.
func TestJevDispatchTriggerLogsAFailedOpenOncePerInterval(t *testing.T) {
	t.Parallel()
	r := newJevDispatchRig(t)
	r.d.jevDispatchOpen = func(string, jevdispatch.Settings, io.Writer) (*jevdispatch.Pass, error) {
		r.opened++
		return nil, io.ErrUnexpectedEOF
	}

	r.d.triggerJevDispatch()
	waitJevDispatchIdle(t, r.d)
	r.d.triggerJevDispatch()
	waitJevDispatchIdle(t, r.d)

	if r.opened != 1 {
		t.Errorf("pass opened %d times in one interval, want 1", r.opened)
	}
	if n := strings.Count(r.logs.String(), "jev_dispatch: error:"); n != 1 {
		t.Errorf("error logged %d times, want 1: %q", n, r.logs)
	}
}

// The patrol sits in the daemon's heartbeat steps, under the E-stop like the other
// paid Jev work.
func TestJevDispatchIsALifecycleHeartbeatStep(t *testing.T) {
	t.Parallel()
	for _, j := range heartbeatSteps {
		if j.name == "jev-dispatch" {
			if !j.lifecycle {
				t.Error("jev-dispatch must be a lifecycle job, so the town E-stop holds its paid calls")
			}
			return
		}
	}
	t.Error("jev-dispatch is not in the daemon's heartbeat steps")
}
