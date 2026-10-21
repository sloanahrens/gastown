package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	jevconflict "github.com/steveyegge/gastown/internal/jev/conflict"
	jevdispatch "github.com/steveyegge/gastown/internal/jev/dispatch"
)

func TestJevConflictPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_conflict") {
		t.Error("jev_conflict must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_conflict") {
		t.Error("jev_conflict must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevConflict: &JevConflictConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_conflict") {
		t.Error("an explicit enabled=false must keep jev_conflict off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevConflict: &JevConflictConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_conflict") {
		t.Error("an explicit enabled=true must turn jev_conflict on")
	}
	// Turning the dispatch patrol on leaves the conflict patrol off.
	dispatchOnly := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDispatch: &JevDispatchConfig{Enabled: true}}}
	if IsPatrolEnabled(dispatchOnly, "jev_conflict") {
		t.Error("enabling jev_dispatch must not enable jev_conflict")
	}
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// and it logs nothing.
func TestJevConflictTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevConflict()

	if d.jevConflictRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_conflict"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and says why.
func TestJevConflictRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevConflict: &JevConflictConfig{Enabled: true}},
		},
	}

	d.runJevConflict()

	if !strings.Contains(logs.String(), "jev_conflict: error:") ||
		!strings.Contains(logs.String(), "usd_per_billion_tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

// conflictCountingAsker answers every pair the same way and counts the calls.
type conflictCountingAsker struct{ calls int }

func (c *conflictCountingAsker) Ask(_ context.Context, _ any, _ map[string]jev.Question) (*jev.Answers, error) {
	c.calls++
	return &jev.Answers{
		Model: "jev-test-1",
		Noul:  map[string]float64{"same_files": 0.6, "same_function": 0.1},
		Score: map[string]jev.ScoreAnswer{"risk": {Score: 1, Confidence: 0.5}},
	}, nil
}

// inFlightBeads are three beads on seats at 10:00, 10:20 and 10:40, the last
// one closed at 10:50, so the pass pairs them three ways.
func inFlightBeads() []*beads.Issue {
	return []*beads.Issue{
		{ID: "gt-a", Title: "Bead a", Status: "in_progress", Description: "attached_at: 2026-10-21T10:00:00Z\n\n## Goal\n\nGoal a.\n"},
		{ID: "gt-b", Title: "Bead b", Status: "in_progress", Description: "attached_at: 2026-10-21T10:20:00Z\n\n## Goal\n\nGoal b.\n"},
		{ID: "gt-c", Title: "Bead c", Status: "closed", ClosedAt: "2026-10-21T10:50:00Z",
			Description: "attached_at: 2026-10-21T10:40:00Z\n\n## Goal\n\nGoal c.\n"},
	}
}

// conflictRig is an enabled daemon over a temp town whose conflict passes open
// through a fake, so no pass reaches Jev or the rig's beads.
type conflictRig struct {
	d     *Daemon
	town  string
	logs  *bytes.Buffer
	asker *conflictCountingAsker
	pass  *jevconflict.Pass
}

func newConflictRig(t *testing.T) *conflictRig {
	t.Helper()
	town := t.TempDir()
	r := &conflictRig{
		town:  town,
		logs:  &bytes.Buffer{},
		asker: &conflictCountingAsker{},
	}
	r.d = &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(r.logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevConflict: &JevConflictConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	r.pass = &jevconflict.Pass{
		Asker:      r.asker,
		Lister:     stubPlaced{all: inFlightBeads()},
		LogPath:    jevconflict.LogPath(town),
		CursorPath: jevconflict.CursorPath(town),
		PairsPath:  jevconflict.PairsPath(town),
		Allows:     func() (bool, error) { return true, nil },
		Now:        func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
	}
	r.d.jevConflictOpen = func(string, jevconflict.Settings, io.Writer) (*jevconflict.Pass, error) {
		return r.pass, nil
	}
	if err := battery.WriteCursor(r.pass.CursorPath, battery.Cursor{Time: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	return r
}

// The trigger's pass asks one pair per placement pairing and logs a line for
// each, and a second pass over the same placements asks nothing.
func TestJevConflictRunLogsEachPairOnce(t *testing.T) {
	t.Parallel()
	r := newConflictRig(t)

	r.d.runJevConflict()

	if r.asker.calls != 3 {
		t.Errorf("asked %d pairs, want 3", r.asker.calls)
	}
	raw, err := os.ReadFile(r.pass.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "\n"); n != 3 {
		t.Errorf("conflict log has %d lines, want 3", n)
	}
	if !strings.Contains(r.logs.String(), "jev_conflict: asked 3 pairs, 0 failed, 0 given up") {
		t.Errorf("log = %q, want the pass summary", r.logs)
	}

	// The cursor is rewound, so the same placements come round again. Every pair
	// is already done.
	if err := battery.WriteCursor(r.pass.CursorPath, battery.Cursor{Time: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	r.d.runJevConflict()
	if r.asker.calls != 3 {
		t.Errorf("a second pass asked %d pairs in total, want still 3", r.asker.calls)
	}
}

// The dispatch pass reads the same placements and writes the same log, whether
// or not the conflict patrol runs over them. The conflict pass writes only its
// own files.
func TestJevConflictLeavesDispatchUnchanged(t *testing.T) {
	t.Parallel()
	off := runDispatchOver(t, false)
	on := runDispatchOver(t, true)

	if !reflect.DeepEqual(off.summary, on.summary) {
		t.Errorf("dispatch summary with the conflict patrol on = %+v, off = %+v", on.summary, off.summary)
	}
	if !bytes.Equal(off.log, on.log) {
		t.Errorf("dispatch log with the conflict patrol on differs:\non:  %s\noff: %s", on.log, off.log)
	}
	if off.summary.Asked != 3 {
		t.Errorf("dispatch asked %d beads, want the three placements", off.summary.Asked)
	}
}

// dispatchRun is what one dispatch pass left behind.
type dispatchRun struct {
	summary jevdispatch.Summary
	log     []byte
}

// runDispatchOver runs one dispatch pass over the in-flight beads in a fresh
// town. With conflictOn it also runs one conflict pass over the same beads
// first, so the two share the placements and nothing else.
func runDispatchOver(t *testing.T, conflictOn bool) dispatchRun {
	t.Helper()
	town := t.TempDir()
	lister := stubPlaced{all: inFlightBeads()}
	cursor := battery.Cursor{Time: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}
	now := func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }

	dispatch := &jevdispatch.Pass{
		Asker:      &dispatchCountingAsker{},
		Lister:     lister,
		LogPath:    jevdispatch.LogPath(town),
		CursorPath: jevdispatch.CursorPath(town),
		RetryPath:  jevdispatch.RetryPath(town),
		Allows:     func() (bool, error) { return true, nil },
		Now:        now,
	}
	if err := battery.WriteCursor(dispatch.CursorPath, cursor); err != nil {
		t.Fatal(err)
	}

	if conflictOn {
		conflict := &jevconflict.Pass{
			Asker:      &conflictCountingAsker{},
			Lister:     lister,
			LogPath:    jevconflict.LogPath(town),
			CursorPath: jevconflict.CursorPath(town),
			PairsPath:  jevconflict.PairsPath(town),
			Allows:     func() (bool, error) { return true, nil },
			Now:        now,
		}
		if err := battery.WriteCursor(conflict.CursorPath, cursor); err != nil {
			t.Fatal(err)
		}
		if _, err := conflict.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	sum, err := dispatch.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dispatch.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	return dispatchRun{summary: sum, log: raw}
}
