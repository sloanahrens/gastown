package rehearsal

import (
	"fmt"
	"strings"
	"testing"
)

// compareRecord is a run inside every bar: sixteen ok slings, one passing gate,
// one landing that did not fail CI, three side runs and measured host load.
func compareRecord(id, changeSet string) Record {
	return Record{RunID: id, ChangeSet: changeSet,
		Slings: slingsOf(rep(setSize, 100)...), Gates: gatesOf(GatePass, 400),
		Landings: []Landing{{Bead: "l1", Landed: true}},
		SideRuns: []SideRun{{Seconds: 30}, {Seconds: 40}, {Seconds: 20}},
		Load:     Load{Samples: 10, Max1: 5, P90One: 4}}
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
	after.Slings = append(slingsOf(rep(setSize-1, 110)...), Sling{Bead: "s16", Outcome: SlingKilled, Seconds: 100})
	after.Gates = []Gate{{Bead: "g0", Verdict: GatePass, Seconds: 400}, {Bead: "g1", Verdict: GateFail, Seconds: 500}, {Bead: "g2", Verdict: GateUnavailable}}
	after.SideRuns = []SideRun{{Seconds: 60}, {Seconds: 90}, {Seconds: 45}}
	after.Load = Load{Samples: 10, Max1: 9, P90One: 7}
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
		{"sling p90", "sling p90: 100 | 110"},
		{"gate p90", "gate p90: 400 | 500"},
		{"side run walls", "side run walls: 30, 40, 20 | 60, 90, 45"},
		{"load max", "load max: 5 | 9"},
		{"load p90", "load p90: 4 | 7"},
	}
	assertCompareLines(t, report, rows)
	// The report holds the two verdicts, then the unavailable count directly under
	// them, then the other count lines and one line per figure, and nothing else.
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
		{"sling p90", "sling p90: none | 100"},
		{"gate p90", "gate p90: none | 400"},
		{"side run walls", "side run walls: none | 30, 40, 20"},
		{"load max", "load max: none | 5"},
		{"load p90", "load p90: none | 4"},
	})
}

func TestRehearsalCompareZeroWallGates(t *testing.T) {
	t.Parallel()
	zeroOnly := Record{RunID: "run-zero", ChangeSet: "abc123", Gates: gatesOf(GatePass, rep(10, 0)...)}
	measured := Record{RunID: "run-measured", ChangeSet: "def456", Gates: gatesOf(GatePass, 400, 500)}
	padded := Record{RunID: "run-padded", ChangeSet: "ghi789", Gates: gatesOf(GatePass, append(rep(8, 0), 400, 500)...)}
	assertCompareLines(t, Compare(zeroOnly, measured), []struct{ name, want string }{
		{"only zero-wall gates", "gate p90: none | 500"},
	})
	assertCompareLines(t, Compare(measured, padded), []struct{ name, want string }{
		{"zeros beside measured gates", "gate p90: 500 | 500"},
	})
}
