package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// windowFake is the town and side runner a window gets. Release slings
// concurrently and SideLoad runs its dirs at once, so both record under mu.
type windowFake struct {
	mu       sync.Mutex
	readErr  error // fails every bd read when set
	err      error // fails every side run when set
	slings   []string
	sideDirs []string

	seatErr   error               // fails every gt pool show when set
	seats     []int               // free seats each gt pool show answers, the last repeating
	advance   func(time.Duration) // moves the fake clock, when set: a seat read costs a minute, a sling a second
	seatReads int
}

func (f *windowFake) run(_ context.Context, argv []string) ([]byte, error) {
	if argv[0] == "gt" {
		f.mu.Lock()
		defer f.mu.Unlock()
		if argv[1] == "pool" {
			f.seatReads++
			if f.seatErr != nil {
				return nil, f.seatErr
			}
			free := f.seats[min(f.seatReads-1, len(f.seats)-1)]
			if f.advance != nil {
				f.advance(time.Minute) // a seat read is slow, so a wait is visible
			}
			return fmt.Appendf(nil, "claude 0/%d  open\ntotal 8 live\n", free), nil
		}
		f.slings = append(f.slings, strings.Join(argv, " "))
		if f.advance != nil {
			f.advance(time.Second)
		}
		return nil, nil
	}
	if f.readErr != nil {
		return nil, f.readErr
	}
	if argv[1] == "show" {
		return []byte(`{"data":[{"status":"closed"}]}`), nil
	}
	return []byte(`{"data":[]}`), nil
}

func (f *windowFake) sideRun(_ context.Context, dir string, _, _ []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sideDirs = append(f.sideDirs, dir)
	return 0, f.err
}

func TestRehearsalRunWindow(t *testing.T) {
	t.Parallel()
	boom, errSide := errors.New("no dolt"), errors.New("presubmit failed")
	ids := []string{"a", "b", "c", "d"}
	wantSlings := []string{"gt sling a rehearsal --agent claude", "gt sling b rehearsal --agent claude", "gt sling c rehearsal --agent claude", "gt sling d rehearsal --agent claude"}
	tests := []struct {
		name      string
		readErr   error
		sideErr   error
		ticks     int
		warm      bool
		wantSides int
		wantErrs  []error
	}{
		{name: "a fake town and side run give a sling per id and the side runs", ticks: 1, wantSides: 2},
		{name: "a failed read returns the wait error with what was gathered", readErr: boom, ticks: 3, wantSides: 2, wantErrs: []error{boom}},
		{name: "a closed tick returns the tick error", ticks: 0, wantSides: 2, wantErrs: []error{errTickClosed}},
		{name: "a failed side run returns its error with the side runs gathered", ticks: 1, sideErr: errSide, wantSides: 2, wantErrs: []error{errSide}},
		{name: "a warm GOCACHE stops the side load and returns its error", ticks: 1, warm: true, wantErrs: []error{errCacheNotEmpty}},
		{name: "a failed read and a failed side run return both errors", readErr: boom, sideErr: errSide, ticks: 3, wantSides: 2, wantErrs: []error{boom, errSide}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &windowFake{readErr: tt.readErr, err: tt.sideErr, seats: []int{8}}
			root := t.TempDir()
			if tt.warm {
				cache := filepath.Join(root, "gocache-0")
				if err := os.MkdirAll(cache, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cache, "warm"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var bound time.Duration
			o := Options{
				Town:      Town{Rig: "rehearsal", Run: f.run},
				Agent:     "claude",
				Worktrees: []string{"w1", "w2"},
				CacheRoot: root,
				SideRun:   f.sideRun,
				Now:       func() time.Time { return time.Time{} },
				Tick:      waitTicks(tt.ticks),
				Deadline:  func(after time.Duration) <-chan time.Time { bound = after; return waitDeadline(false) },
			}
			slings, runs, err := runWindow(context.Background(), o, ids)
			if bound != windowBound {
				t.Errorf("deadline is %v, want %v", bound, windowBound)
			}
			if len(slings) != len(ids) {
				t.Fatalf("got %d slings, want one per id (%d)", len(slings), len(ids))
			}
			for i, s := range slings {
				if s.Bead != ids[i] || s.Outcome != SlingOK {
					t.Errorf("sling %d = %+v, want bead %s with outcome %s", i, s, ids[i], SlingOK)
				}
			}
			slices.Sort(f.slings)
			if !slices.Equal(f.slings, wantSlings) {
				t.Errorf("ran slings %q, want %q", f.slings, wantSlings)
			}
			if len(runs) != tt.wantSides || len(f.sideDirs) != tt.wantSides {
				t.Errorf("got %d side runs over %d dirs, want %d", len(runs), len(f.sideDirs), tt.wantSides)
			}
			for _, want := range tt.wantErrs {
				if !errors.Is(err, want) {
					t.Errorf("error = %v; want it to wrap %v", err, want)
				}
			}
			if len(tt.wantErrs) == 0 && err != nil {
				t.Errorf("error = %v; want nil", err)
			}
		})
	}
}

// TestRehearsalRunWindowSeat checks the seat each sling waits for: with no seat
// free it waits for a tick, so gt sling never refuses one past the cap. The clock
// moves on the town's own reads and slings, so Seconds shows only the sling's second.
func TestRehearsalRunWindowSeat(t *testing.T) {
	t.Parallel()
	boom := errors.New("pool read failed")
	tests := []struct {
		name             string
		seats            []int
		seatErr          error
		ticks, wantReads int
		fired            bool
		wantOutcome      string
		wantSeconds      float64
	}{
		{name: "a sling waits for a tick to show a free seat and its seconds exclude the wait", seats: []int{0, 1}, ticks: 2, wantReads: 2, wantOutcome: SlingOK, wantSeconds: 1},
		{name: "a failed seat read fails the slings with a window error", seatErr: boom, ticks: 1, wantReads: 1, wantOutcome: SlingFailed},
		{name: "a fired seat deadline fails the slings with no seat error", seats: []int{0}, fired: true, wantOutcome: SlingFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var clock atomic.Int64
			f := &windowFake{seats: tt.seats, seatErr: tt.seatErr, advance: func(d time.Duration) { clock.Add(int64(d)) }}
			deadlines := 0
			o := Options{
				Town:     Town{Rig: "rehearsal", Run: f.run},
				Agent:    "claude",
				Now:      func() time.Time { return time.Unix(0, clock.Load()) },
				Tick:     waitTicks(tt.ticks),
				Deadline: func(time.Duration) <-chan time.Time { deadlines++; return waitDeadline(tt.fired) },
			}
			slings, _, err := runWindow(context.Background(), o, []string{"a"})
			if deadlines != 2 { // the seat's deadline and Wait's are separate channels
				t.Errorf("asked for %d deadlines, want one for the seat and one for the wait", deadlines)
			}
			if len(slings) != 1 || slings[0].Bead != "a" || slings[0].Outcome != tt.wantOutcome || slings[0].Seconds != tt.wantSeconds {
				t.Fatalf("slings = %+v; want one for a with outcome %s in %v seconds", slings, tt.wantOutcome, tt.wantSeconds)
			}
			if slung := len(f.slings) == 1 && f.slings[0] == "gt sling a rehearsal --agent claude"; slung != (tt.wantOutcome == SlingOK) {
				t.Errorf("ran slings %q, want a sling: %t", f.slings, tt.wantOutcome == SlingOK)
			}
			if f.seatReads != tt.wantReads {
				t.Errorf("read free seats %d times, want %d", f.seatReads, tt.wantReads)
			}
			if tt.seatErr != nil && (!errors.Is(err, tt.seatErr) || !strings.Contains(err.Error(), "run window: seat")) {
				t.Errorf("error = %v; want it to wrap %v as a seat error", err, tt.seatErr)
			} else if tt.seatErr == nil && err != nil {
				t.Errorf("error = %v; want nil", err)
			}
		})
	}
}
