package testpolicy

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

const stream = `{"Action":"output","Package":"m/internal/a","Output":"ok\n"}
{"Action":"pass","Package":"m/internal/a","Test":"TestSlow","Elapsed":9.5}
{"Action":"pass","Package":"m/internal/a","Test":"TestFast","Elapsed":0.1}
{"Action":"pass","Package":"m/internal/a","Elapsed":12.0}
{"Action":"pass","Package":"m/internal/b","Elapsed":30.0}
{"Action":"pass","Package":"m/internal/c","Elapsed":1.0}
{"Action":"pass","Package":"m/internal/loaded","Elapsed":13.2}
`

// cpuOf is a CPUSource over a fixed table: the CPU a package's test process
// tree used, as the budget runner's exec wrapper would have recorded it.
func cpuOf(table map[string]CPUTime) CPUSource {
	return func(pkg string) (CPUTime, bool) {
		c, ok := table[pkg]
		return c, ok
	}
}

// TestWatchBudget checks that the budget judges a package's user CPU time,
// not its wall time. internal/loaded is the gt-ty2fy case: 13.2 s of wall
// under a loaded full run, but the same 2.1 s of user CPU it uses alone.
func TestWatchBudget(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cpu := cpuOf(map[string]CPUTime{
		"internal/a":      {User: 12 * time.Second, Sys: time.Second},
		"internal/b":      {User: 30 * time.Second},
		"internal/c":      {User: time.Second},
		"internal/loaded": {User: 2100 * time.Millisecond, Sys: 40 * time.Second},
	})
	over, err := WatchBudget(strings.NewReader(stream), &out, 10*time.Second, map[string]bool{"internal/b": true}, "m", cpu)
	if err != nil {
		t.Fatal(err)
	}
	if len(over) != 1 || over[0].Package != "internal/a" || over[0].CPU.User != 12*time.Second || over[0].Elapsed != 12*time.Second {
		t.Fatalf("overruns = %+v, want internal/a at 12s user CPU only (b is exempt, c and loaded are under)", over)
	}
	if over[0].Unmeasured {
		t.Fatalf("overrun = %+v, want it measured", over[0])
	}
	if len(over[0].Slowest) == 0 || over[0].Slowest[0].Name != "TestSlow" {
		t.Fatalf("slowest = %+v, want TestSlow first", over[0].Slowest)
	}
	if out.String() != "ok\n" {
		t.Fatalf("output = %q, want the Output fields passed through", out.String())
	}
}

// TestWatchBudgetSysTimeNotJudged checks that kernel time does not count
// against the budget. Measured on the gate host, internal/slot's system time
// went from 1.6 s to 11.1 s between runs while its user time stayed at
// 0.1-0.2 s: parallel fork/exec contends in the kernel, and that contention
// is charged as system time to whichever process is waiting.
func TestWatchBudgetSysTimeNotJudged(t *testing.T) {
	t.Parallel()
	const stream = `{"Action":"pass","Package":"m/internal/slot","Elapsed":3.7}
`
	cpu := cpuOf(map[string]CPUTime{"internal/slot": {User: 160 * time.Millisecond, Sys: 11 * time.Second}})
	over, err := WatchBudget(strings.NewReader(stream), io.Discard, 10*time.Second, nil, "m", cpu)
	if err != nil {
		t.Fatal(err)
	}
	if len(over) != 0 {
		t.Fatalf("overruns = %+v, want none: 11 s of system time is contention, not the package", over)
	}
}

// TestWatchBudgetUnmeasured checks that a converted package that passed with
// no CPU measurement is reported rather than let through: a missing
// measurement must not read as "under budget". A failed package (a build
// failure never runs a binary) and an exempt one are not reported.
func TestWatchBudgetUnmeasured(t *testing.T) {
	t.Parallel()
	const stream = `{"Action":"pass","Package":"m/internal/lost","Elapsed":0.5}
{"Action":"fail","Package":"m/internal/broken","Elapsed":0}
{"Action":"pass","Package":"m/internal/exempt","Elapsed":0.5}
{"Action":"skip","Package":"m/internal/notests","Elapsed":0}
`
	over, err := WatchBudget(strings.NewReader(stream), io.Discard, 10*time.Second, map[string]bool{"internal/exempt": true}, "m", cpuOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(over) != 1 || over[0].Package != "internal/lost" || !over[0].Unmeasured {
		t.Fatalf("overruns = %+v, want internal/lost reported as unmeasured only", over)
	}
}

// TestWatchBudgetBuildOutput checks that a Go >=1.24 build-output event (a
// compile/vet failure, keyed by ImportPath rather than Package) is written
// through immediately, so a `make test` failure shows file:line instead of
// only "[build failed]".
func TestWatchBudgetBuildOutput(t *testing.T) {
	t.Parallel()
	const stream = `{"Action":"build-output","ImportPath":"m/internal/bad","Output":"# m/internal/bad\n"}
{"Action":"build-output","ImportPath":"m/internal/bad","Output":"internal/bad/bad.go:10:2: undefined: Foo\n"}
{"Action":"build-fail","ImportPath":"m/internal/bad"}
`
	var out bytes.Buffer
	over, err := WatchBudget(strings.NewReader(stream), &out, 10*time.Second, nil, "m", cpuOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(over) != 0 {
		t.Fatalf("overruns = %+v, want none", over)
	}
	want := "# m/internal/bad\ninternal/bad/bad.go:10:2: undefined: Foo\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

// TestWatchBudgetPassSummaryOnly checks that a passing package's test-level
// output ("=== RUN", "--- PASS") is dropped, and only its package-level
// summary line ("ok  \tpkg\t0.010s") is written — the plain `go test` shape
// (Ruling R10).
func TestWatchBudgetPassSummaryOnly(t *testing.T) {
	t.Parallel()
	const stream = `{"Action":"run","Package":"m/internal/a","Test":"TestX"}
{"Action":"output","Package":"m/internal/a","Test":"TestX","Output":"=== RUN   TestX\n"}
{"Action":"output","Package":"m/internal/a","Test":"TestX","Output":"--- PASS: TestX (0.00s)\n"}
{"Action":"pass","Package":"m/internal/a","Test":"TestX","Elapsed":0}
{"Action":"output","Package":"m/internal/a","Output":"PASS\n"}
{"Action":"output","Package":"m/internal/a","Output":"ok  \tm/internal/a\t0.010s\n"}
{"Action":"pass","Package":"m/internal/a","Elapsed":0.01}
`
	var out bytes.Buffer
	if _, err := WatchBudget(strings.NewReader(stream), &out, 10*time.Second, nil, "m", cpuOf(nil)); err != nil {
		t.Fatal(err)
	}
	want := "PASS\nok  \tm/internal/a\t0.010s\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q (test-level output must be dropped)", out.String(), want)
	}
}

// TestWatchBudgetFailWholePackageInterleaved checks that a failing package's
// whole output (test-level included) is written, and that two packages'
// events interleaved in the input still produce each package's output
// contiguously, in package-terminal-event order.
func TestWatchBudgetFailWholePackageInterleaved(t *testing.T) {
	t.Parallel()
	const stream = `{"Action":"run","Package":"m/internal/x","Test":"TestX"}
{"Action":"run","Package":"m/internal/y","Test":"TestY"}
{"Action":"output","Package":"m/internal/x","Test":"TestX","Output":"=== RUN   TestX\n"}
{"Action":"output","Package":"m/internal/y","Test":"TestY","Output":"=== RUN   TestY\n"}
{"Action":"output","Package":"m/internal/x","Test":"TestX","Output":"--- FAIL: TestX (0.00s)\n"}
{"Action":"fail","Package":"m/internal/x","Test":"TestX","Elapsed":0}
{"Action":"output","Package":"m/internal/y","Test":"TestY","Output":"--- PASS: TestY (0.00s)\n"}
{"Action":"pass","Package":"m/internal/y","Test":"TestY","Elapsed":0}
{"Action":"output","Package":"m/internal/x","Output":"FAIL\n"}
{"Action":"output","Package":"m/internal/x","Output":"FAIL\tm/internal/x\t0.01s\n"}
{"Action":"fail","Package":"m/internal/x","Elapsed":0.01}
{"Action":"output","Package":"m/internal/y","Output":"ok  \tm/internal/y\t0.01s\n"}
{"Action":"pass","Package":"m/internal/y","Elapsed":0.01}
`
	var out bytes.Buffer
	if _, err := WatchBudget(strings.NewReader(stream), &out, 10*time.Second, nil, "m", cpuOf(nil)); err != nil {
		t.Fatal(err)
	}
	want := "=== RUN   TestX\n--- FAIL: TestX (0.00s)\nFAIL\nFAIL\tm/internal/x\t0.01s\n" +
		"ok  \tm/internal/y\t0.01s\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q (x's block contiguous, then y's summary)", out.String(), want)
	}
}

func TestEnforceBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		load1     float64
		loadKnown bool
		strict    bool
		want      bool
	}{
		{"quiet host", 3.5, true, false, true},
		{"load at ncpu", 16, true, false, false},
		{"loaded host", 30, true, false, false},
		{"loaded host, strict", 30, true, true, true},
		{"load unknown", 0, false, false, false},
		{"load unknown, strict", 0, false, true, true},
	} {
		got, why := EnforceBudget(tc.load1, tc.loadKnown, 16, tc.strict)
		if got != tc.want || why == "" {
			t.Errorf("%s: EnforceBudget(%v, %v, 16, %v) = %v, %q; want %v with a reason", tc.name, tc.load1, tc.loadKnown, tc.strict, got, why, tc.want)
		}
	}
}

// TestBudgetFails checks that a CPU overrun fails only an enforced run, and
// a missing measurement fails every run.
func TestBudgetFails(t *testing.T) {
	t.Parallel()
	cpu := []Overrun{{Package: "internal/a", CPU: CPUTime{User: 11 * time.Second}}}
	unmeasured := []Overrun{{Package: "internal/b", Unmeasured: true}}
	for _, tc := range []struct {
		name    string
		over    []Overrun
		enforce bool
		want    bool
	}{
		{"none", nil, true, false},
		{"cpu overrun, enforced", cpu, true, true},
		{"cpu overrun, reported", cpu, false, false},
		{"unmeasured, reported", unmeasured, false, true},
		{"both, reported", append(append([]Overrun{}, cpu...), unmeasured...), false, true},
	} {
		if got := BudgetFails(tc.over, tc.enforce); got != tc.want {
			t.Errorf("%s: BudgetFails(enforce=%v) = %v, want %v", tc.name, tc.enforce, got, tc.want)
		}
	}
}

func TestParseLoad1(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]float64{
		"1.23 4.56 7.89 2/345 6789\n": 1.23,
		"{ 27.65 30.07 30.59 }\n":     27.65,
	} {
		got, err := ParseLoad1(in)
		if err != nil || got != want {
			t.Errorf("ParseLoad1(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "{ }", "busy"} {
		if _, err := ParseLoad1(in); err == nil {
			t.Errorf("ParseLoad1(%q) succeeded, want an error", in)
		}
	}
}
