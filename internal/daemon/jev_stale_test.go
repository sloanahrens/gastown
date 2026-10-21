package daemon

import (
	"bytes"
	"context"
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
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/stale"
)

func TestJevStalePatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_stale") {
		t.Error("jev_stale must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_stale") {
		t.Error("jev_stale must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevStale: &JevStaleConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_stale") {
		t.Error("an explicit enabled=false must keep jev_stale off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevStale: &JevStaleConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_stale") {
		t.Error("an explicit enabled=true must turn jev_stale on")
	}
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// so a later enable is due at once, and it logs nothing.
func TestJevStaleTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevStale()

	if d.jevStaleRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_stale"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled read with no price refuses to run and says why, without reaching
// Jev.
func TestJevStaleRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevStale: &JevStaleConfig{Enabled: true}},
		},
	}

	d.runJevStale()

	if !strings.Contains(logs.String(), "jev_stale: error:") ||
		!strings.Contains(logs.String(), "usd_per_billion_tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

func TestJevStaleConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_stale":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevStale
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_stale = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// staleAsker answers every bead the same way and counts the calls it was made.
type staleAsker struct {
	calls int
	// onAsk, when set, runs before each answer.
	onAsk func()
}

func (a *staleAsker) Ask(_ context.Context, _ any, _ map[string]jev.Question) (*jev.Answers, error) {
	a.calls++
	if a.onAsk != nil {
		a.onAsk()
	}
	return &jev.Answers{
		Model: "jev-test",
		Noul:  map[string]float64{stale.QObsolete: 0.1, stale.QSuperseded: 0.1, stale.QWorthDoing: 0.9, stale.QNeedsReshape: 0.2},
		Score: map[string]jev.ScoreAnswer{stale.QPriorityFit: {Score: 2, Confidence: 0.7}},
	}, nil
}

// staleBeads holds one rig's open beads and none landed.
type staleBeads struct{ open []*beads.Issue }

func (s staleBeads) Open(string) ([]*beads.Issue, error)              { return s.open, nil }
func (s staleBeads) Landed(string, time.Time) ([]*beads.Issue, error) { return nil, nil }

// staleBead is an open bead last written in September, so it is stale on the
// clock the tests use.
func staleBead(id string) *beads.Issue {
	return &beads.Issue{
		ID:        id,
		Title:     "Title " + id,
		Status:    "open",
		Priority:  2,
		Type:      "task",
		CreatedAt: "2026-08-25T12:00:00Z",
		UpdatedAt: "2026-09-01T12:00:00Z",
	}
}

type jevStaleRig struct {
	d      *Daemon
	town   string
	logs   *bytes.Buffer
	clock  *clockwork.FakeClock
	opened int
	got    stale.Settings
	pass   *stale.Pass
	asker  *staleAsker
}

func newJevStaleRig(t *testing.T) *jevStaleRig {
	t.Helper()
	town := t.TempDir()
	r := &jevStaleRig{
		town:  town,
		logs:  &bytes.Buffer{},
		clock: clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)),
		asker: &staleAsker{},
	}
	r.d = &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(r.logs, "", 0),
		clock:  r.clock,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevStale: &JevStaleConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	r.pass = &stale.Pass{
		Asker:   r.asker,
		Beads:   staleBeads{open: []*beads.Issue{staleBead("gt-old")}},
		Rigs:    []string{"gastown"},
		LogPath: stale.LogPath(town),
		Allows:  func() (bool, error) { return true, nil },
		Now:     r.clock.Now,
	}
	r.d.jevStaleOpen = func(_ string, _ []string, s stale.Settings, _ io.Writer) (*stale.Pass, error) {
		r.opened++
		r.got = s
		return r.pass, nil
	}
	return r
}

func waitJevStaleIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevStaleRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_stale pass did not finish")
		}
		runtime.Gosched()
	}
}

// With the patrol enabled and due, the trigger runs one pass on its own
// goroutine with the configured settings and records the run. It is not due
// again until the interval has passed, and a bead asked in the window is not
// asked again.
func TestJevStaleTriggerRunsOnePassWhenEnabledAndDue(t *testing.T) {
	t.Parallel()
	r := newJevStaleRig(t)

	r.d.triggerJevStale()
	waitJevStaleIdle(t, r.d)

	if r.opened != 1 {
		t.Fatalf("pass opened %d times, want 1", r.opened)
	}
	if want := (stale.Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}); r.got != want {
		t.Errorf("settings = %+v, want %+v from daemon.json", r.got, want)
	}
	if r.asker.calls != 1 {
		t.Errorf("first pass asked %d beads, want 1", r.asker.calls)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_stale"); !found {
		t.Error("no last-run record after the pass")
	}

	r.d.triggerJevStale()
	waitJevStaleIdle(t, r.d)
	if r.opened != 1 {
		t.Errorf("a second trigger inside the week opened a pass (%d opens)", r.opened)
	}

	r.clock.Advance(jevStaleInterval)
	r.d.triggerJevStale()
	waitJevStaleIdle(t, r.d)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after the interval, want 2", r.opened)
	}
	if r.asker.calls != 1 {
		t.Errorf("a pass inside thirty days asked the same bead again (%d calls)", r.asker.calls)
	}
}

// A trigger that arrives while a pass is in flight is turned away.
func TestJevStaleTriggerSkipsWhileAPassIsRunning(t *testing.T) {
	t.Parallel()
	r := newJevStaleRig(t)
	r.d.jevStaleRunning.Store(true)

	r.d.triggerJevStale()

	if r.opened != 0 {
		t.Errorf("a trigger during a pass opened another (%d opens)", r.opened)
	}
}

// An E-stop raised while a pass runs ends it after the call in flight: no
// further bead is asked.
func TestJevStalePassStopsBetweenCallsWhenEstopped(t *testing.T) {
	t.Parallel()
	r := newJevStaleRig(t)
	r.pass.Beads = staleBeads{open: []*beads.Issue{staleBead("gt-a"), staleBead("gt-b"), staleBead("gt-c")}}
	r.asker.onAsk = func() {
		if r.asker.calls == 1 {
			if err := estop.Activate(r.town, "test", "stop the stale read"); err != nil {
				t.Fatal(err)
			}
		}
	}

	r.d.runJevStale()

	if r.asker.calls != 1 {
		t.Errorf("asked %d beads after the E-stop, want 1", r.asker.calls)
	}
	if !strings.Contains(r.logs.String(), "jev_stale: stopped: E-STOP active") {
		t.Errorf("log = %q, want the stop named", r.logs)
	}
}

// An E-stop that ends a pass does not spend the week: no last-run is recorded,
// so the next trigger after the E-stop clears runs the pass, and the bead the
// stop cut short is the one it asks about.
func TestJevStaleEstopLeavesTheWeekUnspent(t *testing.T) {
	t.Parallel()
	r := newJevStaleRig(t)
	r.pass.Beads = staleBeads{open: []*beads.Issue{staleBead("gt-a"), staleBead("gt-b")}}
	r.asker.onAsk = func() {
		if r.asker.calls == 1 {
			if err := estop.Activate(r.town, "test", "stop the stale read"); err != nil {
				t.Fatal(err)
			}
		}
	}

	r.d.triggerJevStale()
	waitJevStaleIdle(t, r.d)

	if _, found, _ := loadPatrolLastRun(r.town, "jev_stale"); found {
		t.Fatal("an E-stopped pass recorded its last run")
	}
	if err := estop.Deactivate(r.town, false); err != nil {
		t.Fatal(err)
	}
	r.asker.onAsk = nil

	r.d.triggerJevStale()
	waitJevStaleIdle(t, r.d)

	if r.opened != 2 || r.asker.calls != 2 {
		t.Errorf("opened %d passes with %d calls, want the second pass to ask the one bead left", r.opened, r.asker.calls)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_stale"); !found {
		t.Error("no last-run record after the pass that finished")
	}
}

// A run that finds the lock held logs it as skipped, not as a failure.
func TestJevStaleRunLogsABusyLockAsSkipped(t *testing.T) {
	t.Parallel()
	r := newJevStaleRig(t)
	release, err := battery.Lock(stale.LogPath(r.town) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	r.d.runJevStale()

	if r.asker.calls != 0 || !strings.Contains(r.logs.String(), "jev_stale: skipped:") {
		t.Errorf("calls = %d, log = %q; want no call and a skipped line", r.asker.calls, r.logs)
	}
}
