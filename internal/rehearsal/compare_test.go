package rehearsal

import (
	"fmt"
	"strings"
	"testing"
)

// compareRecord is a run inside every bar: sixteen ok slings, one passing gate,
// one landing that did not fail CI, and measured host load.
func compareRecord(id, changeSet string) Record {
	return Record{RunID: id, ChangeSet: changeSet,
		Slings: slingsOf(rep(setSize, 100)...), Gates: gatesOf(GatePass, 400),
		Landings: []Landing{{Bead: "l1", Landed: true}}, Load: Load{Samples: 10}}
}

// assertCompareLines asserts report holds every line in rows, one parallel
// subtest per line.
func assertCompareLines(t *testing.T, report string, rows []struct{ name, want string }) {
	t.Helper()
	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains("\n"+report, "\n"+tt.want+"\n") {
				t.Errorf("Compare report lacks the line %q:\n%s", tt.want, report)
			}
		})
	}
}

func TestRehearsalCompareReport(t *testing.T) {
	t.Parallel()
	before := compareRecord("run-before", "abc123")
	after := compareRecord("run-after", "def456")
	after.Slings = append(after.Slings[:setSize-1], Sling{Bead: "s16", Outcome: SlingKilled, Seconds: 100})
	after.Gates = []Gate{{Bead: "g0", Verdict: GatePass, Seconds: 400}, {Bead: "g1", Verdict: GateFail, Seconds: 120}, {Bead: "g2", Verdict: GateUnavailable}}
	after.Landings = []Landing{{Bead: "l1", Landed: true, CIFailed: true}}
	after.Stranded, after.FailedCalls, after.Interventions = []string{"seat-3"}, []string{"sling: boom"}, []string{"killed a seat"}

	report := Compare(before, after)
	rows := []struct{ name, want string }{
		{"before verdict", "run-before (abc123): pass"},
		// Take Judge's reason wording from Judge, not a literal.
		{"after verdict", fmt.Sprintf("%s (%s): fail: %s", after.RunID, after.ChangeSet, strings.Join(Judge(after).Reasons, "; "))},
		{"gates unavailable", "gates unavailable: 0 | 1"},
		{"slings not ok", "slings not ok: 0 | 1"},
		{"gates not passing", "gates not passing: 0 | 1"},
		{"stranded seats", "stranded seats: 0 | 1"},
		{"CI failures", "CI failures: 0 | 1"},
		{"failed calls", "failed calls: 0 | 1"},
		{"interventions", "interventions: 0 | 1"},
	}
	assertCompareLines(t, report, rows)
	// The report holds the verdicts, then the unavailable count directly under
	// them, then the count lines in the Goal's order, and nothing else.
	if lines := strings.Split(strings.TrimSuffix(report, "\n"), "\n"); len(lines) != len(rows) || lines[2] != rows[2].want {
		t.Fatalf("report = %q; want %d lines with %q third", lines, len(rows), rows[2].want)
	}
}

func TestRehearsalCompareEmptyLists(t *testing.T) {
	t.Parallel()
	report := Compare(Record{RunID: "run-before", ChangeSet: "abc123"}, compareRecord("run-after", "def456"))
	assertCompareLines(t, report, []struct{ name, want string }{
		{"gates unavailable", "gates unavailable: none | 0"},
		{"slings not ok", "slings not ok: none | 0"},
		{"gates not passing", "gates not passing: none | 0"},
		{"stranded seats", "stranded seats: 0 | 0"},
		{"CI failures", "CI failures: 0 | 0"},
		{"failed calls", "failed calls: 0 | 0"},
		{"interventions", "interventions: 0 | 0"},
	})
}
