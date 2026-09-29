package testpolicy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

type TestTime struct {
	Name    string
	Elapsed time.Duration
}

// Overrun is a converted package the budget fails: its test process tree
// used more user CPU than the budget, or (Unmeasured) it passed without a CPU
// measurement, which the runner must not read as "under budget". Elapsed is
// its wall time, reported for context only.
type Overrun struct {
	Package    string
	Elapsed    time.Duration
	CPU        CPUTime
	Unmeasured bool
	Slowest    []TestTime
}

type testEvent struct {
	Action     string
	Package    string
	ImportPath string
	Test       string
	Elapsed    float64
	Output     string
}

// pkgOutput buffers one package's `go test -json` output events so they can
// be emitted contiguously, in the shape plain (non-verbose) `go test` prints:
// a failing package gets its whole buffer (every output event, test-level
// included); a passing or skipped package gets only its package-level
// ("ok  \tpkg\t1.2s" / "?   \tpkg\t[no test files]") lines.
type pkgOutput struct {
	all     bytes.Buffer
	summary bytes.Buffer
}

// WatchBudget copies the human-readable output of a `go test -json` stream to
// w, reproducing plain `go test` text (Ruling R10) rather than the raw
// interleaved JSON stream, and returns every package, outside exempt, whose
// test process tree used more user CPU than budget (cpu supplies the
// measurement). Package names are reported relative to module.
//
// The budget is on user CPU, not wall or system time, because those two
// depend on the host (gt-ty2fy, docs/testing.md "The time budget"): under a
// loaded full run internal/testpolicy took 13.2 s of wall for the 2.1 s of
// user CPU it uses alone, and internal/slot's system time went from 1.6 s to
// 11.1 s while its user time stayed at 0.1-0.2 s.
func WatchBudget(r io.Reader, w io.Writer, budget time.Duration, exempt map[string]bool, module string, cpu CPUSource) ([]Overrun, error) {
	over, _, err := WatchBudgetTracked(r, w, budget, exempt, nil, module, cpu)
	return over, err
}

// WatchBudgetTracked is WatchBudget that also exempts the packages in tracked
// (overbudget.txt: package to bead id) and returns the time each of them
// took, so the caller can report them on every run.
func WatchBudgetTracked(r io.Reader, w io.Writer, budget time.Duration, exempt map[string]bool, tracked map[string]string, module string, cpu CPUSource) ([]Overrun, []TrackedRun, error) {
	var runs []TrackedRun
	tests := map[string][]TestTime{}
	bufs := map[string]*pkgOutput{}
	var over []Overrun
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var ev testEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			// go test prints build failures as plain text; pass them through.
			_, _ = io.WriteString(w, sc.Text()+"\n")
			continue
		}
		if ev.Action == "build-output" {
			// Go >=1.24 reports compile/vet failures as build-output events
			// keyed by ImportPath, not Package; write them through
			// immediately so `make test` shows the file:line instead of
			// only "[build failed]".
			_, _ = io.WriteString(w, ev.Output)
			continue
		}
		pkg := strings.TrimPrefix(strings.TrimPrefix(ev.Package, module), "/")
		if ev.Action == "output" {
			b := bufs[pkg]
			if b == nil {
				b = &pkgOutput{}
				bufs[pkg] = b
			}
			b.all.WriteString(ev.Output)
			if ev.Test == "" {
				b.summary.WriteString(ev.Output)
			}
			continue
		}
		if ev.Test != "" {
			if ev.Action == "pass" || ev.Action == "fail" {
				d := time.Duration(ev.Elapsed * float64(time.Second))
				if !strings.Contains(ev.Test, "/") {
					tests[pkg] = append(tests[pkg], TestTime{ev.Test, d})
				}
			}
			continue
		}
		// A package-level event (Test == ""). "pass"/"fail"/"skip" are the
		// package's terminal event; anything else (run/pause/cont, ...) has
		// nothing to flush.
		switch ev.Action {
		case "fail":
			if b := bufs[pkg]; b != nil {
				_, _ = w.Write(b.all.Bytes())
			}
		case "pass", "skip":
			if b := bufs[pkg]; b != nil {
				_, _ = w.Write(b.summary.Bytes())
			}
		default:
			continue
		}
		delete(bufs, pkg)
		if ev.Action != "pass" && ev.Action != "fail" {
			continue
		}
		d := time.Duration(ev.Elapsed * float64(time.Second))
		if exempt[pkg] {
			continue
		}
		c, measured := cpu(pkg)
		if bead, ok := tracked[pkg]; ok {
			runs = append(runs, TrackedRun{Package: pkg, Bead: bead, Elapsed: d, CPU: c, Unmeasured: !measured})
			continue
		}
		// A failed package may never have run a binary (a build failure);
		// it already fails the run, so a missing measurement adds nothing.
		unmeasured := !measured && ev.Action == "pass"
		if unmeasured || c.User > budget {
			ts := tests[pkg]
			sort.Slice(ts, func(i, j int) bool { return ts[i].Elapsed > ts[j].Elapsed })
			if len(ts) > 3 {
				ts = ts[:3]
			}
			over = append(over, Overrun{Package: pkg, Elapsed: d, CPU: c, Unmeasured: unmeasured, Slowest: ts})
		}
	}
	return over, runs, sc.Err()
}
