package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var errDriveBoom = errors.New("town call failed")

// driveStart is the clock's first reading. The host load sample sits at it, so it
// falls inside the run's [Start, End) window.
var driveStart = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

// driveFake is the town for one run. It files beads gt-01 onward, reads every bead
// as closed, and fails the call named by failAt with errDriveBoom: "create", "list"
// (the polecat listing) or "close".
type driveFake struct {
	failAt  string
	mu      sync.Mutex
	created int
	closed  []string
}

func (f *driveFake) run(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if argv[0] == "gt" {
		switch argv[1] {
		case "polecat":
			if f.failAt == "list" {
				return nil, errDriveBoom
			}
			return []byte("[]"), nil
		case "daemon": // one heartbeat inside the window, so the log covers it
			return []byte(driveStart.Format("2006/01/02 15:04:05") + " daemon: heartbeat ok\n"), nil
		case "sling":
			return nil, nil
		}
	}
	verb := argv[1]
	if verb == "-C" {
		verb = argv[3]
	}
	switch verb {
	case "create":
		if f.failAt == "create" {
			return nil, errDriveBoom
		}
		f.created++
		return []byte(fmt.Sprintf("gt-%02d", f.created)), nil
	case "show":
		return []byte(`{"data":[{"status":"closed","notes":""}]}`), nil
	case "comments":
		return []byte(`{"data":[]}`), nil
	case "close":
		if f.failAt == "close" {
			return nil, errDriveBoom
		}
		f.closed = append(f.closed, argv[4])
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected argv %q", argv)
}

// driveClock reads start on its first call and one second later on each call after,
// so the window is [start, start+n seconds) and a sample at start is inside it.
func driveClock(start time.Time) func() time.Time {
	var mu sync.Mutex
	calls := 0
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return start.Add(time.Duration(calls-1) * time.Second)
	}
}

func driveOptions(t *testing.T, f *driveFake, ticks int, path string) Options {
	tasks := make([]Task, 16)
	for i := range tasks {
		tasks[i] = Task{Title: fmt.Sprintf("task %d", i), Body: "Body.\n\n## Acceptance\n- [ ] done\n"}
	}
	return Options{
		Town:      Town{Rig: "rehearsal", Run: f.run},
		Agent:     "claude",
		Worktrees: []string{"w1", "w2", "w3"},
		CacheRoot: t.TempDir(),
		SideRun:   func(context.Context, string, []string, []string) (int, error) { return 0, nil },
		Now:       driveClock(driveStart),
		Tick:      waitTicks(ticks),
		Deadline:  func(time.Duration) <-chan time.Time { return waitDeadline(false) },
		RunID:     "run-1",
		Tag:       "v1.2.3",
		ChangeSet: "cs-9",
		Tasks:     tasks,
		Path:      path,
		HostLoad:  strings.NewReader(driveStart.Format(time.RFC3339) + " load1=1.50 load5=1.00 load15=1.00 ncpu=24\n"),
	}
}

func TestRehearsalDriveFullRun(t *testing.T) {
	t.Parallel()
	f := &driveFake{}
	path := filepath.Join(t.TempDir(), "record.json")
	if _, err := Drive(context.Background(), driveOptions(t, f, 1, path)); err != nil {
		t.Fatalf("Drive error = %v; want nil", err)
	}
	saved, err := Read(path)
	if err != nil {
		t.Fatalf("read saved record: %v", err)
	}
	if len(saved.Slings) != 16 {
		t.Fatalf("saved %d slings, want 16", len(saved.Slings))
	}
	for i, s := range saved.Slings {
		if want := fmt.Sprintf("gt-%02d", i+1); s.Bead != want || s.Outcome != SlingOK {
			t.Errorf("sling %d = %+v, want bead %s with outcome ok", i, s, want)
		}
	}
	if saved.RunID != "run-1" || saved.Tag != "v1.2.3" || saved.ChangeSet != "cs-9" {
		t.Errorf("identity = %q %q %q, want run-1 v1.2.3 cs-9", saved.RunID, saved.Tag, saved.ChangeSet)
	}
	if !saved.Start.Equal(driveStart) || !saved.End.After(saved.Start) {
		t.Errorf("window = [%s, %s), want it to start at %s and end after", saved.Start, saved.End, driveStart)
	}
	if len(saved.Landings) != 16 || saved.Load.Samples != 1 {
		t.Errorf("collected %d landings and %d load samples, want 16 and 1", len(saved.Landings), saved.Load.Samples)
	}
	if want := []string{"gt-01", "gt-02", "gt-03", "gt-04", "gt-05", "gt-06", "gt-07", "gt-08", "gt-09", "gt-10", "gt-11", "gt-12", "gt-13", "gt-14", "gt-15", "gt-16"}; !slices.Equal(f.closed, want) {
		t.Errorf("closed %q, want %q", f.closed, want)
	}
}

func TestRehearsalDriveFailedStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		failAt   string
		ticks    int
		badDir   bool
		wantWrap error
	}{
		{name: "a failed filing returns its error", failAt: "create", ticks: 1, wantWrap: errDriveBoom},
		{name: "a closed tick ends the run window with its error", ticks: 0, wantWrap: errTickClosed},
		{name: "a failed polecat listing returns its error from Collect", failAt: "list", ticks: 1, wantWrap: errDriveBoom},
		{name: "a failed close returns its error", failAt: "close", ticks: 1, wantWrap: errDriveBoom},
		{name: "a save into a missing directory returns its error", ticks: 1, badDir: true, wantWrap: fs.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "record.json")
			if tt.badDir {
				path = filepath.Join(dir, "missing", "record.json")
			}
			_, err := Drive(context.Background(), driveOptions(t, &driveFake{failAt: tt.failAt}, tt.ticks, path))
			if !errors.Is(err, tt.wantWrap) {
				t.Fatalf("Drive error = %v; want it to wrap %v", err, tt.wantWrap)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("a record was saved at %s after a failed step", path)
			}
		})
	}
}
