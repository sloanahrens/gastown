package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	jevthrash "github.com/steveyegge/gastown/internal/jev/thrash"
)

// thrashAsker answers both thrash questions with fixed probabilities and counts
// the calls it is asked to make.
type thrashAsker struct {
	calls int
}

func (a *thrashAsker) Ask(_ context.Context, _ any, _ map[string]jev.Question) (*jev.Answers, error) {
	a.calls++
	return &jev.Answers{
		Model: "jev-test",
		Noul:  map[string]float64{"will_respawn": 0.7, "needs_split": 0.2},
	}, nil
}

// thrashLister lists one placed bead, stamped 11:00, whatever the cursor.
type thrashLister struct{}

func (thrashLister) Placed(time.Time) ([]*beads.Issue, error) {
	return []*beads.Issue{{
		ID: "gt-new", Title: "Do a thing", Status: "in_progress",
		Description: "attached_at: 2026-10-20T11:00:00Z\n\n## Goal\n\nDo a thing.\n",
	}}, nil
}

// newThrashDaemon is a daemon whose thrash pass runs over fakes, with the
// patrol on and the cap in daemon.json.
func newThrashDaemon(t *testing.T, enabled bool) (*Daemon, *thrashAsker, *jevthrash.Settings, *int, *clockwork.FakeClock) {
	t.Helper()
	town := t.TempDir()
	asker := &thrashAsker{}
	var got jevthrash.Settings
	opened := 0
	fakeClock := clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(&bytes.Buffer{}, "", 0),
		clock:  fakeClock,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevThrash: &JevThrashConfig{
			Enabled: enabled, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	d.jevThrashOpen = func(_ string, s jevthrash.Settings, _ io.Writer) (*jevthrash.Pass, error) {
		opened++
		got = s
		return &jevthrash.Pass{
			Asker:      asker,
			Lister:     thrashLister{},
			LogPath:    jevthrash.LogPath(town),
			CursorPath: jevthrash.CursorPath(town),
			Allows:     func() (bool, error) { return true, nil },
			Now:        func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
		}, nil
	}
	return d, asker, &got, &opened, fakeClock
}

func waitJevThrashIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevThrashRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_thrash pass did not finish")
		}
		runtime.Gosched()
	}
}

func TestJevThrashPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_thrash") {
		t.Error("jev_thrash must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_thrash") {
		t.Error("jev_thrash must be off when daemon.json does not mention it")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevThrash: &JevThrashConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_thrash") {
		t.Error("an explicit enabled=true must turn jev_thrash on")
	}
	dispatchOnly := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDispatch: &JevDispatchConfig{Enabled: true}}}
	if IsPatrolEnabled(dispatchOnly, "jev_thrash") {
		t.Error("enabling jev_dispatch must not enable jev_thrash")
	}
}

// With the patrol off the trigger opens no pass and makes no call.
func TestJevThrashTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	d, asker, _, opened, _ := newThrashDaemon(t, false)

	d.triggerJevThrash()
	waitJevThrashIdle(t, d)

	if *opened != 0 || asker.calls != 0 {
		t.Errorf("opened %d passes and made %d calls with the patrol off", *opened, asker.calls)
	}
}

// With the patrol on and due, the trigger runs one pass with the configured
// settings, logs the placement, and is not due again for fifteen minutes.
func TestJevThrashTriggerRunsOnePassWhenEnabledAndDue(t *testing.T) {
	t.Parallel()
	d, asker, got, opened, clock := newThrashDaemon(t, true)
	// The first pass only sets a cursor, so seed one before the placement at
	// 11:00, as the dispatch battery's test does.
	cursor := jevthrash.CursorPath(d.config.TownRoot)
	if err := battery.WriteCursor(cursor, battery.Cursor{Time: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}

	d.triggerJevThrash()
	waitJevThrashIdle(t, d)

	if *opened != 1 {
		t.Fatalf("pass opened %d times, want 1", *opened)
	}
	if want := (jevthrash.Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}); *got != want {
		t.Errorf("settings = %+v, want %+v from daemon.json", *got, want)
	}
	if asker.calls != 1 {
		t.Errorf("Jev was asked %d times, want 1 placement", asker.calls)
	}
	if _, err := os.Stat(jevthrash.LogPath(d.config.TownRoot)); err != nil {
		t.Errorf("the pass wrote no log line: %v", err)
	}
	if _, found, _ := loadPatrolLastRun(d.config.TownRoot, "jev_thrash"); !found {
		t.Error("no last-run record after the pass")
	}

	clock.Advance(14 * time.Minute)
	d.triggerJevThrash()
	waitJevThrashIdle(t, d)
	if *opened != 1 {
		t.Errorf("a trigger 14 minutes on opened a pass (%d opens)", *opened)
	}
}

func TestJevThrashIsALifecycleHeartbeatStep(t *testing.T) {
	t.Parallel()
	for _, j := range heartbeatSteps {
		if j.name == "jev-thrash" {
			if !j.lifecycle {
				t.Error("jev-thrash must be a lifecycle job, so the town E-stop holds its paid calls")
			}
			return
		}
	}
	t.Error("jev-thrash is not in the daemon's heartbeat steps")
}
