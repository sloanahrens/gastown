package testpolicy

import (
	"bytes"
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
// bead and time on every run, over the budget or not.
func TestWatchBudgetTracked(t *testing.T) {
	t.Parallel()
	const stream = `{"Action":"pass","Package":"m/internal/git","Elapsed":41.5}
{"Action":"pass","Package":"m/internal/fast","Elapsed":2.0}
{"Action":"pass","Package":"m/internal/slow","Elapsed":12.0}
{"Action":"pass","Package":"m/internal/c","Elapsed":1.0}
`
	tracked := map[string]string{"internal/git": "gt-22hdp.35", "internal/fast": "gt-x"}
	var out bytes.Buffer
	over, runs, err := WatchBudgetTracked(strings.NewReader(stream), &out, 10*time.Second, nil, tracked, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(over) != 1 || over[0].Package != "internal/slow" {
		t.Fatalf("overruns = %+v, want internal/slow only", over)
	}
	want := []TrackedRun{
		{Package: "internal/git", Bead: "gt-22hdp.35", Elapsed: 41500 * time.Millisecond},
		{Package: "internal/fast", Bead: "gt-x", Elapsed: 2 * time.Second},
	}
	if len(runs) != len(want) || runs[0] != want[0] || runs[1] != want[1] {
		t.Fatalf("tracked = %+v, want %+v", runs, want)
	}
}
