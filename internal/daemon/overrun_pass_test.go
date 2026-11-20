package daemon

import (
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
// only a pass in flight fails the read (gt-6yyt2j).
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

	t.Run("a pass in flight is an error, not a read", func(t *testing.T) {
		t.Parallel()
		d := passDaemon(t)
		var err error
		d.runOverrunGuardPass(func() { _, err = (&healthSources{d: d}).Ticks() })
		if err == nil {
			t.Error("Ticks with a pass in flight and no daemon start = nil error, want an error")
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
