package rehearsal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// fakeRuns records each run. Every run blocks until want runs have started, so a
// SideLoad that runs them one at a time deadlocks its synctest bubble.
type fakeRuns struct {
	want    int32
	started atomic.Int32
	all     chan struct{}
	codes   map[string]int
	errs    map[string]error
	mu      sync.Mutex
	calls   []sideCall
}

// newFakeRuns must be called inside the bubble that runs it.
func newFakeRuns(want int, codes map[string]int, errs map[string]error) *fakeRuns {
	return &fakeRuns{want: int32(want), all: make(chan struct{}), codes: codes, errs: errs}
}

type sideCall struct {
	dir, cache string
	empty      bool
	argv       []string
}

func (f *fakeRuns) run(_ context.Context, dir string, env []string, argv []string) (int, error) {
	if f.started.Add(1) == f.want {
		close(f.all)
	}
	<-f.all
	cache := gocacheOf(env)
	entries, err := os.ReadDir(cache)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sideCall{dir, cache, err == nil && len(entries) == 0, argv})
	return f.codes[dir], f.errs[dir]
}

// gocacheOf returns the last GOCACHE entry in env, the one a child process sees.
func gocacheOf(env []string) string {
	cache := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GOCACHE="); ok {
			cache = v
		}
	}
	return cache
}

func stepClock() func() time.Time {
	var ticks atomic.Int64
	return func() time.Time { return time.Unix(ticks.Add(1), 0) }
}

func TestRehearsalSideLoadRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, checkSideLoadRuns)
}

func checkSideLoadRuns(t *testing.T) {
	cacheRoot := t.TempDir()
	dirs := []string{"seat/alpha", "seat/beta", "seat/gamma"}
	codes := map[string]int{"seat/alpha": 0, "seat/beta": 2, "seat/gamma": 0}
	fake := newFakeRuns(len(dirs), codes, nil)

	runs, err := SideLoad(context.Background(), dirs, cacheRoot, fake.run, stepClock())
	if err != nil {
		t.Fatalf("SideLoad(%v): %v", dirs, err)
	}
	if len(runs) != len(dirs) || len(fake.calls) != len(dirs) {
		t.Fatalf("SideLoad returned %d runs and started %d, want %d each", len(runs), len(fake.calls), len(dirs))
	}
	for i, dir := range dirs {
		if runs[i].ExitCode != codes[dir] {
			t.Errorf("run in %s exit code = %d, want %d", dir, runs[i].ExitCode, codes[dir])
		}
		if runs[i].Seconds <= 0 {
			t.Errorf("run in %s wall = %v seconds, want a measured positive wall", dir, runs[i].Seconds)
		}
	}
	caches := map[string]bool{}
	for _, c := range fake.calls {
		if !reflect.DeepEqual(c.argv, []string{"make", "presubmit"}) {
			t.Errorf("run in %s argv = %v, want [make presubmit]", c.dir, c.argv)
		}
		if !strings.HasPrefix(c.cache, cacheRoot+string(filepath.Separator)) || !c.empty {
			t.Errorf("run in %s GOCACHE = %q, want an empty directory under %s", c.dir, c.cache, cacheRoot)
		}
		if caches[c.cache] {
			t.Errorf("GOCACHE %q is shared by two runs", c.cache)
		}
		caches[c.cache] = true
	}
}

// warmCache fills the second run's GOCACHE, as an earlier rehearsal would have.
func warmCache(t *testing.T, root string) {
	t.Helper()
	cache := filepath.Join(root, "gocache-1")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "warm.o"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fileAtCache makes the first run's GOCACHE a regular file.
func fileAtCache(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "gocache-0"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRehearsalSideLoadErrors(t *testing.T) {
	t.Parallel()
	errStart := errors.New("cannot start presubmit")
	tests := []struct {
		name, cacheRoot string
		setup           func(t *testing.T, root string)
		errs            map[string]error
		wantErr         error
		wantRuns        int
		wantCalls       int
	}{
		{"cache root missing", "absent", nil, nil, os.ErrNotExist, 0, 0},
		{"one run cannot start", "", nil, map[string]error{"seat/beta": errStart}, errStart, 3, 3},
		{"cache directory not empty", "", warmCache, nil, errCacheNotEmpty, 0, 0},
		{"cache path is a file", "", fileAtCache, nil, syscall.ENOTDIR, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				if tt.cacheRoot != "" {
					root = filepath.Join(root, tt.cacheRoot)
				}
				if tt.setup != nil {
					tt.setup(t, root)
				}
				dirs := []string{"seat/alpha", "seat/beta", "seat/gamma"}
				fake := newFakeRuns(len(dirs), nil, tt.errs)
				runs, err := SideLoad(context.Background(), dirs, root, fake.run, stepClock())
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("SideLoad error = %v, want it to wrap %v", err, tt.wantErr)
				}
				if len(runs) != tt.wantRuns || len(fake.calls) != tt.wantCalls {
					t.Errorf("got %d runs and %d started, want %d and %d", len(runs), len(fake.calls), tt.wantRuns, tt.wantCalls)
				}
			})
		})
	}
}
