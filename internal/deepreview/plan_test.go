package deepreview

import (
	"fmt"
	"strings"
	"testing"
)

// testLedger is the fixture scripts/deep-review_test.sh plans from: two risky
// landings, one without a risk path, and a torn last line.
const testLedger = `{"bead":"gt-1","head":"h1","landed_commit":"c1","landed_at":"2026-10-10T01:00:00.5Z","risk_paths":["internal/daemon/x.go"]}
{"bead":"gt-2","head":"h2","landed_commit":"c2","landed_at":"2026-10-10T02:00:00Z"}
{"bead":"gt-3","head":"h3","landed_commit":"c3","landed_at":"2026-10-10T03:00:00.123456Z","risk_paths":["internal/land/y.go"]}
{torn
`

// instants is the shell test's tie-breaking fixture: a fraction after a whole
// second, and a +02:00 offset that is really 04:30Z.
const instants = `{"bead":"gt-a","head":"ha","landed_commit":"ca","landed_at":"2026-10-10T05:00:00.542005Z","risk_paths":["x"]}
{"bead":"gt-b","head":"hb","landed_commit":"cb","landed_at":"2026-10-10T05:00:00.9Z","risk_paths":["x"]}
{"bead":"gt-c","head":"hc","landed_commit":"cc","landed_at":"2026-10-10T06:30:00+02:00","risk_paths":["x"]}
`

// beadsOf names the plan's landings in order, the way the shell test reads
// landings.jsonl.
func beadsOf(p Plan) string {
	beads := make([]string, len(p.Landings))
	for i, r := range p.Landings {
		beads[i] = r.Bead
	}
	return strings.Join(beads, " ")
}

func TestBuildPlan(t *testing.T) {
	t.Parallel()
	now := utc(2026, 10, 10, 10, 0, 0, 0)
	for _, tc := range []struct {
		name        string
		notes       string
		ledger      string
		beads       string
		since       string
		cursor      string
		until       string
		records     int
		unparsed    int
		quiet       bool
		wantErrPart string
	}{
		{
			name:     "the default window is the day before now",
			ledger:   testLedger,
			beads:    "gt-1 gt-3",
			since:    "2026-10-09T10:00:00Z",
			until:    "2026-10-10T03:00:00.123456Z",
			records:  3,
			unparsed: 1,
		},
		{
			name:     "a cursor is the window, its path echoed",
			notes:    "cursor: 2026-10-10T01:00:00.5Z internal/a/two.go\n",
			ledger:   testLedger,
			beads:    "gt-3",
			since:    "2026-10-10T01:00:00.5Z",
			cursor:   "internal/a/two.go",
			until:    "2026-10-10T03:00:00.123456Z",
			records:  3,
			unparsed: 1,
		},
		{
			name:     "nothing after the cursor is quiet",
			notes:    "cursor: 2026-10-10T03:00:00.123456Z internal/a/two.go\n",
			ledger:   testLedger,
			since:    "2026-10-10T03:00:00.123456Z",
			cursor:   "internal/a/two.go",
			until:    "2026-10-10T03:00:00.123456Z",
			records:  3,
			unparsed: 1,
			quiet:    true,
		},
		{
			name:   "a landing without a risk path is not reviewed",
			notes:  "cursor: 2026-10-10T01:00:00.5Z internal/a/one.go\n",
			ledger: `{"bead":"gt-2","landed_at":"2026-10-10T02:00:00Z","risk_paths":[]}` + "\n",
			since:  "2026-10-10T01:00:00.5Z",
			cursor: "internal/a/one.go",
			until:  "2026-10-10T01:00:00.5Z",
			// A file with one readable record and no risky landing is quiet,
			// not unreadable.
			records: 1,
			quiet:   true,
		},
		{
			name:    "a fraction after the cursor is newer; an offset landing sorts by its instant",
			notes:   "cursor: 2026-10-10T00:00:00Z internal/a/one.go\n",
			ledger:  instants,
			beads:   "gt-c gt-a gt-b",
			since:   "2026-10-10T00:00:00Z",
			cursor:  "internal/a/one.go",
			until:   "2026-10-10T05:00:00.9Z",
			records: 3,
		},
		{
			name:    "a landing at the cursor instant is not reviewed again",
			notes:   "cursor: 2026-10-10T05:00:00.9Z internal/a/one.go\n",
			ledger:  instants,
			since:   "2026-10-10T05:00:00.9Z",
			cursor:  "internal/a/one.go",
			until:   "2026-10-10T05:00:00.9Z",
			records: 3,
			quiet:   true,
		},
		{
			name:        "a cursor after now is an error",
			notes:       "cursor: 2026-10-10T11:00:00Z internal/a/one.go\n",
			ledger:      testLedger,
			wantErrPart: "after now",
		},
		{
			name:        "an unreadable ledger is an error, not a quiet plan",
			ledger:      "",
			wantErrPart: "no readable landing record",
		},
		{
			name:        "a drifted ledger is an error",
			ledger:      `{"bead":"gt-9","landed_at":"yesterday","risk_paths":["x"]}` + "\n",
			wantErrPart: "record shape may have changed",
		},
		{
			name:        "a malformed cursor is an error",
			notes:       "cursor: not-a-time\n",
			ledger:      testLedger,
			wantErrPart: "bad timestamp in cursor line",
		},
	} {
		got, err := BuildPlan(tc.notes, []byte(tc.ledger), now)
		if tc.wantErrPart != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("%s: BuildPlan error = %v, want one naming %q", tc.name, err, tc.wantErrPart)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: BuildPlan error: %v", tc.name, err)
			continue
		}
		if beads := beadsOf(got); beads != tc.beads {
			t.Errorf("%s: landings = %q, want %q", tc.name, beads, tc.beads)
		}
		if got.Since.Text != tc.since {
			t.Errorf("%s: since = %q, want %q", tc.name, got.Since.Text, tc.since)
		}
		if got.Cursor != tc.cursor {
			t.Errorf("%s: cursor = %q, want %q", tc.name, got.Cursor, tc.cursor)
		}
		if got.ReviewedUntil.Text != tc.until {
			t.Errorf("%s: reviewed until = %q, want %q", tc.name, got.ReviewedUntil.Text, tc.until)
		}
		if got.Records != tc.records || got.Unparsed != tc.unparsed {
			t.Errorf("%s: records/unparsed = %d/%d, want %d/%d",
				tc.name, got.Records, got.Unparsed, tc.records, tc.unparsed)
		}
		if got.Quiet != tc.quiet {
			t.Errorf("%s: quiet = %v, want %v", tc.name, got.Quiet, tc.quiet)
		}
	}
}

// TestBuildPlanKeepsTheLedgerOrderOnATie pins the stable sort: a newest landing
// ahead of a tie group is the input that tells a stable sort from an unstable
// one, and the shell's sort_by is stable, so the group keeps the ledger's order.
func TestBuildPlanKeepsTheLedgerOrderOnATie(t *testing.T) {
	t.Parallel()
	var ledger strings.Builder
	ledger.WriteString(`{"bead":"gt-newest","landed_at":"2026-10-10T05:00:00Z","risk_paths":["x"]}` + "\n")
	tie := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&ledger, `{"bead":"gt-tie-%02d","landed_at":"2026-10-10T04:00:00Z","risk_paths":["x"]}`+"\n", i)
		tie = append(tie, fmt.Sprintf("gt-tie-%02d", i))
	}
	want := strings.Join(append(tie, "gt-newest"), " ")

	got, err := BuildPlan("", []byte(ledger.String()), utc(2026, 10, 10, 10, 0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if beads := beadsOf(got); beads != want {
		t.Errorf("landings = %q, want %q", beads, want)
	}
}
