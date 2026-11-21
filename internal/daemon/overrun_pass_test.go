package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townhealth"
)

// passDaemon is a daemon with overrun_guard on and nothing else to read, so a
// pass finds no seats and ends at once.
func passDaemon(t *testing.T) *Daemon {
	t.Helper()
	return &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{Enabled: true}}},
	}
}

// TestOverrunPassStartMarker: a pass reads as running from its start until its
// completion record clears the mark, so a health read can tell a pass in flight
// from one that is merely overdue (gt-46l6ct).
func TestOverrunPassStartMarker(t *testing.T) {
	t.Parallel()

	t.Run("a held pass reads its start, and nothing after it ends", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		if got := d.overrunGuardRun.RunningSince(); !got.IsZero() {
			t.Fatalf("RunningSince before any pass = %v, want zero", got)
		}

		before := time.Now()
		var during time.Time
		d.runOverrunGuardPass(func() { during = d.overrunGuardRun.RunningSince() })
		after := time.Now()

		if during.IsZero() {
			t.Fatal("RunningSince inside a started pass = zero, want the pass start")
		}
		if during.Before(before) || during.After(after) {
			t.Errorf("RunningSince inside the pass = %v, want a time between %v and %v", during, before, after)
		}
		if got := d.overrunGuardRun.RunningSince(); !got.IsZero() {
			t.Errorf("RunningSince after the pass ended = %v, want zero", got)
		}
	})

	t.Run("the guard's goroutine marks its pass", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		d.triggerOverrunGuard()
		waitFlagClear(t, &d.overrunGuardRunning)
		if d.overrunGuardRun.last().IsZero() {
			t.Fatal("triggering the guard recorded no completion, so no pass ran")
		}
		if got := d.overrunGuardRun.RunningSince(); !got.IsZero() {
			t.Errorf("RunningSince after the guard's pass = %v, want zero", got)
		}
	})
}

// TestOverrunPassStartMarkerFile: a pass persists its start to
// patrol_last_run.json while it runs, and its completion removes the entry. A
// start older than the daemon's own start reads as not running, so a pass a
// dead daemon left behind is not shown as in flight (gt-v9ynaw).
func TestOverrunPassStartMarkerFile(t *testing.T) {
	t.Parallel()

	t.Run("a started pass writes running_since and its end removes it", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		root := d.config.TownRoot
		before := time.Now()
		var during time.Time
		var duringErr error
		d.runOverrunGuardPass(func() {
			during, duringErr = ReadPatrolRunningSince(root, "overrun_guard", before)
		})

		if duringErr != nil {
			t.Fatalf("ReadPatrolRunningSince inside the pass: %v", duringErr)
		}
		if during.IsZero() {
			t.Fatal("running_since inside a started pass = zero, want the pass start in the file")
		}
		runs, err := ReadPatrolLastRuns(root)
		if err != nil {
			t.Fatalf("ReadPatrolLastRuns after the pass: %v", err)
		}
		if runs["overrun_guard"].IsZero() {
			t.Fatal("the pass recorded no completion, so its end never cleared the marker")
		}
		got, err := ReadPatrolRunningSince(root, "overrun_guard", before)
		if err != nil {
			t.Fatalf("ReadPatrolRunningSince after the pass: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("running_since after the pass = %v, want the entry removed", got)
		}
		data, err := os.ReadFile(filepath.Join(root, "daemon", "patrol_last_run.json"))
		if err != nil {
			t.Fatalf("read patrol_last_run.json after the pass: %v", err)
		}
		if strings.Contains(string(data), "running_since") {
			t.Errorf("patrol_last_run.json after the pass still carries running_since:\n%s", data)
		}
	})

	t.Run("a start older than the daemon's start reads as not running", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		old := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
		if err := savePatrolRunningSince(root, "overrun_guard", old); err != nil {
			t.Fatalf("savePatrolRunningSince: %v", err)
		}

		got, err := ReadPatrolRunningSince(root, "overrun_guard", old.Add(time.Hour))
		if err != nil {
			t.Fatalf("ReadPatrolRunningSince with a later daemon start: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("running_since %v from before the daemon started = %v, want not running", old, got)
		}

		got, err = ReadPatrolRunningSince(root, "overrun_guard", old.Add(-time.Hour))
		if err != nil {
			t.Fatalf("ReadPatrolRunningSince with an earlier daemon start: %v", err)
		}
		if !got.Equal(old) {
			t.Errorf("running_since from after the daemon started = %v, want %v", got, old)
		}
	})

	t.Run("an unknown daemon start is an error, not a read", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := savePatrolRunningSince(root, "overrun_guard", time.Now()); err != nil {
			t.Fatalf("savePatrolRunningSince: %v", err)
		}
		if _, err := ReadPatrolRunningSince(root, "overrun_guard", time.Time{}); err == nil {
			t.Error("ReadPatrolRunningSince with no daemon start = nil error, want an error")
		}
	})

	t.Run("a corrupt file is reported, not read as idle", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		path := filepath.Join(root, "daemon", "patrol_last_run.json")
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
			t.Fatalf("write corrupt file: %v", err)
		}
		_, err := ReadPatrolRunningSince(root, "overrun_guard", time.Now())
		if !errors.Is(err, ErrPatrolLastRunCorrupt) {
			t.Errorf("ReadPatrolRunningSince on a corrupt file: err = %v, want ErrPatrolLastRunCorrupt", err)
		}
	})

	t.Run("an unreadable file is an error, not read as idle", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		// A directory where the record should be: the file exists but cannot be
		// read, which is not the missing-file case.
		if err := os.MkdirAll(patrolLastRunPath(root), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, err := ReadPatrolRunningSince(root, "overrun_guard", time.Now())
		if err == nil {
			t.Error("ReadPatrolRunningSince on an unreadable file = nil error, want an error")
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadPatrolRunningSince on an unreadable file: err = %v, want a read error, not a missing file", err)
		}
	})
}

// TestOverrunPassTicksFillRunningSince: Ticks carries the overrun_guard pass
// start while it runs, and zero for other patrols and after it ends (gt-6yyt2j).
func TestOverrunPassTicksFillRunningSince(t *testing.T) {
	t.Parallel()
	read := func(t *testing.T, d *Daemon) map[string]time.Time {
		t.Helper()
		ticks, err := (&healthSources{d: d}).Ticks()
		if err != nil {
			t.Fatalf("Ticks: %v", err)
		}
		got := map[string]time.Time{}
		for _, tk := range ticks {
			got[tk.Name] = tk.RunningSince
		}
		return got
	}
	d := passDaemon(t)
	d.patrolConfig.Patrols.WispReaper = &WispReaperConfig{Enabled: true}
	if err := SaveState(d.config.TownRoot, &State{StartedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	before := time.Now()
	var during map[string]time.Time
	d.runOverrunGuardPass(func() { during = read(t, d) })
	after := time.Now()
	if s := during["overrun_guard"]; s.IsZero() || s.Before(before) || s.After(after) {
		t.Errorf("RunningSince for overrun_guard in its pass = %v, want a time between %v and %v", s, before, after)
	}
	if got, ok := during["wisp_reaper"]; !ok || !got.IsZero() {
		t.Errorf("RunningSince for wisp_reaper = %v (listed %v), want it listed and zero", got, ok)
	}
	if got := read(t, d)["overrun_guard"]; !got.IsZero() {
		t.Errorf("RunningSince for overrun_guard after its pass = %v, want zero", got)
	}
}

// TestOverrunPassTicksWithoutDaemonStart: an unknown daemon start blanks no
// tick. Only a marker in flight needs the start to be told from a stale one, so
// only a pass in flight reads UNKNOWN, and only for that patrol (gt-6yyt2j,
// gt-gg176y).
func TestOverrunPassTicksWithoutDaemonStart(t *testing.T) {
	t.Parallel()

	t.Run("no pass in flight reads every tick", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		got, err := (&healthSources{d: d}).Ticks()
		if err != nil {
			t.Fatalf("Ticks with no daemon start and no pass in flight: %v", err)
		}
		listed := false
		for _, tk := range got {
			if tk.Name != "overrun_guard" {
				continue
			}
			listed = true
			if !tk.RunningSince.IsZero() {
				t.Errorf("RunningSince for overrun_guard = %v, want zero with no pass in flight", tk.RunningSince)
			}
		}
		if !listed {
			t.Errorf("Ticks = %+v, want overrun_guard listed", got)
		}
	})

	t.Run("a pass in flight is that patrol's UNKNOWN, not the read's", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		d.patrolConfig.Patrols.WispReaper = &WispReaperConfig{Enabled: true}
		var logs bytes.Buffer
		d.logger = log.New(&logs, "", 0)
		var ticks []townhealth.Tick
		var err error
		d.runOverrunGuardPass(func() {
			ticks, err = (&healthSources{d: d}).Ticks()
			if err != nil {
				return
			}
			// A second read of the same state: the line is per change.
			_, err = (&healthSources{d: d}).Ticks()
		})
		if err != nil {
			t.Fatalf("Ticks with a pass in flight and no daemon start: %v", err)
		}
		byName := map[string]townhealth.Tick{}
		for _, tk := range ticks {
			byName[tk.Name] = tk
		}
		guard, ok := byName["overrun_guard"]
		if !ok {
			t.Fatalf("Ticks = %+v, want overrun_guard listed", ticks)
		}
		if guard.Err == nil {
			t.Errorf("overrun_guard tick Err = nil, want the unknown daemon start")
		}
		if reaper, ok := byName["wisp_reaper"]; !ok || reaper.Err != nil {
			t.Errorf("wisp_reaper tick = %+v (listed %v), want it listed with no error", reaper, ok)
		}
		want := "health ticks: overrun_guard: a pass is in flight and the daemon start is unknown; its tick is UNKNOWN"
		if got := strings.Count(logs.String(), want); got != 1 {
			t.Errorf("log carries the overrun_guard UNKNOWN line %d times, want once:\n%s", got, logs.String())
		}
		if strings.Contains(logs.String(), "health ticks: wisp_reaper:") {
			t.Errorf("log names wisp_reaper, whose tick is fine:\n%s", logs.String())
		}

		// The pass ended and cleared its marker: nothing is unknown, so a later
		// read is clean and logs nothing.
		logs.Reset()
		ticks, err = (&healthSources{d: d}).Ticks()
		if err != nil {
			t.Fatalf("Ticks after the pass: %v", err)
		}
		for _, tk := range ticks {
			if tk.Err != nil {
				t.Errorf("tick %s after the pass carries Err = %v, want none", tk.Name, tk.Err)
			}
		}
		if logs.Len() != 0 {
			t.Errorf("log after the pass = %q, want nothing", logs.String())
		}
	})

	t.Run("a corrupt record is an error, not an empty read", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		path := filepath.Join(d.config.TownRoot, "daemon", "patrol_last_run.json")
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
			t.Fatalf("write corrupt file: %v", err)
		}
		_, err := (&healthSources{d: d}).Ticks()
		if !errors.Is(err, ErrPatrolLastRunCorrupt) {
			t.Errorf("Ticks on a corrupt record: err = %v, want ErrPatrolLastRunCorrupt", err)
		}
	})
}

// TestOverrunPassReadSeat: a pass's three per-seat reads come back from one
// function (overrunReadSeat), each answer and each error in its own field, made
// in order for the seat it was given, with the escalation wait read only
// outside a gate (gt-872lkl).
func TestOverrunPassReadSeat(t *testing.T) {
	t.Parallel()
	answered := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	answerErr, gateErr, waitErr := errors.New("answer unreadable"), errors.New("container-gate slot status: boom"), errors.New("escalation unreadable")
	seat := overrunSeat{Rig: "gastown", Polecat: "toast", Bead: "gt-872lkl"}
	who := "gastown/toast/gt-872lkl"

	tests := []struct {
		name                        string
		inGate, waiting             bool
		gateErr, waitErr, answerErr error
		answeredAt                  time.Time
		want                        overrunSeatReads
		wantWaitRead                bool
	}{
		{name: "outside a gate reads the escalation wait", waiting: true, answeredAt: answered,
			want: overrunSeatReads{waiting: true, answeredAt: answered}, wantWaitRead: true},
		{name: "inside a gate reads no escalation wait", inGate: true, waiting: true, answeredAt: answered,
			want: overrunSeatReads{inGate: true, answeredAt: answered}},
		{name: "each failed read keeps its own error", gateErr: gateErr, waitErr: waitErr, answerErr: answerErr,
			want: overrunSeatReads{gateErr: gateErr, waitingErr: waitErr, answeredErr: answerErr}, wantWaitRead: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			call := func(read string, got overrunSeat) {
				calls = append(calls, read+" "+got.Rig+"/"+got.Polecat+"/"+got.Bead)
			}
			s := overrunSeams{
				// A still clock: the reads take no time, so their times stay zero.
				now: func() time.Time { return answered },
				escalationAnsweredAt: func(got overrunSeat) (time.Time, error) {
					call("answered", got)
					return tc.answeredAt, tc.answerErr
				},
				gateRunning: func(got overrunSeat) (bool, error) { call("gate", got); return tc.inGate, tc.gateErr },
				escalationWaiting: func(got overrunSeat) (bool, error) {
					call("waiting", got)
					return tc.waiting, tc.waitErr
				},
			}

			if got := overrunReadSeat(s, seat); got != tc.want {
				t.Errorf("overrunReadSeat = %+v, want %+v", got, tc.want)
			}
			wantCalls := "answered " + who + ", gate " + who
			if tc.wantWaitRead {
				wantCalls += ", waiting " + who
			}
			if got := strings.Join(calls, ", "); got != wantCalls {
				t.Errorf("seam calls = %q, want %q", got, wantCalls)
			}
		})
	}
}

// TestOverrunPassSeatLoopReads: the seat loop makes the three per-seat reads
// through overrunReadSeat, in order, only for a seat whose overrun state it
// could read. A seat whose state cannot be read is skipped before any per-seat
// read, so it is never judged on a partial read (gt-872lkl).
func TestOverrunPassSeatLoopReads(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	judged := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-ok111"}
	skipped := overrunSeat{Rig: "gastown", Polecat: "nux", Bead: "gt-bad22"}
	f := newOverrunFake(5*time.Minute, judged, skipped)

	// A state file that cannot be parsed: the seat is skipped before any read.
	path, err := overrunStatePath(d.config.TownRoot, skipped.Bead)
	if err != nil {
		t.Fatalf("overrunStatePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	var calls []string
	f.escalationAnsweredAt = func(seat overrunSeat) (time.Time, error) {
		calls = append(calls, "answered "+seat.Bead)
		return time.Time{}, nil
	}
	f.gateRunning = func(seat overrunSeat) (bool, error) {
		calls = append(calls, "gate "+seat.Bead)
		return false, nil
	}
	f.escalationWaiting = func(seat overrunSeat) (bool, error) {
		calls = append(calls, "waiting "+seat.Bead)
		return false, nil
	}

	d.runOverrunGuard(f.seams())

	want := "answered " + judged.Bead + ", gate " + judged.Bead + ", waiting " + judged.Bead
	if got := strings.Join(calls, ", "); got != want {
		t.Errorf("per-seat reads = %q, want %q (none for the unreadable-state seat)", got, want)
	}
	if !strings.Contains(buf.String(), "cannot read state for "+skipped.Bead) {
		t.Errorf("log does not report the unreadable state that skipped the seat:\n%s", buf.String())
	}
}

// TestOverrunPassStats: a pass times each seat's three reads on its own clock
// and, when it ends, tells passDone how many seats it read, the wall time it
// took, the seat whose reads took longest with each read's time, and how many
// reads failed. A dry run is reported too (gt-m0c0im).
func TestOverrunPassStats(t *testing.T) {
	t.Parallel()

	t.Run("two seats: the slowest seat, its read times and no failed reads", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		slow := overrunSeat{Rig: "gastown", Polecat: "furiosa", Bead: "gt-slow11"}
		fast := overrunSeat{Rig: "gastown", Polecat: "toast", Bead: "gt-fast11"}
		// The quick seat is read first, so the slowest seat has to be chosen
		// from the read times, not from the seat order.
		f := newOverrunFake(5*time.Minute, fast, slow)
		// Each read advances the fake clock: slow's answer 7 s and gate 1 s,
		// fast's answer 2 s; every other read takes none.
		answerTook := map[string]time.Duration{slow.Bead: 7 * time.Second, fast.Bead: 2 * time.Second}
		f.escalationAnsweredAt = func(seat overrunSeat) (time.Time, error) {
			f.now = f.now.Add(answerTook[seat.Bead])
			return time.Time{}, nil
		}
		f.gateRunning = func(seat overrunSeat) (bool, error) {
			if seat.Bead == slow.Bead {
				f.now = f.now.Add(time.Second)
			}
			return false, nil
		}
		f.escalationWaiting = func(overrunSeat) (bool, error) { return false, nil }

		var got passStats
		calls := 0
		s := f.seams()
		// The fake clock is shared, so the reads run one at a time (gt-5ngy14).
		s.readPoolSize = 1
		s.passDone = func(stats passStats) { calls++; got = stats }
		d.runOverrunGuard(s)

		if calls != 1 {
			t.Fatalf("passDone calls = %d, want 1", calls)
		}
		if got.seats != 2 {
			t.Errorf("seats = %d, want 2", got.seats)
		}
		if got.failedReads != 0 {
			t.Errorf("failedReads = %d, want 0", got.failedReads)
		}
		if got.wall != 10*time.Second {
			t.Errorf("wall = %s, want 10s (the reads' own time on the pass clock)", got.wall)
		}
		want := passSeatReads{rig: slow.Rig, polecat: slow.Polecat, bead: slow.Bead,
			reads: overrunSeatReads{answeredTook: 7 * time.Second, gateTook: time.Second}}
		if got.slowest != want {
			t.Errorf("slowest = %+v, want %+v", got.slowest, want)
		}
	})

	t.Run("a failed gate read counts as one failed read", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		f := newOverrunFake(5*time.Minute, overrunSeat{Rig: "gastown", Polecat: "nux", Bead: "gt-fail11"})
		f.gateRunning = func(overrunSeat) (bool, error) {
			return false, errors.New("container-gate slot status: boom")
		}

		var got passStats
		s := f.seams()
		s.passDone = func(stats passStats) { got = stats }
		d.runOverrunGuard(s)

		if got.seats != 1 {
			t.Errorf("seats = %d, want 1", got.seats)
		}
		if got.failedReads != 1 {
			t.Errorf("failedReads = %d, want 1", got.failedReads)
		}
	})

	t.Run("a seat list that cannot be read still reports", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		f := newOverrunFake(5 * time.Minute)

		var got passStats
		calls := 0
		s := f.seams()
		s.workingSeats = func() ([]overrunSeat, error) { return nil, errors.New("bd list: store unavailable") }
		s.passDone = func(stats passStats) { calls++; got = stats }
		d.runOverrunGuard(s)

		// The failure is logged; the pass still ends with the report it has.
		if calls != 1 {
			t.Fatalf("passDone calls = %d, want 1", calls)
		}
		if got.seats != 0 {
			t.Errorf("seats = %d, want 0", got.seats)
		}
	})

	t.Run("a dry run is reported too", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, true)
		f := newOverrunFake(5*time.Minute, overrunSeat{Rig: "gastown", Polecat: "toast", Bead: "gt-dry11"})

		var got passStats
		calls := 0
		s := f.seams()
		s.passDone = func(stats passStats) { calls++; got = stats }
		d.runOverrunGuard(s)

		if calls != 1 {
			t.Fatalf("passDone calls = %d, want 1", calls)
		}
		if got.seats != 1 {
			t.Errorf("seats = %d, want 1", got.seats)
		}
	})
}

// TestOverrunPassStatsNoSeats: a pass with no working seats still ends with a
// report: 0 seats, no slowest seat and no failed reads (gt-m0c0im).
func TestOverrunPassStatsNoSeats(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(5 * time.Minute)

	var got passStats
	calls := 0
	s := f.seams()
	s.passDone = func(stats passStats) { calls++; got = stats }
	d.runOverrunGuard(s)

	if calls != 1 {
		t.Fatalf("passDone calls = %d, want 1", calls)
	}
	if got.seats != 0 {
		t.Errorf("seats = %d, want 0", got.seats)
	}
	if got.slowest != (passSeatReads{}) {
		t.Errorf("slowest = %+v, want none", got.slowest)
	}
	if got.failedReads != 0 {
		t.Errorf("failedReads = %d, want 0", got.failedReads)
	}
}

// TestOverrunPassLine: the production passDone hook logs one line per pass: its
// seat count and wall time, the slowest seat with its exempt reads, and the
// reads that failed. A pass that read no seat has no slowest seat (gt-0r6j2r).
func TestOverrunPassLine(t *testing.T) {
	t.Parallel()
	// The slowest seat's three reads: 1 s answering, 7 s in its gate and 1 s
	// waiting on its escalation; the answer is inside the total.
	slow := passSeatReads{rig: "gastown", polecat: "furiosa", bead: "gt-slow11",
		reads: overrunSeatReads{answeredTook: time.Second, gateTook: 7 * time.Second, waitingTook: time.Second}}
	for _, tc := range []struct {
		name  string
		stats passStats
		want  string
	}{
		{"two seats, a slowest seat and a failed read",
			passStats{seats: 2, wall: 48 * time.Second, slowest: slow, failedReads: 1},
			"overrun_guard: pass: 2 seats in 48s; slowest gastown/furiosa 9s (gate 7s, escalation 1s); 1 read failed\n"},
		{"no seats", passStats{wall: time.Second}, "overrun_guard: pass: 0 seats in 1s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, buf := newOverrunDaemon(t, false)
			// The production seam, so the wiring and the line are both under test.
			d.overrunProdSeams().passDone(tc.stats)
			if got := buf.String(); got != tc.want {
				t.Errorf("pass line = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOverrunPassReadDeadline: a per-seat read past its 8 s deadline fails for that
// seat only, and the other seats are judged as usual (gt-jhthh0). That covers a read
// whose clock reading ran past the deadline, and one still running at it, which must
// not hold the pass.
func TestOverrunPassReadDeadline(t *testing.T) {
	t.Parallel()
	slow := overrunSeat{Rig: "gastown", Polecat: "furiosa", Bead: "gt-slow22"}
	fast := overrunSeat{Rig: "gastown", Polecat: "toast", Bead: "gt-fast22"}
	const deadline = "seat read ran past its deadline"

	t.Run("a gate read past the deadline on the pass clock", func(t *testing.T) {
		t.Parallel()
		d, buf := newOverrunDaemon(t, false)
		f := newOverrunFake(5*time.Minute, slow, fast)
		f.gateRunning = func(seat overrunSeat) (bool, error) {
			if seat.Bead == slow.Bead {
				f.now = f.now.Add(overrunSeatReadDeadline + time.Second)
			}
			return false, nil
		}
		var stats passStats
		s := f.seams()
		s.readPoolSize = 1
		s.passDone = func(got passStats) { stats = got }
		d.runOverrunGuard(s)
		if log := buf.String(); stats.seats != 2 || stats.failedReads != 1 || strings.Count(log, deadline) != 1 || !strings.Contains(log, "gate state for "+slow.Bead) {
			t.Errorf("seats %d, failed reads %d; want 2 seats and the slow gate read failed once:\n%s", stats.seats, stats.failedReads, log)
		}
	})

	t.Run("a read still running at the deadline", func(t *testing.T) {
		t.Parallel()
		d, buf := newOverrunDaemon(t, false)
		f := newOverrunFake(5*time.Minute, slow, fast)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		// The slow seat's answer is the first read, so only its timer fires.
		f.escalationAnsweredAt = func(seat overrunSeat) (time.Time, error) {
			if seat.Bead == slow.Bead {
				<-release
			}
			return time.Time{}, nil
		}
		fired := make(chan time.Time, 1)
		fired <- time.Time{}
		calls := 0
		s := f.seams()
		s.readPoolSize = 1
		s.after = func(time.Duration) <-chan time.Time {
			if calls++; calls == 1 {
				return fired
			}
			return make(chan time.Time)
		}
		var stats passStats
		s.passDone = func(got passStats) { stats = got }
		done := make(chan struct{})
		go func() { defer close(done); d.runOverrunGuard(s) }()
		select {
		case <-done:
		case <-time.After(workspaceHangGuard): //testpolicy:allow no-sleep — a hang guard only: the pass returns once every read is answered, so this fires only when a read still running holds it, the failure the test pins
			t.Fatalf("the pass did not return within %s: a read still running held it", workspaceHangGuard)
		}
		if log := buf.String(); stats.seats != 2 || stats.failedReads != 1 || strings.Count(log, deadline) != 1 || !strings.Contains(log, "escalation answer for "+slow.Bead) {
			t.Errorf("seats %d, failed reads %d; want 2 seats and the slow answer read failed once:\n%s", stats.seats, stats.failedReads, log)
		}
	})
}

// TestOverrunPassJudgesInSeatOrder: a pass whose six gate reads finish last
// seat first still nudges and logs in seat order, as a serial pass did (gt-n742vz).
func TestOverrunPassJudgesInSeatOrder(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	seats := make([]overrunSeat, 6)
	release := make([]chan struct{}, len(seats))
	at := map[string]int{}
	var want []string
	for i := range seats {
		seats[i] = overrunSeat{Rig: "gastown", Polecat: "p" + strconv.Itoa(i), Bead: "gt-ord" + strconv.Itoa(i)}
		release[i] = make(chan struct{})
		at[seats[i].Bead] = i
		want = append(want, seats[i].Rig+"/"+seats[i].Polecat)
	}
	f := newOverrunFake(13*time.Minute, seats...)
	stuck := make(chan struct{}, len(seats))
	f.gateRunning = func(seat overrunSeat) (bool, error) {
		select {
		case <-release[at[seat.Bead]]:
		case <-time.After(workspaceHangGuard): //testpolicy:allow no-sleep — the hang guard: a read never released is reported, not waited on
			stuck <- struct{}{}
		}
		if i := at[seat.Bead]; i > 0 {
			close(release[i-1]) // release the seat before this one, so the reads finish last seat first
		}
		return false, nil
	}
	f.escalationWaiting = func(overrunSeat) (bool, error) { return false, errors.New("escalation unreadable") }
	s := f.seams()
	s.readPoolSize = len(seats)
	close(release[len(seats)-1])
	runPoolBounded(t, "the judging pass", 1, 1, func(int) { d.runOverrunGuard(s) })
	if n := len(stuck); n != 0 {
		t.Fatalf("%d gate reads were never released within %s", n, workspaceHangGuard)
	}
	if got := strings.Join(f.nudges, ","); got != strings.Join(want, ",") {
		t.Errorf("judged in order %q, want seat order %q", got, strings.Join(want, ","))
	}
	if out := buf.String(); strings.Count(out, "cannot read the escalation state") != 1 || !strings.Contains(out, "for gt-ord0,") {
		t.Errorf("escalation read not logged once, for the first seat in seat order:\n%s", out)
	}
}

// TestOverrunPassBudgetStops: a pass starts no seat read once its budget (the interval less
// a margin: 50 s) has passed on the pass clock, and logs how many seats it did not reach
// (gt-gn4roo). Each seat's two timed reads advance the fake clock 6 s each, under the read
// deadline, so a seat costs 12 s: the reads that begin at 0, 12, 24, 36 and 48 s run, the
// rest do not. The reads run one at a time (readPoolSize 1) because the clock is shared.
func TestOverrunPassBudgetStops(t *testing.T) {
	t.Parallel()
	const seatCount = 20
	seats := make([]overrunSeat, seatCount)
	for i := range seats {
		seats[i] = overrunSeat{Rig: "gastown", Polecat: fmt.Sprintf("b%02d", i), Bead: fmt.Sprintf("gt-bud%02d", i)}
	}
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(5*time.Minute, seats...)
	read := 0
	f.escalationAnsweredAt = func(overrunSeat) (time.Time, error) {
		read++
		f.now = f.now.Add(6 * time.Second)
		return time.Time{}, nil
	}
	f.gateRunning = func(overrunSeat) (bool, error) {
		f.now = f.now.Add(6 * time.Second)
		return false, nil
	}
	s := f.seams()
	s.readPoolSize = 1
	s.passDone = func(stats passStats) { d.logger.Printf("%s", overrunPassLine(stats)) }
	runPoolBounded(t, "the pass", 1, 1, func(int) { d.runOverrunGuard(s) })
	if read != 5 {
		t.Errorf("%d seats read, want the 5 whose reads begin within 50 s", read)
	}
	if log := buf.String(); !strings.Contains(log, "15 seats not reached") {
		t.Errorf("the pass line does not name the 15 seats not reached:\n%s", log)
	}
}

// TestOverrunPassBudgetResumes: a pass the budget cuts after 5 of 20 seats leaves the next pass
// to start its reads at the first seat not reached, and a pass that reaches every seat leaves
// the next to start at the first seat. The order is the order the escalation-answer reads are
// made in (readPoolSize 1, so one at a time).
func TestOverrunPassBudgetResumes(t *testing.T) {
	t.Parallel()
	seats := make([]overrunSeat, 20)
	for i := range seats {
		seats[i] = overrunSeat{Rig: "gastown", Polecat: fmt.Sprintf("r%02d", i), Bead: fmt.Sprintf("gt-res%02d", i)}
	}
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(5*time.Minute, seats...)
	var order []string
	step := 6 * time.Second
	f.escalationAnsweredAt = func(seat overrunSeat) (time.Time, error) {
		order = append(order, seat.Bead[len("gt-res"):])
		f.now = f.now.Add(step)
		return time.Time{}, nil
	}
	f.gateRunning = func(overrunSeat) (bool, error) { f.now = f.now.Add(step); return false, nil }
	s := f.seams()
	s.readPoolSize = 1
	pass := func() string {
		order = nil
		runPoolBounded(t, "the pass", 1, 1, func(int) { d.runOverrunGuard(s) })
		return strings.Join(order, ",")
	}
	for i, want := range []string{"00,01,02,03,04", "05,06,07,08,09", "10,11,12,13,14", "15,16,17,18,19", "00,01,02,03,04"} {
		if got := pass(); got != want {
			t.Errorf("pass %d read seats %s, want %s", i+1, got, want)
		}
	}
	// Reads that take no time reach every seat from the cursor (seat 05), and clear it.
	step = 0
	if got, want := pass(), "05,06,07,08,09,10,11,12,13,14,15,16,17,18,19,00,01,02,03,04"; got != want {
		t.Errorf("a pass from the cursor read seats %s, want %s", got, want)
	}
	if got, want := pass(), "00,01,02,03,04,05,06,07,08,09,10,11,12,13,14,15,16,17,18,19"; got != want {
		t.Errorf("the pass after one that reached every seat read seats %s, want %s", got, want)
	}
}
