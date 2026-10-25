package leakcheck

import (
	"strings"
	"testing"
	"time"
)

const dumpOne = `goroutine 1 [running]:
main.main()
	/x/main.go:10 +0x1

goroutine 7 [chan receive]:
example.com/p.worker()
	/x/p.go:20 +0x2
created by example.com/p.Start in goroutine 1
	/x/p.go:15 +0x3
`

func TestParseGoroutines(t *testing.T) {
	t.Parallel() // unit tier: pure parsing of a fixed dump
	gs := parseGoroutines(dumpOne)
	if len(gs) != 2 || gs[0].id != 1 || gs[1].id != 7 {
		t.Fatalf("parseGoroutines ids = %+v, want [1 7]", gs)
	}
	if gs[1].state != "chan receive" {
		t.Errorf("state = %q, want chan receive", gs[1].state)
	}
	if got := gs[1].top(); got != "example.com/p.worker" {
		t.Errorf("top = %q, want example.com/p.worker", got)
	}
}

func TestStrayIgnoresCallerAndBaseline(t *testing.T) {
	t.Parallel()
	gs := parseGoroutines(dumpOne)
	if got := stray(gs, map[int]bool{7: true}); len(got) != 0 {
		t.Errorf("stray with 7 in the baseline = %v, want none (goroutine 1 is the caller)", got)
	}
	if got := stray(gs, map[int]bool{}); len(got) != 1 || got[0].id != 7 {
		t.Errorf("stray = %v, want goroutine 7", got)
	}
}

// A goroutine that exits while we wait is not a leak; one that does not is.
// The clock is injected, so no case sleeps.
func TestLeakedSettles(t *testing.T) {
	t.Parallel()
	gs := parseGoroutines(dumpOne)
	calls := 0
	dump := func() []goroutine {
		calls++
		if calls >= 3 {
			return gs[:1] // goroutine 7 returned by the third look
		}
		return gs
	}
	var slept time.Duration
	got := leaked(dump, map[int]bool{}, time.Second, func(d time.Duration) { slept += d })
	if len(got) != 0 {
		t.Errorf("leaked = %v, want none once goroutine 7 returned", got)
	}
	if slept == 0 {
		t.Error("leaked did not wait for the goroutine to return")
	}

	slept = 0
	got = leaked(func() []goroutine { return gs }, map[int]bool{}, 50*time.Millisecond, func(d time.Duration) { slept += d })
	if len(got) != 1 {
		t.Fatalf("leaked = %v, want goroutine 7 still running", got)
	}
	if slept < 50*time.Millisecond {
		t.Errorf("slept %v, want at least the settle time 50ms before giving up", slept)
	}
}

func TestReportNamesTheCreator(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	code := report(0, stray(parseGoroutines(dumpOne), map[int]bool{}), &b)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 when a goroutine leaked", code)
	}
	for _, want := range []string{"GOROUTINE LEAK", "created by example.com/p.Start"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report missing %q:\n%s", want, b.String())
		}
	}
	if got := report(0, nil, &b); got != 0 {
		t.Errorf("exit code = %d with no leak, want 0", got)
	}
	if got := report(3, nil, &b); got != 3 {
		t.Errorf("exit code = %d, want the tests' own code 3 preserved", got)
	}
}

func TestStrayIgnoresRuntimeLifetimeGoroutines(t *testing.T) {
	t.Parallel()
	dump := `goroutine 1 [running]:
main.main()

goroutine 9 [syscall]:
os/signal.signal_recv()
	/go/src/os/signal/sig.go:1 +0x1
created by os/signal.Notify.func1 in goroutine 1
`
	if got := stray(parseGoroutines(dump), map[int]bool{}); len(got) != 0 {
		t.Errorf("stray = %v, want the signal loop ignored: it lives for the process once started", got)
	}
}
