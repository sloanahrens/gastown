package cmd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/fixrecurrence"
)

// fixHeldWindow is the report's window in every test: a month wide, ending at
// the fixed clock the scan tests use.
func fixHeldWindow() (time.Time, time.Time) {
	return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), fixRecurrenceTestNow()
}

// TestFixHeldCountsOnlyClosedBugs pins the rate's denominator: a closed bug
// counts, while a closed bead of another type and an open bug do not.
func TestFixHeldCountsOnlyClosedBugs(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: `[
  {"id":"gt-bugclosed","title":"closed bug","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"},
  {"id":"gt-taskclosed","title":"closed task","issue_type":"task","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-03T00:00:00Z"},
  {"id":"gt-bugopen","title":"open bug","issue_type":"bug","status":"open","created_at":"2026-09-01T00:00:00Z"}
]`}
	since, until := fixHeldWindow()
	result, err := fixHeldReport(fixRecurrenceTestDeps(bd), since, until)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Held != 1 || result.Rate != 1 {
		t.Errorf("result %+v, want the one closed bug counted and held", result)
	}
	// The report is one read of the rig's beads, in the rig directory, with no
	// write.
	if len(bd.calls) != 1 {
		t.Fatalf("ran bd %d times, want the one list read: %v", len(bd.calls), bd.calls)
	}
	if got, want := strings.Join(bd.calls[0], " "), strings.Join(fixRecurrenceListArgs, " "); got != want {
		t.Errorf("bd args %q, want %q", got, want)
	}
	if bd.dirs[0] != "/town/gastown" {
		t.Errorf("bd ran in %q, want the rig directory", bd.dirs[0])
	}
}

// TestFixHeldReadsClaimLabels pins that the label Plan writes on a citing bead
// is an edge against the bead it names, for both relationships.
func TestFixHeldReadsClaimLabels(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: `[
  {"id":"gt-recurbug","title":"fix one","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"},
  {"id":"gt-recurcite","title":"the symptom is back","issue_type":"task","status":"open","created_at":"2026-11-01T00:00:00Z","labels":["recurrence-of:gt-recurbug"]},
  {"id":"gt-classbug","title":"fix two","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-03T00:00:00Z"},
  {"id":"gt-classcite","title":"another instance","issue_type":"task","status":"open","created_at":"2026-11-02T00:00:00Z","labels":["same-class-of:gt-classbug"]}
]`}
	since, until := fixHeldWindow()
	result, err := fixHeldReport(fixRecurrenceTestDeps(bd), since, until)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || result.Held != 0 {
		t.Errorf("result %+v, want both labelled bugs struck", result)
	}
	if got := result.PerRelationship[fixrecurrence.Recurrence]; got != 1 {
		t.Errorf("recurrence claims %d, want 1", got)
	}
	if got := result.PerRelationship[fixrecurrence.SameClass]; got != 1 {
		t.Errorf("same-class claims %d, want 1", got)
	}
}

// TestFixHeldCountsDependencyEdges pins the report's other edge source: a
// typed dependency edge says the claim, and a dependency type that says
// nothing about a fix supplies none.
func TestFixHeldCountsDependencyEdges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		depType string
		held    int
	}{
		{name: "related", depType: "related", held: 0},
		{name: "blocks", depType: "blocks", held: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: `[
  {"id":"gt-oldbug","title":"fix","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"},
  {"id":"gt-citer","title":"broke again","issue_type":"task","status":"open","created_at":"2026-11-01T00:00:00Z","dependencies":[{"depends_on_id":"gt-oldbug","type":"` + tt.depType + `"}]}
]`}
			since, until := fixHeldWindow()
			result, err := fixHeldReport(fixRecurrenceTestDeps(bd), since, until)
			if err != nil {
				t.Fatal(err)
			}
			if result.Total != 1 || result.Held != tt.held {
				t.Errorf("result %+v, want one bug with %d held", result, tt.held)
			}
		})
	}
}

// TestFixHeldLabelResolvesTheRelatedType pins that a same-class label and the
// store's one "related" type for the same pair are one same-class claim, not a
// same-class claim and a recurrence.
func TestFixHeldLabelResolvesTheRelatedType(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: `[
  {"id":"gt-sclass","title":"fix","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"},
  {"id":"gt-sclassciter","title":"another instance","issue_type":"task","status":"open","created_at":"2026-11-01T00:00:00Z","labels":["same-class-of:gt-sclass"],"dependencies":[{"depends_on_id":"gt-sclass","type":"related"}]}
]`}
	since, until := fixHeldWindow()
	result, err := fixHeldReport(fixRecurrenceTestDeps(bd), since, until)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Held != 0 {
		t.Errorf("result %+v, want the bug struck", result)
	}
	if got := result.PerRelationship[fixrecurrence.SameClass]; got != 1 {
		t.Errorf("same-class claims %d, want the label's one claim", got)
	}
	if got := result.PerRelationship[fixrecurrence.Recurrence]; got != 0 {
		t.Errorf("recurrence claims %d, want the label to resolve the related edge", got)
	}
}

// TestFixHeldIgnoresAnEdgeThatPrecedesTheClose pins the order Report cannot
// check: a claim from a bead created before the cited fix closed, or naming a
// bead the rows do not list, cannot say the fix failed.
func TestFixHeldIgnoresAnEdgeThatPrecedesTheClose(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: `[
  {"id":"gt-latebug","title":"fix","issue_type":"bug","status":"closed","created_at":"2026-07-01T00:00:00Z","closed_at":"2026-08-01T00:00:00Z"},
  {"id":"gt-earlyciter","title":"predates the fix","issue_type":"task","status":"open","created_at":"2026-07-15T00:00:00Z","labels":["recurrence-of:gt-latebug"]},
  {"id":"gt-unknownciter","title":"names a bead the rows do not list","issue_type":"task","status":"open","created_at":"2026-09-01T00:00:00Z","labels":["recurrence-of:gt-absent"]}
]`}
	result, err := fixHeldReport(fixRecurrenceTestDeps(bd), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), fixRecurrenceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Held != 1 {
		t.Errorf("result %+v, want the one bug held because neither edge can claim it", result)
	}
	if len(result.PerRelationship) != 0 {
		t.Errorf("per-relationship %v, want no edge counted", result.PerRelationship)
	}
}

// TestFixHeldReadsTheLabelsThePlanWrites pins the two label prefixes this file
// spells out to the ones fixrecurrence.Plan writes, so a rename in the package
// cannot leave the report reading a label nothing writes.
func TestFixHeldReadsTheLabelsThePlanWrites(t *testing.T) {
	t.Parallel()
	for _, rel := range []fixrecurrence.Relationship{fixrecurrence.Recurrence, fixrecurrence.SameClass} {
		t.Run(string(rel), func(t *testing.T) {
			t.Parallel()
			actions := fixrecurrence.Plan(
				[]fixrecurrence.Proposal{{New: "gt-new", Old: "gt-old", Rel: rel}},
				fixrecurrence.State{},
			)
			// The plan writes a label on the citing bead and a marker on the
			// cited one; the claim label is the one this file must read.
			var found bool
			for _, action := range actions {
				for _, arg := range action.Args {
					label, ok := strings.CutPrefix(arg, "--add-label=")
					if !ok {
						continue
					}
					if gotRel, gotOld, ok := fixHeldLabel(label); ok && gotRel == rel && gotOld == "gt-old" {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("no planned label reads as %q against gt-old: %v", rel, actions)
			}
		})
	}
}

// TestFixHeldEmptyWindowHasZeroRate pins that a window with no closed bug
// reports a zero rate rather than a division by zero.
func TestFixHeldEmptyWindowHasZeroRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		out   string
		since time.Time
		until time.Time
	}{
		{
			name:  "no beads",
			out:   `[]`,
			since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			until: fixRecurrenceTestNow(),
		},
		{
			name:  "closed bug outside the window",
			out:   `[{"id":"gt-oldbug","title":"fix","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"}]`,
			since: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
			until: fixRecurrenceTestNow(),
		},
		{
			name:  "since at until",
			out:   `[{"id":"gt-oldbug","title":"fix","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"}]`,
			since: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
			until: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: tt.out}
			result, err := fixHeldReport(fixRecurrenceTestDeps(bd), tt.since, tt.until)
			if err != nil {
				t.Fatal(err)
			}
			if result.Total != 0 || result.Held != 0 || result.Rate != 0 {
				t.Errorf("result %+v, want an empty window at rate 0", result)
			}
		})
	}
}

// TestFixHeldReturnsReadError pins that a failed bd read fails the report
// instead of reading as an empty, clean window.
func TestFixHeldReturnsReadError(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{err: errors.New("bd refused")}
	since, until := fixHeldWindow()
	result, err := fixHeldReport(fixRecurrenceTestDeps(bd), since, until)
	if err == nil {
		t.Fatal("want the read failure returned")
	}
	// The returned error is the read's, not one a later parse of the empty
	// output would raise: a failed read must never be reported as bad JSON.
	if !strings.Contains(err.Error(), "bd refused") {
		t.Errorf("error %v, want it to carry the read failure", err)
	}
	if result.Total != 0 || result.Rate != 0 {
		t.Errorf("result %+v, want no report from a failed read", result)
	}
}

// TestFixHeldReturnsWorkDirError pins that a report with no rig directory to
// read fails before it reads anything.
func TestFixHeldReturnsWorkDirError(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: `[]`}
	d := fixRecurrenceTestDeps(bd)
	d.workDir = func() (string, error) { return "", errors.New("caller cwd gone") }
	since, until := fixHeldWindow()
	_, err := fixHeldReport(d, since, until)
	if err == nil || !strings.Contains(err.Error(), "caller cwd gone") {
		t.Errorf("error %v, want the directory failure returned", err)
	}
	if len(bd.calls) != 0 {
		t.Errorf("ran bd %d times without a directory, want none", len(bd.calls))
	}
}

// TestFixHeldRefusesAnUnparseableList pins that output bd prints which is not
// JSON fails the report rather than reading as an empty, clean window.
func TestFixHeldRefusesAnUnparseableList(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: `not json`}
	since, until := fixHeldWindow()
	_, err := fixHeldReport(fixRecurrenceTestDeps(bd), since, until)
	if err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("error %v, want the unparseable list refused", err)
	}
}

// fixHeldCmdFixture is the command tests' rig: two bugs the window's claims
// leave held, one struck by a single recurrence, and one two citing beads fail
// -- a recurrence and a same-class claim -- which makes it chronic. Every citing
// bead was created after the bug it names closed.
const fixHeldCmdFixture = `[
  {"id":"gt-heldone","title":"fix one","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-20T00:00:00Z"},
  {"id":"gt-heldtwo","title":"fix two","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-21T00:00:00Z"},
  {"id":"gt-struck","title":"fix three","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-22T00:00:00Z"},
  {"id":"gt-chronic","title":"fix four","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-23T00:00:00Z"},
  {"id":"gt-citerone","title":"broke again","issue_type":"task","status":"open","created_at":"2026-11-01T00:00:00Z","labels":["recurrence-of:gt-struck"]},
  {"id":"gt-citertwo","title":"broke again","issue_type":"task","status":"open","created_at":"2026-11-01T00:00:00Z","labels":["recurrence-of:gt-chronic"]},
  {"id":"gt-citerthree","title":"another instance","issue_type":"task","status":"open","created_at":"2026-11-02T00:00:00Z","labels":["same-class-of:gt-chronic"]}
]`

// fixHeldCmdRun is one run of `gt fix-held`: what it wrote to each stream and
// the error Execute returned.
type fixHeldCmdRun struct {
	stdout string
	stderr string
	err    error
}

// runFixHeldCmdTest runs the production command with args, the way main runs
// it, so the flags, the read and the printing are all driven. out is the
// command's standard output.
func runFixHeldCmdTest(t *testing.T, d fixRecurrenceDeps, out io.Writer, args ...string) fixHeldCmdRun {
	t.Helper()
	cmd := newFixHeldCmd(d)
	var stdout, stderr bytes.Buffer
	if out != nil {
		cmd.SetOut(out)
	} else {
		cmd.SetOut(&stdout)
	}
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return fixHeldCmdRun{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// TestFixHeldCmdPrintsTheReport pins the person-readable report: the rate, one
// line per relationship including the ones nothing claimed, and the chronic
// list, for the default 30d window.
func TestFixHeldCmdPrintsTheReport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "claims of every kind",
			out:  fixHeldCmdFixture,
			want: `held 2 of 4 (rate 0.50)
recurrence 2
same class 1
regression 0
follow-up 0
reference 0
chronic: gt-chronic
`,
		},
		{
			name: "an empty window",
			out:  `[]`,
			want: `held 0 of 0 (rate 0.00)
recurrence 0
same class 0
regression 0
follow-up 0
reference 0
chronic: none
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: tt.out}
			run := runFixHeldCmdTest(t, fixRecurrenceTestDeps(bd), nil)
			if run.err != nil {
				t.Fatalf("command failed: %v", run.err)
			}
			if run.stdout != tt.want {
				t.Errorf("output:\n%s\nwant:\n%s", run.stdout, tt.want)
			}
			// The report is one read of the rig's beads, in the rig directory.
			if len(bd.calls) != 1 {
				t.Fatalf("ran bd %d times, want the one list read: %v", len(bd.calls), bd.calls)
			}
			if bd.dirs[0] != "/town/gastown" {
				t.Errorf("bd ran in %q, want the rig directory", bd.dirs[0])
			}
		})
	}
}

// TestFixHeldCmdPrintsJSON pins --json to one object carrying the fields the
// text prints, with every relationship a key so the two forms total the same.
func TestFixHeldCmdPrintsJSON(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: fixHeldCmdFixture}
	run := runFixHeldCmdTest(t, fixRecurrenceTestDeps(bd), nil, "--json")
	if run.err != nil {
		t.Fatalf("command failed: %v", run.err)
	}
	want := `{
  "held": 2,
  "total": 4,
  "rate": 0.5,
  "per_relationship": {
    "follow-up": 0,
    "recurrence": 2,
    "reference": 0,
    "regression": 0,
    "same class": 1
  },
  "chronic": [
    "gt-chronic"
  ]
}
`
	if run.stdout != want {
		t.Errorf("output:\n%s\nwant:\n%s", run.stdout, want)
	}
	if len(bd.calls) != 1 {
		t.Errorf("ran bd %d times, want the one list read: %v", len(bd.calls), bd.calls)
	}
}

// TestFixHeldCmdKeepsTheSinceWindow pins --since: a bug that closed before the
// window is not counted, and one inside it is.
func TestFixHeldCmdKeepsTheSinceWindow(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: `[
  {"id":"gt-recent","title":"fix","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-11-10T00:00:00Z"},
  {"id":"gt-stale","title":"fix","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z","closed_at":"2026-10-20T00:00:00Z"}
]`}
	run := runFixHeldCmdTest(t, fixRecurrenceTestDeps(bd), nil, "--since", "7d")
	if run.err != nil {
		t.Fatalf("command failed: %v", run.err)
	}
	if want := "held 1 of 1 (rate 1.00)\n"; !strings.HasPrefix(run.stdout, want) {
		t.Errorf("output:\n%s\nwant it to start %q", run.stdout, want)
	}
}

// TestFixHeldCmdRefusesABadSince pins that a --since no window can be built
// from fails before the rig is read, with the reason on stderr.
func TestFixHeldCmdRefusesABadSince(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		since string
		want  string
	}{
		{name: "zero days", since: "0d", want: `--since "0d": days must be a whole number of at least 1`},
		{name: "not a duration", since: "yesterday", want: `--since "yesterday": want Nd or a positive Go duration`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: `[]`}
			run := runFixHeldCmdTest(t, fixRecurrenceTestDeps(bd), nil, "--since", tt.since)
			if code, ok := IsSilentExit(run.err); !ok || code != 2 {
				t.Fatalf("error %v, want a silent exit 2", run.err)
			}
			if !strings.Contains(run.stderr, "gt fix-held: "+tt.want) {
				t.Errorf("stderr %q, want it to carry %q", run.stderr, tt.want)
			}
			if run.stdout != "" {
				t.Errorf("output %q, want nothing printed", run.stdout)
			}
			if len(bd.calls) != 0 {
				t.Errorf("ran bd %d times without a window, want none", len(bd.calls))
			}
		})
	}
}

// TestFixHeldCmdReportsAReadFailure pins that a rig the command cannot read
// fails it, with the reason on stderr and no report on stdout.
func TestFixHeldCmdReportsAReadFailure(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{err: errors.New("bd refused")}
	run := runFixHeldCmdTest(t, fixRecurrenceTestDeps(bd), nil)
	if code, ok := IsSilentExit(run.err); !ok || code != 2 {
		t.Fatalf("error %v, want a silent exit 2", run.err)
	}
	if want := "gt fix-held: reading the rig's beads: bd refused"; !strings.Contains(run.stderr, want) {
		t.Errorf("stderr %q, want it to carry %q", run.stderr, want)
	}
	if run.stdout != "" {
		t.Errorf("output %q, want no partial report", run.stdout)
	}
}

// fixHeldWriteErrorWriter fails its failAt-th write and takes every other one,
// so the report's own check on that write is the only thing that can stop the
// command: a check that is dropped leaves the run exiting 0.
type fixHeldWriteErrorWriter struct {
	failAt int
	writes int
}

func (w *fixHeldWriteErrorWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, errors.New("stdout closed")
	}
	return len(p), nil
}

// TestFixHeldCmdReportsAWriteFailure pins that a report stream that stops
// taking the report fails the command: the header, a relationship row, the
// chronic line and the JSON object each report the write, so a truncated
// report never exits 0.
func TestFixHeldCmdReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		args   []string
		failAt int
	}{
		{name: "text header", failAt: 1},
		{name: "relationship row", failAt: 3},
		{name: "chronic line", failAt: 7},
		{name: "json object", args: []string{"--json"}, failAt: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: fixHeldCmdFixture}
			run := runFixHeldCmdTest(t, fixRecurrenceTestDeps(bd), &fixHeldWriteErrorWriter{failAt: tt.failAt}, tt.args...)
			if code, ok := IsSilentExit(run.err); !ok || code != 2 {
				t.Fatalf("error %v, want a silent exit 2", run.err)
			}
			if want := "gt fix-held: writing the report: stdout closed"; !strings.Contains(run.stderr, want) {
				t.Errorf("stderr %q, want it to carry %q", run.stderr, want)
			}
		})
	}
}

// TestFixHeldRefusesARowItCannotPlace pins that a bead the rows cannot date is
// an error, never counted as held or silently left out.
func TestFixHeldRefusesARowItCannotPlace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "unreadable created_at",
			out:  `[{"id":"gt-one","issue_type":"bug","status":"closed","created_at":"yesterday","closed_at":"2026-10-02T00:00:00Z"}]`,
			want: "gt-one: created_at",
		},
		{
			name: "closed bug without closed_at",
			out:  `[{"id":"gt-one","issue_type":"bug","status":"closed","created_at":"2026-09-01T00:00:00Z"}]`,
			want: "gt-one: closed_at is missing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: tt.out}
			since, until := fixHeldWindow()
			_, err := fixHeldReport(fixRecurrenceTestDeps(bd), since, until)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
