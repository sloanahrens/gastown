package beads

import (
	"sync"
	"testing"
)

// resetCensus zeroes the process-wide counters around a test so the counts it
// asserts are its own.
func resetCensus(t *testing.T) {
	t.Helper()
	CensusSnapshotAndReset()
	t.Cleanup(func() { CensusSnapshotAndReset() })
}

// censusByVerb folds the rows into one count per verb.
func censusByVerb(rows []CensusRow) map[string]int {
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.Verb] += row.Count
	}
	return counts
}

// TestCensusCountsVerbsFromTheSeam drives a list and a mol wisp call through a
// fake bd and checks the census named both verbs and kept the seam out of the
// caller.
func TestCensusCountsVerbsFromTheSeam(t *testing.T) {
	resetCensus(t)
	r := newRecorder(func([]string) reply { return reply{stdout: "[]"} })
	b := newRecordedBeads(t.TempDir(), r)

	if _, err := b.run("list", "--json", "--status=open"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := b.run("mol", "wisp", "list", "--json"); err != nil {
		t.Fatalf("mol wisp list: %v", err)
	}

	rows := CensusSnapshotAndReset()
	got := censusByVerb(rows)
	want := map[string]int{"list": 1, "mol wisp": 1}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want verbs %v", rows, want)
	}
	for verb, count := range want {
		if got[verb] != count {
			t.Errorf("verb %q counted %d, want %d (rows %+v)", verb, got[verb], count, rows)
		}
	}
	for _, row := range rows {
		if row.Caller == "" || row.Caller == censusUnknown {
			t.Errorf("row %+v did not name a caller outside the seam", row)
		}
	}
}

// TestCensusCountsFlatRetryAsSecondCall: a --flat retry is a second bd
// subprocess, so the census counts it twice.
func TestCensusCountsFlatRetryAsSecondCall(t *testing.T) {
	resetCensus(t)
	r := newRecorder(func(args []string) reply {
		if hasArg(args, "--flat") {
			return reply{stderr: "Error: unknown flag: --flat\n", err: exitError{code: 1}}
		}
		return reply{stdout: "[]"}
	})
	b := newRecordedBeads(t.TempDir(), r)

	if _, err := b.run("list", "--json"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if calls := len(r.calls()); calls != 2 {
		t.Fatalf("bd calls = %d, want 2 (--flat attempt and retry)", calls)
	}
	rows := CensusSnapshotAndReset()
	if len(rows) != 1 || rows[0].Verb != "list" || rows[0].Count != 2 {
		t.Fatalf("rows = %+v, want one list row counted twice", rows)
	}
}

// TestCensusSnapshotSortsAndResets records concurrently and checks the rows
// come back most frequent first, then that a second snapshot is empty.
func TestCensusSnapshotSortsAndResets(t *testing.T) {
	resetCensus(t)
	args := map[string][]string{
		"show":     {"show", "gt-1"},
		"list":     {"list", "--json"},
		"mol wisp": {"mol", "wisp", "list"},
	}
	counts := map[string]int{"show": 50, "list": 30, "mol wisp": 20}

	var wg sync.WaitGroup
	for verb, n := range counts {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(argv []string) {
				defer wg.Done()
				censusRecord(argv)
			}(args[verb])
		}
	}
	wg.Wait()

	wantTotal := 0
	for _, n := range counts {
		wantTotal += n
	}
	if total := CensusTotal(); total != wantTotal {
		t.Fatalf("CensusTotal = %d, want %d", total, wantTotal)
	}

	rows := CensusSnapshotAndReset()
	if len(rows) != len(counts) {
		t.Fatalf("rows = %+v, want one per verb", rows)
	}
	// "list" and "mol wisp" tie on count, so the verb breaks the tie.
	wantOrder := []string{"show", "list", "mol wisp"}
	for i, verb := range wantOrder {
		if rows[i].Verb != verb || rows[i].Count != counts[verb] {
			t.Errorf("row %d = %+v, want verb %q counted %d (rows %+v)", i, rows[i], verb, counts[verb], rows)
		}
	}

	if after := CensusSnapshotAndReset(); len(after) != 0 {
		t.Errorf("second snapshot = %+v, want none", after)
	}
	if total := CensusTotal(); total != 0 {
		t.Errorf("CensusTotal after reset = %d, want 0", total)
	}
}

// TestCensusVerb checks the verb is the first argument that is neither a flag
// nor a flag's value, and that two-word verbs keep their second word.
func TestCensusVerb(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"plain verb", []string{"show", "gt-1"}, "show"},
		{"leading boolean flag", []string{"--allow-stale", "list", "--json"}, "list"},
		{"value of a separate-value flag", []string{"--db", "/tmp/x.db", "list", "--json"}, "list"},
		{"value that looks like a verb", []string{"-C", "show", "list"}, "list"},
		{"attached flag value", []string{"--db=/tmp/x.db", "close", "gt-1"}, "close"},
		{"two-word verb", []string{"mol", "wisp", "list", "--json"}, "mol wisp"},
		{"two-word verb after a flag", []string{"--json", "comments", "add", "gt-1"}, "comments add"},
		{"two-word verb keeps two words", []string{"gate", "wait", "g-1", "--timeout", "5"}, "gate wait"},
		{"two-word verb without a second word", []string{"mol", "--json"}, "mol"},
		{"one-word verb keeps one word", []string{"dep", "list"}, "dep list"},
		{"separator before the verb", []string{"--", "list", "--json"}, "list"},
		{"flags only", []string{"--json"}, censusUnknown},
		{"no args", nil, censusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := censusVerb(tt.args); got != tt.want {
				t.Errorf("censusVerb(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// TestCensusCallerFromFrames checks the caller walk skips this package, trims
// the module prefix, and strips closure suffixes.
func TestCensusCallerFromFrames(t *testing.T) {
	seam := func(names ...string) []string {
		out := make([]string, 0, len(names))
		for _, name := range names {
			out = append(out, censusPkgPath+"."+name)
		}
		return out
	}
	tests := []struct {
		name   string
		frames []string
		want   string
	}{
		{
			name: "skips the seam",
			frames: append(
				seam("censusRecord", "(*Beads).runBdOnce.func1", "(*Beads).runBdOnce", "(*Beads).runWithStdin.func2"),
				"github.com/steveyegge/gastown/internal/daemon.(*patrolScanHost).AssignedWork",
			),
			want: "daemon.(*patrolScanHost).AssignedWork",
		},
		{
			name: "skips beads subpackages",
			frames: []string{
				"github.com/steveyegge/gastown/internal/beads/testutil.Fake",
				"github.com/steveyegge/gastown/internal/cmd.showCmd",
			},
			want: "cmd.showCmd",
		},
		{
			name:   "strips nested closure suffixes",
			frames: []string{"github.com/steveyegge/gastown/internal/daemon.(*patrol).Tick.func2.1"},
			want:   "daemon.(*patrol).Tick",
		},
		{
			name:   "keeps a function without the module prefix",
			frames: []string{"main.main"},
			want:   "main.main",
		},
		{
			name:   "skips runtime frames",
			frames: []string{"runtime.Callers", "github.com/steveyegge/gastown/internal/daemon.Loop"},
			want:   "daemon.Loop",
		},
		{
			name:   "unknown when every frame is the seam",
			frames: seam("censusRecord", "(*Beads).runBdOnce"),
			want:   censusUnknown,
		},
		{
			name:   "unknown without frames",
			frames: nil,
			want:   censusUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := censusCallerFromFrames(tt.frames); got != tt.want {
				t.Errorf("censusCallerFromFrames(%q) = %q, want %q", tt.frames, got, tt.want)
			}
		})
	}
}

// hasArg reports whether args holds want.
func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// BenchmarkCensusRecord measures the per-call cost the census adds to a bd
// call; the bead's budget is a stack walk under ~20 microseconds.
func BenchmarkCensusRecord(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		censusRecord([]string{"show", "gt-1"})
	}
	CensusSnapshotAndReset()
}
