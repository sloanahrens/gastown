package cmd

import (
	"strings"
	"testing"
)

// TestLandOmStatsClassesSharesTheSnapshot: --classes prints the class-rate table
// after the report from the same read of the records: the records path is opened
// once, the report's own unparseable-line sentence is there once, and a run
// without the flag prints no class table (gt-izivy6).
func TestLandOmStatsClassesSharesTheSnapshot(t *testing.T) {
	t.Parallel()
	records, log := omStatsFixtureLines(t)
	town := func() (string, error) { return "/town", nil }
	run := func(classes bool) (string, []string) {
		fake := &landOmStatsFakeFS{files: map[string]string{"/town/reviews.jsonl": records + "not json\n", "/town/daemon.log": log}}
		var out strings.Builder
		if err := landOmStatsReportFrom(fake, town, &out, "", "/town/reviews.jsonl", "/town/daemon.log", false, classes); err != nil {
			t.Fatalf("landOmStatsReportFrom(classes %v): %v", classes, err)
		}
		return out.String(), fake.opened
	}
	with, opened := run(true)
	opens := 0
	for _, name := range opened {
		if name == "/town/reviews.jsonl" {
			opens++
		}
	}
	if opens != 1 {
		t.Errorf("the records path was opened %d times with --classes, want once (opened: %v)", opens, opened)
	}
	if n := strings.Count(with, "unparseable review-record line(s) skipped"); n != 1 {
		t.Errorf("the unparseable-line sentence appears %d times, want the report's own, once:\n%s", n, with)
	}
	if !strings.Contains(with, "No-text") || !strings.Contains(with, "2026-W42") {
		t.Errorf("--classes printed no class table:\n%s", with)
	}
	if without, _ := run(false); strings.Contains(without, "No-text") {
		t.Errorf("a run without --classes printed the class table:\n%s", without)
	}
}
