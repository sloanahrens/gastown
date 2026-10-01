package testpolicy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
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
	res, err := WatchBudgetTracked(r, w, budget, exempt, nil, module, cpu)
	return res.Over, err
}

// SlowRun is a judged package whose wall time was over the budget although
// its user CPU was not. It is reported, never failed: wall time depends on
// the host, but work outside the test binary's waited-for process tree (a
// daemon it started, a server it waited on) shows up nowhere else.
type SlowRun struct {
	Package string
	Elapsed time.Duration
	CPU     CPUTime
}

// PkgTime is one judged package's measurement.
type PkgTime struct {
	Package string
	// Wall is the package's test wall time; CPU is what its test process
	// tree used, zero and Measured false when it passed unmeasured.
	Wall     time.Duration
	CPU      CPUTime
	Measured bool
}

// BudgetResult is what one budget run found.
type BudgetResult struct {
	// Over are the packages that fail the budget.
	Over []Overrun
	// Tracked are the overbudget.txt packages' measurements, reported on
	// every run.
	Tracked []TrackedRun
	// SlowWall are the judged packages over the budget in wall time only.
	SlowWall []SlowRun
	// Judged is every judged package that ran, for the gate's idle check
	// (IdleJudged): a gate whose packages wait rather than compute names
	// the host, which no single package's measurement can (gt-2ycne.1).
	Judged []PkgTime
}

// IdleTop is how many packages the idle report names.
const IdleTop = 5

// IdleRatioFactor and IdleWall are the gate's idle check: judged packages
// whose median waited IdleRatioFactor times its CPU time for over IdleWall of
// wall were waiting on the host, not computing, and the gate's wall time
// reads as a verdict on the tree when it is not one (gt-2ycne.1).
const (
	IdleRatioFactor = 10
	IdleWall        = 20 * time.Second
)

// IdleReport is what one run's judged packages say about the host.
type IdleReport struct {
	// Packages is how many packages the medians are over: the judged ones
	// that ran with a CPU measurement. An unmeasured package's CPU is
	// unknown, not zero, so it takes no part.
	Packages int
	// Wall and CPU are those packages' medians, taken one field at a time.
	Wall, CPU time.Duration
	// Waiting reports whether the median package waited on the host.
	Waiting bool
	// Top are the measured judged packages with the most wall time, most
	// first, at most IdleTop of them.
	Top []PkgTime
}

// IdleJudged judges a run's judged packages. Waiting is true when the median
// measured package ran over IdleWall of wall and over IdleRatioFactor times
// its own CPU time: the time was spent waiting (a scan per executable, a
// throttled process, a lock), and the run's wall time is evidence about the
// host rather than about the tests.
func IdleJudged(times []PkgTime) IdleReport {
	measured := make([]PkgTime, 0, len(times))
	for _, p := range times {
		if p.Measured {
			measured = append(measured, p)
		}
	}
	rep := IdleReport{Packages: len(measured)}
	if len(measured) == 0 {
		return rep
	}
	walls := make([]time.Duration, len(measured))
	cpus := make([]time.Duration, len(measured))
	for i, p := range measured {
		walls[i] = p.Wall
		cpus[i] = p.CPU.User + p.CPU.Sys
	}
	rep.Wall, rep.CPU = medianDuration(walls), medianDuration(cpus)
	rep.Waiting = rep.Wall > IdleWall && rep.Wall > IdleRatioFactor*rep.CPU
	sort.SliceStable(measured, func(i, j int) bool { return measured[i].Wall > measured[j].Wall })
	if len(measured) > IdleTop {
		measured = measured[:IdleTop]
	}
	rep.Top = measured
	return rep
}

// medianDuration returns the middle value (the lower middle for an even
// count).
func medianDuration(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)-1)/2]
}

// WatchBudgetTracked is WatchBudget that also exempts the packages in tracked
// (overbudget.txt: package to bead id) and returns the time each of them
// took, so the caller can report them on every run. A tracked package that
// passes without a CPU measurement still fails, like any converted package.
func WatchBudgetTracked(r io.Reader, w io.Writer, budget time.Duration, exempt map[string]bool, tracked map[string]string, module string, cpu CPUSource) (BudgetResult, error) {
	var res BudgetResult
	tests := map[string][]TestTime{}
	bufs := map[string]*pkgOutput{}
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
		res.Judged = append(res.Judged, PkgTime{Package: pkg, Wall: d, CPU: c, Measured: measured})
		// A failed package may never have run a binary (a build failure);
		// it already fails the run, so a missing measurement adds nothing.
		unmeasured := !measured && ev.Action == "pass"
		bead, isTracked := tracked[pkg]
		switch {
		case isTracked && !unmeasured:
			res.Tracked = append(res.Tracked, TrackedRun{Package: pkg, Bead: bead, Elapsed: d, CPU: c, Unmeasured: !measured})
		case unmeasured || c.User > budget:
			ts := tests[pkg]
			sort.Slice(ts, func(i, j int) bool { return ts[i].Elapsed > ts[j].Elapsed })
			if len(ts) > 3 {
				ts = ts[:3]
			}
			res.Over = append(res.Over, Overrun{Package: pkg, Elapsed: d, CPU: c, Unmeasured: unmeasured, Slowest: ts})
		case d > budget:
			res.SlowWall = append(res.SlowWall, SlowRun{Package: pkg, Elapsed: d, CPU: c})
		}
	}
	return res, sc.Err()
}

// StrictBudgetEnv, set to 1, makes the budget runner fail a CPU overrun
// whatever the host load (EnforceBudget).
const StrictBudgetEnv = "GATE_STRICT_BUDGET"

// EnforceBudget reports whether a run's user-CPU overruns fail it, and why.
// User CPU depends on load far less than wall time, but not on nothing: an
// oversubscribed host shares cores and caches, so a package can cross the
// budget beside six other gates and not alone (gt-ik4a1.1). The budget is
// enforced when the 1-minute load average at the start of the run is below
// ncpu, or when strict (GATE_STRICT_BUDGET=1) is set; otherwise the overruns
// are reported and the run passes. A load that could not be read is not
// evidence of a quiet host, so it reports too.
func EnforceBudget(load1 float64, loadKnown bool, ncpu int, strict bool) (bool, string) {
	switch {
	case strict:
		return true, StrictBudgetEnv + "=1"
	case !loadKnown:
		return false, "host load unknown"
	case load1 < float64(ncpu):
		return true, fmt.Sprintf("load %.1f < %d CPUs", load1, ncpu)
	default:
		return false, fmt.Sprintf("load %.1f >= %d CPUs", load1, ncpu)
	}
}

// BudgetFails reports whether the overruns fail the run. A package that
// passed without a CPU measurement always fails, because host load has
// nothing to do with a missing measurement; a CPU overrun fails only when
// enforce is set.
func BudgetFails(over []Overrun, enforce bool) bool {
	for _, o := range over {
		if o.Unmeasured || enforce {
			return true
		}
	}
	return false
}

// ParseLoad1 reads the 1-minute load average from Linux's /proc/loadavg
// ("1.23 4.56 7.89 2/345 6789") or macOS's `sysctl -n vm.loadavg`
// ("{ 1.23 4.56 7.89 }").
func ParseLoad1(s string) (float64, error) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(fields) == 0 {
		return 0, fmt.Errorf("no load average in %q", s)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("load average %q: %w", s, err)
	}
	return v, nil
}
