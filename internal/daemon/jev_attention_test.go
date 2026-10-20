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

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	jevattention "github.com/steveyegge/gastown/internal/jev/attention"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

func TestJevAttentionPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_attention") {
		t.Error("jev_attention must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_attention") {
		t.Error("jev_attention must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevAttention: &JevAttentionConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_attention") {
		t.Error("an explicit enabled=false must keep jev_attention off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevAttention: &JevAttentionConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_attention") {
		t.Error("an explicit enabled=true must turn jev_attention on")
	}
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// so a later enable is due at once, and it logs nothing.
func TestJevAttentionTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevAttention()

	if d.jevAttentionRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_attention"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and says why.
func TestJevAttentionRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevAttention: &JevAttentionConfig{Enabled: true}},
		},
	}

	d.runJevAttention()

	if !strings.Contains(logs.String(), "jev_attention: error:") ||
		!strings.Contains(logs.String(), "usd_per_billion_tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

// The configured cap and price reach the pass, and the pass runs its first
// pass, which only sets the cursor and makes no Jev call.
func TestJevAttentionRunPassesTheConfiguredSettings(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var got jevattention.Settings
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(&bytes.Buffer{}, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevAttention: &JevAttentionConfig{Enabled: true, MaxUSDPerDay: 2, USDPerBillionTokens: 3}},
		},
		jevAttentionOpen: func(_ string, s jevattention.Settings, _ io.Writer) (*jevattention.Pass, error) {
			got = s
			return &jevattention.Pass{
				State:      func() (attention.State, error) { return attention.State{}, nil },
				LogPath:    jevattention.LogPath(town),
				CursorPath: jevattention.CursorPath(town),
				SpentToday: func() (float64, error) { return 0, nil },
				CapUSD:     s.MaxUSDPerDay,
			}, nil
		},
	}

	d.runJevAttention()

	if got.MaxUSDPerDay != 2 || got.USDPerBillionTokens != 3 {
		t.Errorf("settings = %+v, want the 2 USD cap at 3 USD per billion tokens", got)
	}
	if _, err := os.Stat(jevattention.CursorPath(town)); err != nil {
		t.Errorf("the pass did not run: no cursor written (%v)", err)
	}
}

func TestJevAttentionConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_attention":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevAttention
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_attention = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// jevAttentionRig is an enabled daemon over a temp town whose passes open
// through a seam. Each pass has no items, so it makes no Jev call.
type jevAttentionRig struct {
	d      *Daemon
	clock  *clockwork.FakeClock
	town   string
	opened int
	got    jevattention.Settings
}

func newJevAttentionRig(t *testing.T) *jevAttentionRig {
	t.Helper()
	town := t.TempDir()
	r := &jevAttentionRig{town: town, clock: clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))}
	r.d = &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(&bytes.Buffer{}, "", 0),
		clock:  r.clock,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevAttention: &JevAttentionConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	r.d.jevAttentionOpen = func(_ string, s jevattention.Settings, _ io.Writer) (*jevattention.Pass, error) {
		r.opened++
		r.got = s
		return &jevattention.Pass{
			State:      func() (attention.State, error) { return attention.State{}, nil },
			LogPath:    jevattention.LogPath(town),
			CursorPath: jevattention.CursorPath(town),
			SpentToday: func() (float64, error) { return 0, nil },
			CapUSD:     s.MaxUSDPerDay,
		}, nil
	}
	return r
}

func waitJevAttentionIdle(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for d.jevAttentionRunning.Load() {
		if ctx.Err() != nil {
			t.Fatal("jev_attention pass did not finish")
		}
		runtime.Gosched()
	}
}

// With the patrol enabled and due, the trigger runs one pass on its own
// goroutine with the configured settings and records the run. It is not due
// again until the 15-minute interval passes.
func TestJevAttentionTriggerRunsOnePassWhenEnabledAndDue(t *testing.T) {
	t.Parallel()
	r := newJevAttentionRig(t)

	r.d.triggerJevAttention()
	waitJevAttentionIdle(t, r.d)

	if r.opened != 1 {
		t.Fatalf("pass opened %d times, want 1", r.opened)
	}
	if want := (jevattention.Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}); r.got != want {
		t.Errorf("settings = %+v, want %+v from daemon.json", r.got, want)
	}
	if _, found, _ := loadPatrolLastRun(r.town, "jev_attention"); !found {
		t.Error("no last-run record after the pass")
	}

	r.d.triggerJevAttention()
	waitJevAttentionIdle(t, r.d)
	if r.opened != 1 {
		t.Errorf("a second trigger inside the interval opened a pass (%d opens)", r.opened)
	}

	r.clock.Advance(jevAttentionInterval)
	r.d.triggerJevAttention()
	waitJevAttentionIdle(t, r.d)
	if r.opened != 2 {
		t.Errorf("pass opened %d times after the interval, want 2", r.opened)
	}
}

// An operator-disabled patrol stays off even with the config on.
func TestJevAttentionTriggerHonorsDisabledPatrols(t *testing.T) {
	t.Parallel()
	r := newJevAttentionRig(t)
	r.d.disabledPatrols = map[string]bool{"jev_attention": true}

	r.d.triggerJevAttention()

	if r.opened != 0 {
		t.Error("a disabled patrol opened a pass")
	}
}

// noAsk fails the test if a pass reaches Jev.
type noAsk struct{ t *testing.T }

func (n noAsk) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	n.t.Error("the daemon test reached Jev")
	return nil, errors.New("unexpected Jev call")
}

// The daily cap is checked before each item's call: an item raised after the
// cursor is not asked once today's spend has reached the cap.
func TestJevAttentionRunStopsAtTheCap(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	cursor := jevattention.CursorPath(town)
	if err := battery.WriteCursor(cursor, battery.Cursor{Time: time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevAttention: &JevAttentionConfig{Enabled: true, USDPerBillionTokens: 3}},
		},
		jevAttentionOpen: func(string, jevattention.Settings, io.Writer) (*jevattention.Pass, error) {
			return &jevattention.Pass{
				Asker: noAsk{t},
				State: func() (attention.State, error) {
					return attention.State{Items: []attention.Item{{
						Key:       "red",
						FirstSeen: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
					}}}, nil
				},
				LogPath:    jevattention.LogPath(town),
				CursorPath: cursor,
				SpentToday: func() (float64, error) { return 1, nil },
				CapUSD:     1,
			}, nil
		},
	}

	d.runJevAttention()

	if !strings.Contains(logs.String(), "the daily cap was reached") {
		t.Errorf("log = %q, want the cap named", logs)
	}
	if _, err := os.Stat(jevattention.LogPath(town)); !os.IsNotExist(err) {
		t.Errorf("a capped pass wrote a log line: %v", err)
	}
}

// failAsk fails every call, as Jev does in an outage.
type failAsk struct{}

func (failAsk) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	return nil, jev.ErrTimeout
}

// A pass where every call fails does not log like one where Jev answered: the
// daemon log says the pass stopped and no item was answered.
func TestJevAttentionRunLogsAnOutageApartFromAnswers(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	cursor := jevattention.CursorPath(town)
	if err := battery.WriteCursor(cursor, battery.Cursor{Time: time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	var items []attention.Item
	for _, k := range []string{"a", "b", "c"} {
		items = append(items, attention.Item{Key: k, FirstSeen: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)})
	}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevAttention: &JevAttentionConfig{Enabled: true, USDPerBillionTokens: 3}},
		},
		jevAttentionOpen: func(string, jevattention.Settings, io.Writer) (*jevattention.Pass, error) {
			return &jevattention.Pass{
				Asker:      failAsk{},
				State:      func() (attention.State, error) { return attention.State{Items: items}, nil },
				LogPath:    jevattention.LogPath(town),
				CursorPath: cursor,
				SpentToday: func() (float64, error) { return 0, nil },
				CapUSD:     1,
			}, nil
		},
	}

	d.runJevAttention()

	got := logs.String()
	if !strings.Contains(got, "0 items answered") || !strings.Contains(got, "stopped after 3 failed calls") {
		t.Errorf("log = %q, want the outage named and no items answered", got)
	}
}
