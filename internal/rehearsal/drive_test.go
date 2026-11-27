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

// driveTasks is how many tasks driveOptions files, and so how many beads Drive creates.
const driveTasks = 16

// driveFake is the town for one run. It files beads gt-01 onward, reads every bead
// as closed, and fails the call named by failAt with errDriveBoom: "create", "list"
// (the polecat listing) or "close". A failed create fails every create after the first
// createBefore. A call under a canceled ctx fails with its error, as the town's
// commands do. When cancelOnShow is set, the first bead read cancels the run's ctx and
// finds that bead open.
type driveFake struct {
	failAt       string
	createBefore int
	cancelOnShow context.CancelFunc
	mu           sync.Mutex
	created      int
	lists        int
	closed       []string
}

func (f *driveFake) run(ctx context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if argv[0] == "gt" {
		switch argv[1] {
		case "polecat":
			f.lists++
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
		f.created++
		if f.failAt == "create" && f.created > f.createBefore {
			return nil, errDriveBoom
		}
		return []byte(fmt.Sprintf("gt-%02d", f.created)), nil
	case "show":
		if f.cancelOnShow != nil {
			f.cancelOnShow()
			f.cancelOnShow = nil
			return []byte(`{"data":[{"status":"open","notes":""}]}`), nil
		}
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
	tasks := make([]Task, driveTasks)
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
	if len(saved.Slings) != driveTasks {
		t.Fatalf("saved %d slings, want %d", len(saved.Slings), driveTasks)
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
	if len(saved.Landings) != driveTasks || saved.Load.Samples != 1 {
		t.Errorf("collected %d landings and %d load samples, want %d and 1", len(saved.Landings), saved.Load.Samples, driveTasks)
	}
	if want := []string{"gt-01", "gt-02", "gt-03", "gt-04", "gt-05", "gt-06", "gt-07", "gt-08", "gt-09", "gt-10", "gt-11", "gt-12", "gt-13", "gt-14", "gt-15", "gt-16"}; !slices.Equal(f.closed, want) {
		t.Errorf("closed %q, want %q", f.closed, want)
	}
}

func TestRehearsalDriveFailedStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		failAt       string
		ticks        int
		createBefore int
		badDir       bool
		cancelWindow bool
		wantWrap     error
		wantAlso     error
		wantStop     string
		wantClosed   int
		wantLists    int
	}{
		{name: "a failed filing saves a stopped record and closes what it filed", failAt: "create", ticks: 1, createBefore: 2, wantWrap: errDriveBoom, wantStop: "file set failed", wantClosed: 2, wantLists: 1},
		{name: "a closed tick stops the run and saves what Collect read", ticks: 0, wantWrap: errTickClosed, wantStop: "run window failed", wantClosed: driveTasks, wantLists: 1},
		{name: "a canceled ctx stops the run and still saves what Collect read", ticks: 1, cancelWindow: true, wantWrap: context.Canceled, wantStop: "run window failed", wantClosed: driveTasks, wantLists: 1},
		{name: "a failed close after a failed window joins both and still saves", failAt: "close", ticks: 0, wantWrap: errTickClosed, wantAlso: errDriveBoom, wantStop: "run window failed", wantLists: 1},
		{name: "a failed save after a failed window joins both", ticks: 0, badDir: true, wantWrap: errTickClosed, wantAlso: fs.ErrNotExist, wantStop: "run window failed", wantClosed: driveTasks, wantLists: 1},
		{name: "a failed collect after a failed window joins both and still closes and saves", failAt: "list", ticks: 0, wantWrap: errTickClosed, wantAlso: errDriveBoom, wantStop: "run window failed", wantClosed: driveTasks, wantLists: 1},
		{name: "a failed collect after a good window saves a stopped record", failAt: "list", ticks: 1, wantWrap: errDriveBoom, wantStop: "collect failed", wantClosed: driveTasks, wantLists: 1},
		{name: "a failed close after a good window saves a stopped record", failAt: "close", ticks: 1, wantWrap: errDriveBoom, wantStop: "close beads failed", wantLists: 1},
		{name: "a failed save returns the stopped record and runs no step again", ticks: 1, badDir: true, wantWrap: fs.ErrNotExist, wantStop: "save record failed", wantClosed: driveTasks, wantLists: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &driveFake{failAt: tt.failAt, createBefore: tt.createBefore}
			if tt.cancelWindow {
				f.cancelOnShow = cancel
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "record.json")
			if tt.badDir {
				path = filepath.Join(dir, "missing", "record.json")
			}
			rec, err := Drive(ctx, driveOptions(t, f, tt.ticks, path))
			if !errors.Is(err, tt.wantWrap) {
				t.Fatalf("Drive error = %v; want it to wrap %v", err, tt.wantWrap)
			}
			if tt.wantAlso != nil && !errors.Is(err, tt.wantAlso) {
				t.Errorf("Drive error = %v; want it to also wrap %v", err, tt.wantAlso)
			}
			if !rec.Stopped || rec.StopReason != tt.wantStop {
				t.Errorf("returned record Stopped = %t, reason %q; want true, reason %q", rec.Stopped, rec.StopReason, tt.wantStop)
			}
			if len(f.closed) != tt.wantClosed {
				t.Errorf("closed %d beads, want %d", len(f.closed), tt.wantClosed)
			}
			if f.lists != tt.wantLists {
				t.Errorf("ran %d polecat listings, want %d; a run collects once", f.lists, tt.wantLists)
			}
			if tt.badDir {
				if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("a record was saved at %s after a failed save", path)
				}
				return
			}
			saved, readErr := Read(path)
			if readErr != nil {
				t.Fatalf("read saved record: %v", readErr)
			}
			if !saved.Stopped || saved.StopReason != tt.wantStop {
				t.Errorf("saved Stopped = %t, reason %q; want true, reason %q", saved.Stopped, saved.StopReason, tt.wantStop)
			}
			if !saved.End.After(saved.Start) {
				t.Errorf("window = [%s, %s), want it to end after it starts", saved.Start, saved.End)
			}
		})
	}
}
