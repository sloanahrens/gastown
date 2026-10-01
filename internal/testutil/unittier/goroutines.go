// Package unittier checks, at the end of a package's unit tests, the unit
// tier's rules that only a run can see: no goroutine a test started may still
// be running when the tests end (docs/testing.md, "The rules").
//
// A package whose TestMain runs testutil.HermeticMain gets the check from the
// harness. Every other package's unit-tier TestMain calls Main:
//
//	func TestMain(m *testing.M) {
//		os.Exit(unittier.Main(m))
//	}
//
// The package imports nothing from the module, so any package can use it,
// including the ones testutil itself imports. TestUnitTierMain in
// internal/testpolicy holds every package to one or the other. In the
// integration tier (the integration build tag) the checks are off.
package unittier

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Main runs the tests and returns the exit code for os.Exit: m.Run's code,
// forced to 1 when a goroutine outlived the tests.
func Main(m *testing.M) int {
	before := Snapshot()
	return before.Check(m.Run(), os.Stderr)
}

// Goroutines is the set of goroutines running at a moment, taken by Snapshot
// before the tests start. The zero value checks nothing.
type Goroutines struct {
	ids map[int]bool
}

// Snapshot records the goroutines running now: those that package init or
// TestMain started before the tests, which Check does not blame on a test.
func Snapshot() Goroutines {
	gs := allGoroutines()
	ids := make(map[int]bool, len(gs))
	for _, g := range gs {
		ids[g.id] = true
	}
	return Goroutines{ids: ids}
}

// Check reports to w every goroutine still running that the snapshot does
// not hold, after giving stopped ones settle to exit, and returns code,
// forced to 1 from 0 when there was any. It checks nothing in the
// integration tier, or on the zero Goroutines.
func (before Goroutines) Check(code int, w io.Writer) int {
	if !enforced || before.ids == nil {
		return code
	}
	return report(code, leaked(allGoroutines, before.ids, settle, time.Sleep), w)
}

// report writes leaks to w and returns code, forced to 1 from 0 when there
// is any.
func report(code int, leaks []goroutine, w io.Writer) int {
	if len(leaks) == 0 {
		return code
	}
	rule := strings.Repeat("=", 72)
	fmt.Fprintf(w, "\n%s\nGOROUTINE LEAK: %d goroutine(s) outlived the unit tests\n\n", rule, len(leaks))
	for _, g := range leaks {
		fmt.Fprintf(w, "%s\n\n", g)
	}
	fmt.Fprintf(w, "A goroutine a test starts, or starts through the code under test, must\n")
	fmt.Fprintf(w, "have returned by the time the test ends: stop it in t.Cleanup (cancel its\n")
	fmt.Fprintf(w, "context, close its channel) and wait for it. \"created by\" names the\n")
	fmt.Fprintf(w, "function that started it (docs/testing.md, \"The rules\").\n%s\n", rule)
	if code == 0 {
		code = 1
	}
	return code
}

// goroutine is one goroutine in a runtime.Stack dump of all goroutines.
type goroutine struct {
	id    int
	state string
	// stack is the goroutine's frames, "created by" line included.
	stack string
}

// top is the function the goroutine is running: the first frame's.
func (g goroutine) top() string {
	line, _, _ := strings.Cut(g.stack, "\n")
	if i := strings.LastIndex(line, "("); i > 0 {
		line = line[:i]
	}
	return line
}

// String is the goroutine as the dump printed it.
func (g goroutine) String() string {
	return "goroutine " + strconv.Itoa(g.id) + " [" + g.state + "]:\n" + g.stack
}

// parseGoroutines splits a runtime.Stack(buf, true) dump into goroutines,
// in the dump's order: the calling goroutine first.
func parseGoroutines(dump string) []goroutine {
	var out []goroutine
	for _, block := range strings.Split(strings.TrimSpace(dump), "\n\n") {
		header, stack, _ := strings.Cut(block, "\n")
		rest, ok := strings.CutPrefix(header, "goroutine ")
		if !ok {
			continue
		}
		idText, state, ok := strings.Cut(rest, " [")
		if !ok {
			continue
		}
		id, err := strconv.Atoi(idText)
		if err != nil {
			continue
		}
		out = append(out, goroutine{id: id, state: strings.TrimSuffix(state, "]:"), stack: stack})
	}
	return out
}

// allGoroutines returns every goroutine of the process, the caller first.
func allGoroutines() []goroutine {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return parseGoroutines(string(buf[:n]))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// lifetimeGoroutines are the tops of goroutines that live as long as the
// process once something starts them, and that nothing can stop.
var lifetimeGoroutines = map[string]bool{
	// The standard library's signal loop, which signal.Notify starts.
	"os/signal.signal_recv": true,
	"os/signal.loop":        true,
	"runtime.ensureSigM":    true,
	// lumberjack starts its compression goroutine on a Logger's first write
	// and its Close does not stop it (v2.2.1). internal/daemon's New opens
	// daemon.log through one.
	"gopkg.in/natefinch/lumberjack%2ev2.(*Logger).millRun": true,
}

// stray returns the goroutines in gs, other than the first (the caller), that
// were not running before the tests (before) and are not lifetimeGoroutines.
func stray(gs []goroutine, before map[int]bool) []goroutine {
	var out []goroutine
	for i, g := range gs {
		if i == 0 || before[g.id] || lifetimeGoroutines[g.top()] {
			continue
		}
		out = append(out, g)
	}
	return out
}

// settle is how long Check waits for goroutines a test stopped, but that have
// not returned yet, to exit. A package with no stray goroutine pays nothing:
// the first look returns.
const settle = 2 * time.Second

// leaked returns the goroutines that outlived the tests: those stray still
// finds in dump after waiting up to settle for them to exit.
func leaked(dump func() []goroutine, before map[int]bool, settle time.Duration, sleep func(time.Duration)) []goroutine {
	wait := time.Millisecond
	var waited time.Duration
	for {
		gs := stray(dump(), before)
		if len(gs) == 0 || waited >= settle {
			return gs
		}
		sleep(wait)
		waited += wait
		if wait < 100*time.Millisecond {
			wait *= 2
		}
	}
}
