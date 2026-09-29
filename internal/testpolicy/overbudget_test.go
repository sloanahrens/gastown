package testpolicy

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestParseOverBudget(t *testing.T) {
	t.Parallel()
	const list = `# comment
internal/git gt-22hdp.35   # 30-45 s under gate load

internal/a bd-x1
internal/nobead
`
	got, err := ParseOverBudget(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	want := []OverBudgetEntry{
		{Package: "internal/git", Bead: "gt-22hdp.35", Line: 2},
		{Package: "internal/a", Bead: "bd-x1", Line: 4},
		{Package: "internal/nobead", Bead: "", Line: 5},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Each case is one overbudget.txt fixture checked against a fixed picture of
// the repo: internal/clean passes every rule, internal/dirty has violations,
// internal/listed is in unconverted.txt.
func TestCheckOverBudget(t *testing.T) {
	t.Parallel()
	violations := map[string]int{"internal/clean": 0, "internal/other": 0, "internal/dirty": 2, "internal/listed": 0}
	unconverted := map[string]bool{"internal/listed": true}
	cases := []struct {
		name string
		list string
		max  int
		want string // substring of the single expected error; "" means none
	}{
		{"valid entry", "internal/clean gt-22hdp.35 # note\n", 1, ""},
		{"no bead id", "internal/clean\n", 1, "bead id"},
		{"malformed bead id", "internal/clean GT_22\n", 1, "bead id"},
		{"entry fails a rule", "internal/dirty gt-abc\n", 1, "fails 2 rule"},
		{"entry also unconverted", "internal/listed gt-abc\n", 1, "also in unconverted.txt"},
		{"duplicate entry", "internal/clean gt-abc\ninternal/clean gt-def\n", 2, "listed twice"},
		{"not a package", "internal/gone gt-abc\n", 1, "not a Go package"},
		{"ratchet grew", "internal/clean gt-abc\ninternal/other gt-def\n", 1, "at most 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			entries, err := ParseOverBudget(strings.NewReader(c.list))
			if err != nil {
				t.Fatal(err)
			}
			errs := CheckOverBudget(entries, unconverted, violations, c.max)
			if c.want == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %q, want none", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0], c.want) {
				t.Fatalf("errors = %q, want one containing %q", errs, c.want)
			}
		})
	}
}

// A package in overbudget.txt is exempt from the budget but reported with its
// bead, CPU and wall time on every run, over the budget or not. Like any
// converted package, one that passes without a CPU measurement fails.
func TestWatchBudgetTracked(t *testing.T) {
	t.Parallel()
	const stream = `{"Action":"pass","Package":"m/internal/git","Elapsed":41.5}
{"Action":"pass","Package":"m/internal/fast","Elapsed":2.0}
{"Action":"pass","Package":"m/internal/slow","Elapsed":12.0}
{"Action":"pass","Package":"m/internal/c","Elapsed":1.0}
`
	tracked := map[string]string{"internal/git": "gt-22hdp.35", "internal/fast": "gt-x"}
	cpu := cpuOf(map[string]CPUTime{
		"internal/git":  {User: 15 * time.Second, Sys: 300 * time.Second},
		"internal/slow": {User: 12 * time.Second},
		"internal/c":    {User: time.Second},
	})
	var out bytes.Buffer
	res, err := WatchBudgetTracked(strings.NewReader(stream), &out, 10*time.Second, nil, tracked, "m", cpu)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Over) != 2 || res.Over[0].Package != "internal/fast" || !res.Over[0].Unmeasured || res.Over[1].Package != "internal/slow" || res.Over[1].Unmeasured {
		t.Fatalf("overruns = %+v, want internal/fast unmeasured, then internal/slow over", res.Over)
	}
	want := []TrackedRun{
		{Package: "internal/git", Bead: "gt-22hdp.35", Elapsed: 41500 * time.Millisecond, CPU: CPUTime{User: 15 * time.Second, Sys: 300 * time.Second}},
	}
	if len(res.Tracked) != len(want) || res.Tracked[0] != want[0] {
		t.Fatalf("tracked = %+v, want %+v", res.Tracked, want)
	}
}

// TestWatchBudgetSlowWall checks that every judged package whose wall time is
// over the budget is reported, whatever its CPU: wall is report-only, but work
// done outside the test binary's waited-for process tree (a daemon it
// started, a server it waits on) shows up nowhere else.
func TestWatchBudgetSlowWall(t *testing.T) {
	t.Parallel()
	const stream = `{"Action":"pass","Package":"m/internal/waits","Elapsed":13.2}
{"Action":"pass","Package":"m/internal/quick","Elapsed":1.0}
{"Action":"pass","Package":"m/internal/over","Elapsed":12.0}
{"Action":"pass","Package":"m/internal/exempt","Elapsed":30.0}
`
	cpu := cpuOf(map[string]CPUTime{
		"internal/waits": {User: 2 * time.Second},
		"internal/quick": {User: time.Second},
		"internal/over":  {User: 11 * time.Second},
	})
	res, err := WatchBudgetTracked(strings.NewReader(stream), io.Discard, 10*time.Second, map[string]bool{"internal/exempt": true}, nil, "m", cpu)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Over) != 1 || res.Over[0].Package != "internal/over" {
		t.Fatalf("overruns = %+v, want internal/over only", res.Over)
	}
	want := SlowRun{Package: "internal/waits", Elapsed: 13200 * time.Millisecond, CPU: CPUTime{User: 2 * time.Second}}
	if len(res.SlowWall) != 1 || res.SlowWall[0] != want {
		t.Fatalf("slow wall = %+v, want [%+v] (an overrun already prints its wall; exempt packages are not judged)", res.SlowWall, want)
	}
}
