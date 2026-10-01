package unittier

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// dump is a runtime.Stack(all) dump: the caller, a goroutine running since
// before the tests, the signal loop, a lumberjack mill and a leaked worker.
const dump = `goroutine 1 [running]:
main.main()
	/src/main.go:5 +0x1

goroutine 7 [select]:
example.com/init.loop()
	/src/init.go:9 +0x2
created by example.com/init.init.0 in goroutine 1
	/src/init.go:4 +0x3

goroutine 8 [syscall]:
os/signal.signal_recv()
	/go/src/runtime/sigqueue.go:152 +0x29
os/signal.loop()
	/go/src/os/signal/signal_unix.go:23 +0x13
created by os/signal.Notify.func1.1 in goroutine 1
	/go/src/os/signal/signal.go:151 +0x1f

goroutine 9 [chan receive, 1 minutes]:
gopkg.in/natefinch/lumberjack%2ev2.(*Logger).millRun(...)
	/mod/lumberjack.go:379
created by gopkg.in/natefinch/lumberjack%2ev2.(*Logger).mill.func1 in goroutine 8
	/mod/lumberjack.go:390 +0x9c

goroutine 21 [chan receive]:
example.com/pkg.(*cache).worker()
	/src/pkg/cache.go:184 +0x44
created by example.com/pkg.(*cache).enqueue in goroutine 20
	/src/pkg/cache.go:183 +0x198
`

func TestParseGoroutines(t *testing.T) {
	t.Parallel()
	gs := parseGoroutines(dump)
	var ids []int
	for _, g := range gs {
		ids = append(ids, g.id)
	}
	if want := []int{1, 7, 8, 9, 21}; !equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if g := gs[4]; g.state != "chan receive" || g.top() != "example.com/pkg.(*cache).worker" {
		t.Errorf("state %q top %q, want chan receive and the worker", g.state, g.top())
	}
	if !strings.HasPrefix(gs[4].String(), "goroutine 21 [chan receive]:\nexample.com/pkg.(*cache).worker()") {
		t.Errorf("String = %q, want the dump's own block", gs[4].String())
	}
}

func TestStraySkipsTheCallerTheSnapshotAndLifetimeGoroutines(t *testing.T) {
	t.Parallel()
	got := stray(parseGoroutines(dump), map[int]bool{7: true})
	if len(got) != 1 || got[0].id != 21 {
		t.Fatalf("stray = %v, want only goroutine 21", got)
	}
}

func TestLeakedWaitsForStoppedGoroutinesToExit(t *testing.T) {
	t.Parallel()
	all := parseGoroutines(dump)
	exited := all[:4]
	looks := 0
	var slept time.Duration
	got := leaked(func() []goroutine {
		looks++
		if looks < 3 {
			return all
		}
		return exited
	}, map[int]bool{7: true}, time.Second, func(d time.Duration) { slept += d })
	if len(got) != 0 {
		t.Fatalf("leaked = %v, want none once the worker exited", got)
	}
	if looks != 3 || slept != 3*time.Millisecond {
		t.Errorf("looked %d times, slept %v; want 3 looks and 1ms+2ms of sleep", looks, slept)
	}
}

func TestLeakedGivesUpAfterSettle(t *testing.T) {
	t.Parallel()
	all := parseGoroutines(dump)
	var slept time.Duration
	got := leaked(func() []goroutine { return all }, map[int]bool{7: true}, 2*time.Second, func(d time.Duration) { slept += d })
	if len(got) != 1 || got[0].id != 21 {
		t.Fatalf("leaked = %v, want goroutine 21", got)
	}
	if slept < 2*time.Second || slept > 2*time.Second+100*time.Millisecond {
		t.Errorf("slept %v, want settle (2s) rounded up to the next wait", slept)
	}
}

func TestReportLeaksFailsTheRunAndNamesTheCreator(t *testing.T) {
	t.Parallel()
	leaks := stray(parseGoroutines(dump), map[int]bool{7: true})
	var w bytes.Buffer
	if code := reportLeaks(0, leaks, &w); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	for _, want := range []string{"GOROUTINE LEAK: 1 goroutine(s) outlived the unit tests", "created by example.com/pkg.(*cache).enqueue"} {
		if !strings.Contains(w.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, w.String())
		}
	}
	if code := reportLeaks(3, leaks, &bytes.Buffer{}); code != 3 {
		t.Errorf("a failing code = %d, want it kept (3)", code)
	}
	w.Reset()
	if code := reportLeaks(0, nil, &w); code != 0 || w.Len() != 0 {
		t.Errorf("no leaks: code %d, output %q; want 0 and nothing", code, w.String())
	}
}

func TestNilRunChecksNothing(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	if code := (*Run)(nil).Check(0, &w); code != 0 || w.Len() != 0 {
		t.Errorf("nil Run: code %d, output %q; want 0 and nothing", code, w.String())
	}
}

func TestSnapshotHoldsTheCaller(t *testing.T) {
	t.Parallel()
	ids := snapshot()
	if self := allGoroutines()[0]; !ids[self.id] {
		t.Errorf("snapshot %v lacks the calling goroutine %d", ids, self.id)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
