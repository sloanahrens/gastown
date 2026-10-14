package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// stopWaitEpoch is the fixed start of the fake clock these tests run on.
var stopWaitEpoch = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// fakeStopTarget is a daemon process that exits exitsAfter past its SIGTERM
// on the fake clock, or never when exitsAfter is zero, and that dies
// immediately when killed — unless survivesKill, which models a process the
// SIGKILL has not taken down yet.
type fakeStopTarget struct {
	clk          *clockwork.FakeClock
	termAt       time.Time
	exitsAfter   time.Duration
	survivesKill bool

	mu     sync.Mutex
	kills  int
	exited bool
}

func (f *fakeStopTarget) alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exited {
		return false
	}
	return f.exitsAfter == 0 || f.clk.Since(f.termAt) < f.exitsAfter
}

func (f *fakeStopTarget) kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills++
	f.exited = !f.survivesKill
	return nil
}

func (f *fakeStopTarget) killCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kills
}

// runStopWait runs w on its fake clock, advancing the clock by w.poll each
// time w sleeps, and returns the fake time that passed and whether run
// reported the daemon gone.
func runStopWait(t *testing.T, clk *clockwork.FakeClock, w *daemonStopWait) (time.Duration, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := clk.Now()
	gone := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gone <- w.run()
	}()
	for {
		select {
		case <-done:
			return clk.Since(start), <-gone
		default:
		}
		// BlockUntilContext also returns when run finished while no sleeper
		// was registered, so cancel it from the done side.
		waitCtx, stop := context.WithCancel(ctx)
		go func() {
			select {
			case <-done:
				stop()
			case <-waitCtx.Done():
			}
		}()
		err := clk.BlockUntilContext(waitCtx, 1)
		stop()
		if err == nil {
			clk.Advance(w.poll)
			continue
		}
		if ctx.Err() != nil {
			t.Fatal("daemonStopWait.run did not finish")
		}
	}
}

func newStopWait(clk *clockwork.FakeClock, target *fakeStopTarget, out *bytes.Buffer) *daemonStopWait {
	return &daemonStopWait{
		clock:  clk,
		out:    out,
		pid:    4242,
		budget: StopWaitBudget,
		poll:   stopPollInterval,
		alive:  target.alive,
		verify: func() error { return nil },
		kill:   target.kill,
	}
}

func TestStopDaemonWaitTable(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		exitsAfter time.Duration // zero: never exits
		verifyErr  error
		staysAlive bool // ignores the SIGKILL
		wantKills  int
		wantGone   bool
		wantMin    time.Duration // fake time the wait must last at least
		wantMax    time.Duration // and at most
		wantOut    []string
	}{
		{
			name:       "exits within the shutdown budget: not killed",
			exitsAfter: ShutdownBudget - time.Second,
			wantKills:  0,
			wantGone:   true,
			wantMin:    ShutdownBudget - time.Second,
			wantMax:    ShutdownBudget,
			wantOut:    []string{"Waiting up to", "PID 4242"},
		},
		{
			name:       "exits past the old 500ms grace: not killed",
			exitsAfter: 2 * time.Second,
			wantKills:  0,
			wantGone:   true,
			wantMin:    2 * time.Second,
			wantMax:    3 * time.Second,
		},
		{
			name:       "still alive past the deadline: killed",
			exitsAfter: 0,
			wantKills:  1,
			wantGone:   true,
			wantMin:    StopWaitBudget,
			wantMax:    StopWaitBudget + time.Second,
			wantOut:    []string{"sending SIGKILL"},
		},
		{
			name:       "pid no longer the daemon at the deadline: not killed",
			exitsAfter: 0,
			verifyErr:  errors.New("PID 4242 is not the gt daemon"),
			wantKills:  0,
			wantMin:    StopWaitBudget,
			wantMax:    StopWaitBudget + time.Second,
		},
		{
			name:       "kill sent but the process outlives the grace",
			exitsAfter: 0,
			staysAlive: true,
			wantKills:  1,
			wantMin:    StopWaitBudget,
			wantMax:    StopWaitBudget + stopKillGrace + time.Second,
			wantOut:    []string{"sending SIGKILL"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clk := clockwork.NewFakeClockAt(stopWaitEpoch)
			target := &fakeStopTarget{clk: clk, termAt: clk.Now(), exitsAfter: tt.exitsAfter, survivesKill: tt.staysAlive}
			var out bytes.Buffer
			w := newStopWait(clk, target, &out)
			w.verify = func() error { return tt.verifyErr }

			elapsed, gone := runStopWait(t, clk, w)

			if got := target.killCount(); got != tt.wantKills {
				t.Errorf("SIGKILLs sent = %d, want %d", got, tt.wantKills)
			}
			if gone != tt.wantGone {
				t.Errorf("run reported gone = %v, want %v", gone, tt.wantGone)
			}
			if elapsed < tt.wantMin || elapsed > tt.wantMax {
				t.Errorf("waited %v, want between %v and %v", elapsed, tt.wantMin, tt.wantMax)
			}
			for _, s := range tt.wantOut {
				if !strings.Contains(out.String(), s) {
					t.Errorf("output %q does not mention %q", out.String(), s)
				}
			}
		})
	}
}

// A daemon already gone when the wait starts is neither waited on nor killed.
func TestStopDaemonWaitReturnsAtOnceWhenAlreadyGone(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClockAt(stopWaitEpoch)
	target := &fakeStopTarget{clk: clk, termAt: clk.Now(), exited: true}
	var out bytes.Buffer
	w := newStopWait(clk, target, &out)

	if elapsed, gone := runStopWait(t, clk, w); elapsed != 0 || !gone {
		t.Errorf("waited %v, gone = %v; want no wait and gone for a daemon that was already gone", elapsed, gone)
	}
	if target.killCount() != 0 || out.Len() != 0 {
		t.Errorf("kills = %d, output = %q; want neither", target.killCount(), out.String())
	}
}

// The wait is derived from the shutdown budget, not the 500ms notify delay
// that used to cut shutdown() short.
func TestStopDaemonWaitBudgetCoversShutdown(t *testing.T) {
	t.Parallel()
	if StopWaitBudget <= ShutdownBudget {
		t.Fatalf("StopWaitBudget = %v, want more than ShutdownBudget %v", StopWaitBudget, ShutdownBudget)
	}
}
