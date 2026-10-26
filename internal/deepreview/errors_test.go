package deepreview

import (
	"reflect"
	"strings"
	"testing"
)

// errorLedgerLine is one readable landing the refusal rows place beside the
// broken input, so a row fails before its error is raised rather than after.
const errorLedgerLine = `{"bead":"gt-1","head":"h1","landed_commit":"c1","landed_at":"2026-10-10T01:00:00Z","risk_paths":["internal/daemon/x.go"]}` + "\n"

// TestEveryErrorRefuses has a row per error the plan layer owns in the
// design's Errors section: a ledger missing or unreadable, a malformed line,
// and an unreadable cursor.
//
// Every row is the bytes and notes the command layer hands BuildPlan, and every
// row must come back as an error and a zero Plan. A plan returned beside an
// error would read as an empty or quiet day, and the design's rule for these
// errors is a refusal, never an empty slice.
//
// The design's other three errors -- a slice over the bound, a crash between
// the steps, and one run at a time -- belong to the slice and the daemon
// (tasks 3, 5 and 6: gt-7axri, gt-pyxj0, gt-37ne7). They cannot be written
// without those files, which this bead puts out of scope; their rows land with
// them.
func TestEveryErrorRefuses(t *testing.T) {
	t.Parallel()
	now := utc(2026, 10, 10, 10, 0, 0, 0)
	for _, tc := range []struct {
		// design is the design's error this row proves, in the bead's words.
		design string
		name   string
		notes  string
		// ledger is nil for a ledger the caller could not read at all, the
		// same bytes an empty file yields.
		ledger []byte
		want   string
	}{
		{
			design: "ledger missing or unreadable",
			name:   "a ledger the caller could not read",
			ledger: nil,
			want:   "no readable landing record",
		},
		{
			design: "ledger missing or unreadable",
			name:   "an empty ledger",
			ledger: []byte{},
			want:   "no readable landing record",
		},
		{
			design: "ledger missing or unreadable",
			name:   "a ledger of blank lines",
			ledger: []byte("\n \n\t\n"),
			want:   "no readable landing record",
		},
		{
			design: "ledger missing or unreadable",
			name:   "a ledger holding only a torn last line",
			ledger: []byte("{torn\n"),
			want:   "no readable landing record",
		},
		{
			design: "malformed line",
			name:   "a line that is not JSON before the last",
			ledger: []byte("garbage\n" + errorLedgerLine),
			want:   "before the last are not JSON",
		},
		{
			design: "malformed line",
			name:   "a JSON null before the last",
			ledger: []byte("null\n" + errorLedgerLine),
			want:   "before the last are not JSON",
		},
		{
			design: "malformed line",
			name:   "a torn line between two records",
			ledger: []byte(errorLedgerLine + "garbage\n" + errorLedgerLine),
			want:   "before the last are not JSON",
		},
		{
			design: "malformed line",
			name:   "a record whose landed_at is not a time",
			ledger: []byte(errorLedgerLine + `{"bead":"gt-2","landed_at":"yesterday"}` + "\n"),
			want:   "record shape may have changed",
		},
		{
			design: "malformed line",
			name:   "a record whose landed_at is missing, and no other record",
			ledger: []byte(`{"bead":"gt-2","risk_paths":["x"]}` + "\n"),
			want:   "record shape may have changed",
		},
		{
			design: "malformed line",
			name:   "a record whose risk_paths is not a list",
			ledger: []byte(`{"landed_at":"2026-10-10T01:00:00Z","risk_paths":"x"}` + "\n"),
			want:   "record shape may have changed",
		},
		{
			design: "malformed line",
			name:   "a record whose bead is not a string",
			ledger: []byte(`{"landed_at":"2026-10-10T01:00:00Z","bead":7}` + "\n"),
			want:   "record shape may have changed",
		},
		{
			design: "malformed line",
			name:   "a line that is JSON but not an object",
			ledger: []byte("[1,2]\n"),
			want:   "record shape may have changed",
		},
		{
			design: "malformed line",
			// Go's RFC3339 parser takes a comma for a fraction's point; the
			// shell's ts rule does not, and the ledger never writes one.
			name:   "a record whose fraction has a comma for its point",
			ledger: []byte(`{"bead":"gt-2","landed_at":"2026-10-10T01:00:00,5Z","risk_paths":["x"]}` + "\n"),
			want:   "record shape may have changed",
		},
		{
			design: "unreadable cursor",
			name:   "a cursor with no timestamp",
			notes:  "cursor: \n",
			ledger: []byte(errorLedgerLine),
			want:   "malformed cursor line",
		},
		{
			design: "unreadable cursor",
			name:   "a cursor whose timestamp is not a time",
			notes:  "cursor: not-a-time\n",
			ledger: []byte(errorLedgerLine),
			want:   "bad timestamp in cursor line",
		},
		{
			design: "unreadable cursor",
			name:   "a cursor whose fraction has a comma for its point",
			notes:  "cursor: 2026-10-10T01:00:00,5Z\n",
			ledger: []byte(errorLedgerLine),
			want:   "bad timestamp in cursor line",
		},
		{
			design: "unreadable cursor",
			name:   "a tab where the path's space goes",
			notes:  "cursor: 2026-10-10T01:00:00Z\tinternal/a/one.go\n",
			ledger: []byte(errorLedgerLine),
			want:   "malformed cursor line",
		},
		{
			design: "unreadable cursor",
			name:   "a malformed cursor beside a newer readable one",
			notes:  "cursor: 2026-10-10T01:00:00Z\ncursor: not-a-time\n",
			ledger: []byte(errorLedgerLine),
			want:   "bad timestamp in cursor line",
		},
		{
			design: "unreadable cursor",
			name:   "a cursor after now",
			notes:  "cursor: 2026-10-10T11:00:00Z\n",
			ledger: []byte(errorLedgerLine),
			want:   "after now",
		},
	} {
		plan, err := BuildPlan(tc.notes, tc.ledger, now)
		if err == nil {
			t.Errorf("%s (%s): BuildPlan returned no error, plan %+v", tc.name, tc.design, plan)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s (%s): error = %q, want one naming %q", tc.name, tc.design, err, tc.want)
		}
		if !reflect.DeepEqual(plan, Plan{}) {
			t.Errorf("%s (%s): plan = %+v beside the error, want the zero Plan so no caller sees a slice",
				tc.name, tc.design, plan)
		}
	}
}

// TestAPlanErrorIsNotAQuietDay pins the property the refusal rows rest on: an
// invalid input must not come back as a plan that is quiet. Quiet is the one
// verdict a caller acts on without reading the landings, so a refusal that
// looked quiet would stop the review while reviewing nothing.
func TestAPlanErrorIsNotAQuietDay(t *testing.T) {
	t.Parallel()
	now := utc(2026, 10, 10, 10, 0, 0, 0)
	for _, ledger := range [][]byte{
		nil,
		[]byte("garbage\n" + errorLedgerLine),
		[]byte(`{"bead":"gt-2","landed_at":"yesterday"}` + "\n"),
	} {
		plan, err := BuildPlan("", ledger, now)
		if err == nil {
			t.Errorf("BuildPlan(%q) error = nil, want a refusal", ledger)
			continue
		}
		if plan.Quiet || plan.Landings != nil {
			t.Errorf("BuildPlan(%q) = %+v beside the error, want no quiet plan and no landings", ledger, plan)
		}
	}
}
