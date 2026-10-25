// Package leakcheck fails a package's tests when a goroutine a test started
// is still running after they end. A package's TestMain calls Main:
//
//	func TestMain(m *testing.M) { os.Exit(leakcheck.Main(m)) }
//
// The tracker module may not import gastown's own harness, so this is its
// equivalent of internal/testutil/unittier's goroutine check.
package leakcheck

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

// settle is how long Main waits for goroutines the tests stopped, but that
// have not returned yet, before calling them leaked. A package with no stray
// goroutine pays nothing: the first look returns.
const settle = 2 * time.Second

// Main runs the tests and returns the exit code for os.Exit: m.Run's code,
// forced to 1 when a goroutine outlived the tests.
func Main(m *testing.M) int {
	before := snapshot()
	code := m.Run()
	return report(code, leaked(allGoroutines, before, settle, time.Sleep), os.Stderr)
}

// report writes leaks to w and returns code, forced to 1 from 0 when there is
// any.
func report(code int, leaks []goroutine, w io.Writer) int {
	if len(leaks) == 0 {
		return code
	}
	rule := strings.Repeat("=", 72)
	fmt.Fprintf(w, "\n%s\nGOROUTINE LEAK: %d goroutine(s) outlived the tests\n\n", rule, len(leaks))
	for _, g := range leaks {
		fmt.Fprintf(w, "%s\n\n", g)
	}
	fmt.Fprintf(w, "A goroutine a test starts, or starts through the code under test, must\n")
	fmt.Fprintf(w, "have returned by the time the test ends: stop it in t.Cleanup (cancel its\n")
	fmt.Fprintf(w, "context, close its channel) and wait for it. \"created by\" names the\n")
	fmt.Fprintf(w, "function that started it.\n%s\n", rule)
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

// parseGoroutines splits a runtime.Stack(buf, true) dump into goroutines, in
// the dump's order: the calling goroutine first.
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

// snapshot is the ids of the goroutines running now.
func snapshot() map[int]bool {
	gs := allGoroutines()
	ids := make(map[int]bool, len(gs))
	for _, g := range gs {
		ids[g.id] = true
	}
	return ids
}

// stray returns the goroutines in gs, other than the first (the caller), that
// were not running before the tests (before).
func stray(gs []goroutine, before map[int]bool) []goroutine {
	var out []goroutine
	for i, g := range gs {
		if i == 0 || before[g.id] {
			continue
		}
		out = append(out, g)
	}
	return out
}

// leaked returns the goroutines that outlived the tests: those stray still
// finds in dump after waiting up to settle for them to exit. sleep is
// injected so a test can run without a clock.
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
