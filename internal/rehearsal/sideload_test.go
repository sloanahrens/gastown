package rehearsal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRuns records each run. A run yields at most 10000 times until want runs
// have started, so a SideLoad that runs them one at a time sees fewer peers.
type fakeRuns struct {
	want    int32
	started atomic.Int32
	codes   map[string]int
	errs    map[string]error
	mu      sync.Mutex
	calls   []sideCall
}

type sideCall struct {
	dir, cache string
	empty      bool
	peers      int32
	argv       []string
}

func (f *fakeRuns) run(_ context.Context, dir string, env []string, argv []string) (int, error) {
	f.started.Add(1)
	for i := 0; i < 10000 && f.started.Load() < f.want; i++ {
		runtime.Gosched()
	}
	cache := gocacheOf(env)
	entries, err := os.ReadDir(cache)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sideCall{dir, cache, err == nil && len(entries) == 0, f.started.Load(), argv})
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
	cacheRoot := t.TempDir()
	dirs := []string{"seat/alpha", "seat/beta", "seat/gamma"}
	codes := map[string]int{"seat/alpha": 0, "seat/beta": 2, "seat/gamma": 0}
	fake := &fakeRuns{want: int32(len(dirs)), codes: codes}

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
		if int(c.peers) != len(dirs) {
			t.Errorf("run in %s started with %d of %d runs in flight", c.dir, c.peers, len(dirs))
		}
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

func TestRehearsalSideLoadErrors(t *testing.T) {
	t.Parallel()
	errStart := errors.New("cannot start presubmit")
	tests := []struct {
		name, cacheRoot string
		errs            map[string]error
		wantErr         error
		wantRuns        int
		wantCalls       int
	}{
		{"cache root missing", "absent", nil, os.ErrNotExist, 0, 0},
		{"one run cannot start", "", map[string]error{"seat/beta": errStart}, errStart, 3, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tt.cacheRoot != "" {
				root = filepath.Join(root, tt.cacheRoot)
			}
			dirs := []string{"seat/alpha", "seat/beta", "seat/gamma"}
			fake := &fakeRuns{want: int32(len(dirs)), errs: tt.errs}
			runs, err := SideLoad(context.Background(), dirs, root, fake.run, stepClock())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SideLoad error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if len(runs) != tt.wantRuns || len(fake.calls) != tt.wantCalls {
				t.Errorf("got %d runs and %d started, want %d and %d", len(runs), len(fake.calls), tt.wantRuns, tt.wantCalls)
			}
		})
	}
}
