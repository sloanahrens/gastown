package rehearsal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
}

func (f *windowFake) run(_ context.Context, argv []string) ([]byte, error) {
	if argv[0] == "gt" {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.slings = append(f.slings, strings.Join(argv, " "))
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
			f := &windowFake{readErr: tt.readErr, err: tt.sideErr}
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
