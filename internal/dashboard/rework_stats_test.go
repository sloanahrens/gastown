package dashboard

import (
	"strconv"
	"strings"
	"testing"
)

// omReworkStatsTable is the table ~/.claude/tools/om-rework-stats.py printed
// for the reviews in omReworkStatsReviews (gt-weelq). To regenerate it after
// the script's grouping changes, build a throwaway HOME the script reads: a
// rig repo at gt/gastown/crew/sloan whose origin/main history sets .om.json's
// threshold to 0.7 at 160h and to 0.8 at 48h before reworkNow, a gt/daemon/
// daemon.log "landing rejected (review)" line per rejection, and a
// gt/.runtime/landings/gastown.jsonl record per approval; then run
// `om-rework-stats.py --rig gastown --since 2026-10-07` and paste its table.
const omReworkStatsTable = `
  bar  reviews  1st-try ok  rework %  reviews/landed bead  loops out  mean score  rejected mean
  0.7        7     0/3           71%                 2.00          1        0.60           0.50
  0.8        7     1/4           71%                 2.00          0        0.61           0.50
    ?        1     1/1            0%                 1.00          0        0.91           0.00
`

// omReworkStatsReviews avoids what only the Go side knows: a verdict logged
// twice for one attempt, and an approval the landing bounced for its findings.
func omReworkStatsReviews() []ReworkReview {
	const approve, reject = "approve", "request_changes"
	return []ReworkReview{
		// gt-u: reviewed before any bar was set, so its bar is unknown.
		rv("gt-u", 1, approve, 0.91, 0, 166),
		// gt-a: lands on its second review under 0.7.
		rv("gt-a", 1, reject, 0.55, 0.7, 150), rv("gt-a", 2, approve, 0.82, 0.7, 140),
		// gt-b: three rejections under 0.7 and no approval: a loop that ran out.
		rv("gt-b", 1, reject, 0.4, 0.7, 120), rv("gt-b", 2, reject, 0.45, 0.7, 110), rv("gt-b", 3, reject, 0.5, 0.7, 100),
		// gt-c: first reviewed under 0.7, approved after the bar moved to 0.8.
		rv("gt-c", 1, reject, 0.6, 0.7, 60), rv("gt-c", 2, approve, 0.88, 0.8, 20),
		// gt-d: lands first try under 0.8.
		rv("gt-d", 1, approve, 0.9, 0.8, 30),
		// gt-e: two rejections, then lands.
		rv("gt-e", 1, reject, 0.5, 0.8, 12), rv("gt-e", 2, reject, 0.65, 0.8, 10), rv("gt-e", 3, approve, 0.85, 0.8, 8),
		// gt-f: rejected once, not yet approved.
		rv("gt-f", 1, reject, 0.7, 0.8, 5),
		// gt-g: rejected twice, one short of a loop that ran out.
		rv("gt-g", 1, reject, 0.3, 0.8, 4), rv("gt-g", 2, reject, 0.35, 0.8, 3),
	}
}

// scriptRow is one bar's line of the table om-rework-stats.py prints.
type scriptRow struct {
	bar                               float64
	reviews, firstTry, beads          int
	reworkPct, loopsOut               int
	reviewsPerLanded, meanScoreFigure float64
}

// parseScriptRows reads the per-bar rows of the script's table: bar, reviews,
// first-try/beads, rework %, reviews per landed bead, loops out, mean score and
// rejected mean. A bar of "?" is the unknown bar, which is 0 in the payload.
func parseScriptRows(t *testing.T, table string) []scriptRow {
	t.Helper()
	var rows []scriptRow
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] == "bar" {
			continue
		}
		if len(f) != 8 {
			t.Fatalf("table row %q has %d columns, want 8", line, len(f))
		}
		ints := func(s string) int {
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("row %q: %v", line, err)
			}
			return n
		}
		floats := func(s string) float64 {
			x, err := strconv.ParseFloat(s, 64)
			if err != nil {
				t.Fatalf("row %q: %v", line, err)
			}
			return x
		}
		r := scriptRow{reviews: ints(f[1]), reworkPct: ints(strings.TrimSuffix(f[3], "%")), loopsOut: ints(f[5]),
			reviewsPerLanded: floats(f[4]), meanScoreFigure: floats(f[6])}
		if f[0] != "?" {
			r.bar = floats(f[0])
		}
		ft := strings.SplitN(f[2], "/", 2)
		if len(ft) != 2 {
			t.Fatalf("first-try column %q in %q", f[2], line)
		}
		r.firstTry, r.beads = ints(ft[0]), ints(ft[1])
		rows = append(rows, r)
	}
	return rows
}

// round2 is the script's %.2f.
func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// The table is the figure tools/om-rework-stats.py prints, and the grouping
// lives in two languages with nothing else holding them together. This holds
// each bar's numbers to the table the script printed for the same reviews. The
// script lives outside the repo and a unit test runs no python, so the table is
// a recording: it ties the Go grouping to the script's figures for these
// reviews, and it is stale when the script's grouping moves (gt-weelq).
func TestAggregateReworkMatchesOmReworkStats(t *testing.T) {
	t.Parallel()

	rows := parseScriptRows(t, omReworkStatsTable)
	want := AggregateRework(omReworkStatsReviews(), reworkNow).Windows[0].Bars
	if len(rows) != len(want) {
		t.Fatalf("the script's table has %d bars, AggregateRework built %d: %+v", len(rows), len(want), want)
	}
	for i, w := range want {
		g := rows[i]
		pct := int(100*float64(w.Rejected)/float64(w.Reviews) + 0.5)
		if g.bar != w.Bar || g.reviews != w.Reviews || g.firstTry != w.FirstTry || g.beads != w.Beads ||
			g.loopsOut != w.LoopsOut || g.reworkPct != pct ||
			!near(g.reviewsPerLanded, round2(w.ReviewsPerLanded)) || !near(g.meanScoreFigure, round2(w.MeanScore)) {
			t.Errorf("bar %v: the script printed %+v, AggregateRework built %+v", w.Bar, g, w)
		}
	}
}
