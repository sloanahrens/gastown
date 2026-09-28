package slot

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// testNow is where every test gate's fake clock starts. Container fixtures are
// dated relative to it, so their ages are exact.
var testNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// testHost is the host name a test gate's owner probe reports.
const testHost = "test-host"

// fakeRuntime is a ContainerRuntime that serves a scripted `docker ps` listing
// and records removals instead of reaching the host's docker.
type fakeRuntime struct {
	mu sync.Mutex
	// lines and listErr answer List, unless listFn is set, which is handed the
	// 1-based number of the call it is answering.
	lines     []string
	listErr   error
	listFn    func(call int) ([]string, error)
	listCalls int
	removed   []string
	removeErr error
}

func (r *fakeRuntime) List() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCalls++
	if r.listFn != nil {
		return r.listFn(r.listCalls)
	}
	return append([]string(nil), r.lines...), r.listErr
}

func (r *fakeRuntime) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.removeErr != nil {
		return r.removeErr
	}
	r.removed = append(r.removed, id)
	return nil
}

func (r *fakeRuntime) Info() (VMInfo, error) {
	return VMInfo{}, errors.New("fake runtime has no VM")
}

// setLines makes the listing a fixed set of lines.
func (r *fakeRuntime) setLines(lines ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines, r.listErr, r.listFn = lines, nil, nil
}

// failList makes every listing fail with err.
func (r *fakeRuntime) failList(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines, r.listErr, r.listFn = nil, err, nil
}

func (r *fakeRuntime) removedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removed...)
}

func (r *fakeRuntime) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCalls
}

// mapEnv is a process environment private to one test gate.
type mapEnv struct {
	mu   sync.Mutex
	vars map[string]string
}

func newMapEnv() *mapEnv { return &mapEnv{vars: map[string]string{}} }

func (e *mapEnv) Getenv(key string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.vars[key]
}

func (e *mapEnv) Setenv(key, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[key] = value
}

func (e *mapEnv) Unsetenv(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.vars, key)
}

// clone is the environment a child process would inherit at fork.
func (e *mapEnv) clone() *mapEnv {
	e.mu.Lock()
	defer e.mu.Unlock()
	return &mapEnv{vars: maps.Clone(e.vars)}
}

// syncBuffer is a bytes.Buffer safe to write from an acquire goroutine while
// the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testGate is a Gate whose every collaborator is a fake the test can script
// and inspect: a fake clock, a fake docker runtime (empty listing by default),
// a private environment, an owner probe on testHost where no pid is gone and
// no start time is readable, and captured diagnostics.
type testGate struct {
	*Gate
	clk    *clockwork.FakeClock
	rt     *fakeRuntime
	env    *mapEnv
	probe  *syncBuffer
	debris *syncBuffer
	// gone and starts script the owner probe; set them before the gate runs.
	gone   map[int]bool
	starts map[int]string
}

func newTestGate(t *testing.T) *testGate {
	t.Helper()
	tg := &testGate{
		clk:    clockwork.NewFakeClockAt(testNow),
		rt:     &fakeRuntime{},
		env:    newMapEnv(),
		probe:  &syncBuffer{},
		debris: &syncBuffer{},
		gone:   map[int]bool{},
		starts: map[int]string{},
	}
	tg.Gate = NewGate(WithRuntime(tg.rt), WithClock(tg.clk))
	tg.Gate.env = tg.env
	tg.Gate.probeOut = tg.probe
	tg.Gate.debrisOut = tg.debris
	tg.Gate.owner = ownerProbe{
		hostname: func() (string, error) { return testHost, nil },
		gone:     func(pid int) bool { return tg.gone[pid] },
		startToken: func(pid int) (string, bool) {
			s, ok := tg.starts[pid]
			return s, ok
		},
	}
	return tg
}

// child is the gate as a process forked from this one sees it: the same
// town-wide collaborators, the environment it inherited at fork, and its own
// pid.
func (tg *testGate) child() *Gate {
	c := *tg.Gate
	c.env = tg.env.clone()
	c.pid = tg.pid + 100000
	return &c
}

// driveClock advances clk by step each time something blocks on it, until done
// yields a value.
func driveClock[T any](t *testing.T, clk *clockwork.FakeClock, step time.Duration, done <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		blocked := make(chan error, 1)
		go func() { blocked <- clk.BlockUntilContext(ctx, 1) }()
		select {
		case v := <-done:
			return v
		case err := <-blocked:
			if err != nil {
				t.Fatalf("nothing blocked on the fake clock: %v", err)
			}
			clk.Advance(step)
		}
	}
}

// waitBlocked returns once something sleeps on clk: for an acquire, that is a
// finished blocked pass.
func waitBlocked(t *testing.T, clk *clockwork.FakeClock) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatalf("nothing blocked on the fake clock: %v", err)
	}
}

// acquired is one acquire's outcome.
type acquired struct {
	h   *Handle
	err error
}

// goAcquire runs fn in the background.
func goAcquire(fn func() (*Handle, error)) <-chan acquired {
	done := make(chan acquired, 1)
	go func() {
		h, err := fn()
		done <- acquired{h, err}
	}()
	return done
}

// run runs one acquire to completion on the fake clock, stepping it one poll
// interval whenever the acquire sleeps, and returns the fake time it took.
func (tg *testGate) run(t *testing.T, fn func() (*Handle, error)) (*Handle, error, time.Duration) {
	t.Helper()
	start := tg.clk.Now()
	got := driveClock(t, tg.clk, tg.pollInterval, goAcquire(fn))
	keepHeld(t, got.h)
	return got.h, got.err, tg.clk.Since(start)
}

// keepHeld releases h when the test ends. Until then the cleanup keeps h
// reachable: a Handle the test no longer mentions would otherwise be garbage
// collected, and the finalizer on its lock file would drop the flock under the
// test's feet.
func keepHeld(t *testing.T, h *Handle) {
	if h != nil {
		t.Cleanup(func() { _ = h.Release() })
	}
}

// mustAcquirePool acquires a slot the test expects to be free.
func (tg *testGate) mustAcquirePool(t *testing.T, town, role string, pool Pool) *Handle {
	t.Helper()
	h, err, _ := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, role, 5*time.Second, pool) })
	if err != nil {
		t.Fatalf("AcquirePool(%q): %v", role, err)
	}
	return h
}

// release releases h, failing the test on error.
func release(t *testing.T, h *Handle) {
	t.Helper()
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// foreignPID is a pid that is not this process's.
func foreignPID() int { return os.Getpid() + 100000 }
