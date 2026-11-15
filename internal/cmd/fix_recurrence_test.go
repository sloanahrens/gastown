package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/fixrecurrence"
)

// fixRecurrenceFakeBD answers the scan's calls from inline JSON and records
// every one, so a test sees each program's argv and directory and no process
// starts.
type fixRecurrenceFakeBD struct {
	out string
	err error
	// failAt, when set, is the 1-based call that fails with err, so a test
	// drives a write that fails mid-plan; unset, err fails every call, which is
	// the read-failure case.
	failAt int
	dirs   []string
	calls  [][]string
}

func (f *fixRecurrenceFakeBD) run(dir string, args []string) ([]byte, error) {
	f.dirs = append(f.dirs, dir)
	f.calls = append(f.calls, args)
	if f.failAt > 0 {
		if len(f.calls) == f.failAt {
			return nil, f.err
		}
		return []byte(f.out), nil
	}
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.out), nil
}

// fixRecurrenceTestNow is the scan's clock: recent enough that a --since 7d
// window has the rows tests date in it.
func fixRecurrenceTestNow() time.Time {
	return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
}

// fixRecurrenceTestDeps is production's wiring with the fake runner and the
// fixed clock; the tests never read the working directory or the wall clock.
func fixRecurrenceTestDeps(bd *fixRecurrenceFakeBD) fixRecurrenceDeps {
	return fixRecurrenceDeps{
		workDir: func() (string, error) { return "/town/gastown", nil },
		run:     bd.run,
		now:     fixRecurrenceTestNow,
	}
}

// fixRecurrenceThreeProposals lists one closed bead and three beads that cite
// it: a recurrence, a regression, and a title no classifier can read.
const fixRecurrenceThreeProposals = `[
  {"id":"gt-newre","title":"recurrence of gt-olda","status":"open","created_at":"2026-11-01T00:00:00Z"},
  {"id":"gt-newreg","title":"the regression gt-oldb","status":"open","created_at":"2026-11-02T00:00:00Z"},
  {"id":"gt-newamb","title":"see gt-olda for the fix","status":"open","created_at":"2026-11-03T00:00:00Z"},
  {"id":"gt-olda","title":"fix one","status":"closed","created_at":"2026-10-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"},
  {"id":"gt-oldb","title":"fix two","status":"closed","created_at":"2026-10-01T00:00:00Z","closed_at":"2026-10-03T00:00:00Z"}
]`

func TestFixRecurrenceScanPrintsOneJSONLinePerProposal(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: fixRecurrenceThreeProposals}
	var out bytes.Buffer
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", true, false); err != nil {
		t.Fatal(err)
	}
	want := `{"new":"gt-newamb","old":"gt-olda","relationship":"reference","confidence":0,"source":"ambiguous","ambiguous":true}
{"new":"gt-newre","old":"gt-olda","relationship":"recurrence","confidence":0.9,"source":"title"}
{"new":"gt-newreg","old":"gt-oldb","relationship":"regression","confidence":0.9,"source":"title"}
`
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
	// Without --apply the scan is read-only: one bd list, in the rig directory,
	// and nothing else.
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

func TestFixRecurrenceScanPrintsReadableLines(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: fixRecurrenceThreeProposals}
	var out bytes.Buffer
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", false, false); err != nil {
		t.Fatal(err)
	}
	want := `gt-newamb -> gt-olda  ambiguous
gt-newre -> gt-olda  recurrence 0.90 (title)
gt-newreg -> gt-oldb  regression 0.90 (title)
`
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// fixRecurrenceListJSON renders the bd list payload from rows as the read
// parses them, so a fixture states the beads and never spells bd's list
// members by hand. The shape a real read returns is pinned separately by
// testdata/fix_recurrence_bd_list.json.
func fixRecurrenceListJSON(t *testing.T, rows ...fixRecurrenceRow) string {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshalling a bead fixture: %v", err)
	}
	return string(raw)
}

// TestFixRecurrenceScanSuppressesAPairWithAnEdgeEitherWay pins that a relation
// bd prints on one row suppresses the pair whichever end it appears on: the
// reverse relation exists only in the other bead's dependencies array, so the
// row bd printed it on is the wrong end to search.
func TestFixRecurrenceScanSuppressesAPairWithAnEdgeEitherWay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rows []fixRecurrenceRow
	}{
		{
			name: "citing bead carries the edge",
			rows: []fixRecurrenceRow{
				{ID: "gt-newe1", Title: "recurrence of gt-olde1", Status: "open", CreatedAt: "2026-11-01T00:00:00Z",
					Dependencies: []fixRecurrenceDepRow{{DependsOnID: "gt-olde1"}}},
				{ID: "gt-olde1", Title: "fix", Status: "closed", CreatedAt: "2026-10-01T00:00:00Z", ClosedAt: "2026-10-02T00:00:00Z"},
			},
		},
		{
			name: "cited bead carries the edge",
			rows: []fixRecurrenceRow{
				{ID: "gt-newe1", Title: "recurrence of gt-olde1", Status: "open", CreatedAt: "2026-11-01T00:00:00Z"},
				{ID: "gt-olde1", Title: "fix", Status: "closed", CreatedAt: "2026-10-01T00:00:00Z", ClosedAt: "2026-10-02T00:00:00Z",
					Dependencies: []fixRecurrenceDepRow{{DependsOnID: "gt-newe1"}}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: fixRecurrenceListJSON(t, tt.rows...)}
			var out bytes.Buffer
			if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", true, false); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Errorf("printed a proposal for a pair an edge already joins:\n%s", out.String())
			}
		})
	}
}

func TestFixRecurrenceScanFailedReadIsAnError(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{err: errors.New("bd list refused")}
	var out bytes.Buffer
	err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", true, false)
	if err == nil {
		t.Fatal("a failed bead read returned nil")
	}
	if !strings.Contains(err.Error(), "reading the rig's beads") {
		t.Errorf("error %q does not name the failed read", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed output on a failed read:\n%s", out.String())
	}
}

func TestFixRecurrenceScanEmptyPairSetPrintsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
	}{
		{name: "empty bead list", out: `[]`},
		{
			name: "no citation",
			out:  `[{"id":"gt-one","title":"a fix","status":"closed","created_at":"2026-10-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"}]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, asJSON := range []bool{true, false} {
				bd := &fixRecurrenceFakeBD{out: tt.out}
				var out bytes.Buffer
				if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", asJSON, false); err != nil {
					t.Fatalf("empty pair set (json=%t) returned %v, want nil", asJSON, err)
				}
				if out.Len() != 0 {
					t.Errorf("empty pair set (json=%t) printed %q", asJSON, out.String())
				}
			}
		})
	}
}

func TestFixRecurrenceScanRefusesUnreadableBeadList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		want string
	}{
		{name: "malformed list", out: `{"id":"gt-one"`, want: "does not parse"},
		{
			name: "unparseable created_at",
			out:  `[{"id":"gt-one","created_at":"yesterday"}]`,
			want: "gt-one: created_at",
		},
		{
			name: "unparseable closed_at",
			out:  `[{"id":"gt-one","created_at":"2026-10-01T00:00:00Z","closed_at":"last week"}]`,
			want: "gt-one: closed_at",
		},
		{
			// A bead with no creation time cannot be dated, so reading it as a
			// bead that cites nothing would drop it from the signal unseen.
			name: "missing created_at",
			out:  `[{"id":"gt-one","title":"recurrence of gt-two","status":"open"}]`,
			want: "gt-one: created_at is missing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: tt.out}
			var out bytes.Buffer
			err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", true, false)
			if err == nil {
				t.Fatal("an unreadable bead list returned nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q lacks %q", err, tt.want)
			}
			if out.Len() != 0 {
				t.Errorf("printed output for an unreadable list:\n%s", out.String())
			}
		})
	}
}

func TestFixRecurrenceScanSinceKeepsRecentCitingBeads(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: `[
  {"id":"gt-oldone","title":"fix","status":"closed","created_at":"2025-09-01T00:00:00Z","closed_at":"2025-09-02T00:00:00Z"},
  {"id":"gt-stale","title":"recurrence of gt-oldone","status":"open","created_at":"2026-10-01T00:00:00Z"},
  {"id":"gt-ontheedge","title":"recurrence of gt-oldone","status":"open","created_at":"2026-11-08T12:00:00Z"},
  {"id":"gt-recent","title":"recurrence of gt-oldone","status":"open","created_at":"2026-11-10T00:00:00Z"}
]`}
	var out bytes.Buffer
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "7d", true, false); err != nil {
		t.Fatal(err)
	}
	want := `{"new":"gt-ontheedge","old":"gt-oldone","relationship":"recurrence","confidence":0.9,"source":"title"}
{"new":"gt-recent","old":"gt-oldone","relationship":"recurrence","confidence":0.9,"source":"title"}
`
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestFixRecurrenceScanRefusesAnUnreadableSince(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: fixRecurrenceThreeProposals}
	var out bytes.Buffer
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "banana", true, false); err == nil {
		t.Fatal("--since banana returned nil")
	}
	// The window is read before the beads, so a bad value makes no bd call.
	if len(bd.calls) != 0 {
		t.Errorf("ran bd %v for an unreadable --since", bd.calls)
	}
}

func TestFixRecurrenceScanWorkDirErrorIsReturned(t *testing.T) {
	t.Parallel()
	deps := fixRecurrenceTestDeps(&fixRecurrenceFakeBD{})
	deps.workDir = func() (string, error) { return "", errors.New("no working directory") }
	var out bytes.Buffer
	err := runFixRecurrenceScan(&out, deps, "", true, false)
	if err == nil || !strings.Contains(err.Error(), "finding the rig directory") {
		t.Fatalf("workDir error = %v, want it named", err)
	}
}

// fixRecurrenceRefusingWriter fails every write, so a broken stdout is an
// error rather than a silently truncated answer.
type fixRecurrenceRefusingWriter struct{}

func (fixRecurrenceRefusingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write refused")
}

func TestFixRecurrenceScanWriteErrorIsReturned(t *testing.T) {
	t.Parallel()
	for _, asJSON := range []bool{true, false} {
		bd := &fixRecurrenceFakeBD{out: fixRecurrenceThreeProposals}
		err := runFixRecurrenceScan(fixRecurrenceRefusingWriter{}, fixRecurrenceTestDeps(bd), "", asJSON, false)
		if err == nil {
			t.Fatalf("a refused write (json=%t) returned nil", asJSON)
		}
	}
}

// fixRecurrenceCallLines renders each recorded call as one space-joined line,
// the argv a reader compares against the plan.
func fixRecurrenceCallLines(f *fixRecurrenceFakeBD) []string {
	lines := make([]string, len(f.calls))
	for i, call := range f.calls {
		lines[i] = strings.Join(call, " ")
	}
	return lines
}

// fixRecurrenceCheckCalls fails when the runner did not receive want, in that
// order, and nothing else.
func fixRecurrenceCheckCalls(t *testing.T, f *fixRecurrenceFakeBD, want []string) {
	t.Helper()
	got := fixRecurrenceCallLines(f)
	if len(got) != len(want) {
		t.Fatalf("ran %d programs, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i+1, got[i], want[i])
		}
	}
}

// fixRecurrenceChronicProposals lists a closed fix two beads already claim and
// one bead that newly cites it: the new recurrence earns the edge and its
// labels, and the third claim makes the fix chronic.
func fixRecurrenceChronicProposals(t *testing.T) string {
	t.Helper()
	return fixRecurrenceListJSON(t,
		fixRecurrenceRow{ID: "gt-newa", Title: "recurrence of gt-olda", Status: "open", CreatedAt: "2026-11-01T00:00:00Z"},
		fixRecurrenceRow{ID: "gt-newb", Title: "first claim", Status: "open", CreatedAt: "2026-11-02T00:00:00Z",
			Dependencies: []fixRecurrenceDepRow{{DependsOnID: "gt-olda", Type: "related"}}},
		fixRecurrenceRow{ID: "gt-newc", Title: "second claim", Status: "open", CreatedAt: "2026-11-03T00:00:00Z",
			Dependencies: []fixRecurrenceDepRow{{DependsOnID: "gt-olda", Type: "related"}}},
		fixRecurrenceRow{ID: "gt-olda", Title: "fix one", Status: "closed", CreatedAt: "2026-10-01T00:00:00Z", ClosedAt: "2026-10-02T00:00:00Z"},
	)
}

// fixRecurrenceChronicCalls is the plan fixRecurrenceChronicProposals earns, as
// the runner receives it: the read, then each write once in plan order.
var fixRecurrenceChronicCalls = []string{
	"bd list --all --limit 0 --json --include-infra",
	"bd dep add --type=related -- gt-newa gt-olda",
	"bd update --add-label=recurrence-of:gt-olda -- gt-newa",
	"bd update --add-label=recurred -- gt-olda",
	"bd update --add-label=chronic -- gt-olda",
	"gt escalate chronic fix gt-olda: 3 beads report it did not hold --severity medium --fingerprint fix-recurrence:gt-olda",
}

func TestFixRecurrenceScanApplyRunsEachPlannedActionOnceInPlanOrder(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: fixRecurrenceChronicProposals(t)}
	var out bytes.Buffer
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", false, true); err != nil {
		t.Fatal(err)
	}
	fixRecurrenceCheckCalls(t, bd, fixRecurrenceChronicCalls)
	want := `did dep-add: bd dep add --type=related -- gt-newa gt-olda
did label-add: bd update --add-label=recurrence-of:gt-olda -- gt-newa
did label-add: bd update --add-label=recurred -- gt-olda
did label-add: bd update --add-label=chronic -- gt-olda
did escalate: gt escalate chronic fix gt-olda: 3 beads report it did not hold --severity medium --fingerprint fix-recurrence:gt-olda
`
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestFixRecurrenceScanApplyStopsAtTheFirstFailure pins that a write which
// fails ends the run: the writes after it must not follow a plan a reader is
// told stopped.
func TestFixRecurrenceScanApplyStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: fixRecurrenceThreeProposals, err: errors.New("bd refused"), failAt: 3}
	var out bytes.Buffer
	err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", false, true)
	if err == nil {
		t.Fatal("a failed write returned nil")
	}
	if !strings.Contains(err.Error(), "dep-add") || !strings.Contains(err.Error(), "bd refused") {
		t.Errorf("error %q does not name the action and the failure", err)
	}
	// The list read and the ambiguous label ran; the dep-add that failed is the
	// last call, and nothing after it was attempted.
	fixRecurrenceCheckCalls(t, bd, []string{
		"bd list --all --limit 0 --json --include-infra",
		"bd update --add-label=fix-rel:ambiguous -- gt-newamb",
		"bd dep add --type=related -- gt-newre gt-olda",
	})
	if want := "did label-add: bd update --add-label=fix-rel:ambiguous -- gt-newamb\n"; out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestFixRecurrenceScanApplySkipsWritesTheReadCarries pins that the plan is
// built from the same read: the labels the rows already hold earn no write.
func TestFixRecurrenceScanApplySkipsWritesTheReadCarries(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: `[
  {"id":"gt-newre","title":"recurrence of gt-olda","status":"open","created_at":"2026-11-01T00:00:00Z","labels":["recurrence-of:gt-olda"]},
  {"id":"gt-olda","title":"fix one","status":"closed","created_at":"2026-10-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z","labels":["recurred"]}
]`}
	var out bytes.Buffer
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", false, true); err != nil {
		t.Fatal(err)
	}
	fixRecurrenceCheckCalls(t, bd, []string{
		"bd list --all --limit 0 --json --include-infra",
		"bd dep add --type=related -- gt-newre gt-olda",
	})
}

// TestFixRecurrenceScanApplyCountsClaimsByDependencyType pins which stored bd
// types the chronic count reads: the three types that assert a fix failed
// count, a follow-up and a relation that says nothing about a fix do not.
func TestFixRecurrenceScanApplyCountsClaimsByDependencyType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		depType string
		chronic bool
	}{
		{name: "related counts", depType: "related", chronic: true},
		{name: "caused-by counts", depType: "caused-by", chronic: true},
		{name: "discovered-from claims nothing", depType: "discovered-from"},
		{name: "blocks claims nothing", depType: "blocks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: fixRecurrenceListJSON(t,
				fixRecurrenceRow{ID: "gt-newa", Title: "recurrence of gt-olda", Status: "open", CreatedAt: "2026-11-01T00:00:00Z"},
				fixRecurrenceRow{ID: "gt-newb", Title: "first claim", Status: "open", CreatedAt: "2026-11-02T00:00:00Z",
					Dependencies: []fixRecurrenceDepRow{{DependsOnID: "gt-olda", Type: tt.depType}}},
				fixRecurrenceRow{ID: "gt-newc", Title: "second claim", Status: "open", CreatedAt: "2026-11-03T00:00:00Z",
					Dependencies: []fixRecurrenceDepRow{{DependsOnID: "gt-olda", Type: tt.depType}}},
				fixRecurrenceRow{ID: "gt-olda", Title: "fix one", Status: "closed", CreatedAt: "2026-10-01T00:00:00Z", ClosedAt: "2026-10-02T00:00:00Z"},
			)}
			var out bytes.Buffer
			if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", false, true); err != nil {
				t.Fatal(err)
			}
			escalated := false
			for _, call := range fixRecurrenceCallLines(bd) {
				if strings.HasPrefix(call, "gt escalate ") {
					escalated = true
				}
			}
			if escalated != tt.chronic {
				t.Errorf("escalated = %t for a %q edge, want %t:\n%s", escalated, tt.depType, tt.chronic, strings.Join(fixRecurrenceCallLines(bd), "\n"))
			}
		})
	}
}

// TestFixRecurrenceScanReadsBDsRecordedListShape drives the scan over a bd list
// payload in the shape a read receives (testdata/fix_recurrence_bd_list.json:
// bd's list rows, whose dependency links carry issue_id, depends_on_id, type,
// created_at, created_by and metadata). The two links a stored "related" edge
// joins are read from depends_on_id, which keeps them out of the proposals, and
// the same type counts the two claims that make the third citation chronic.
func TestFixRecurrenceScanReadsBDsRecordedListShape(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "fix_recurrence_bd_list.json"))
	if err != nil {
		t.Fatalf("reading the recorded bd list: %v", err)
	}
	bd := &fixRecurrenceFakeBD{out: string(raw)}
	var out bytes.Buffer
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", false, true); err != nil {
		t.Fatal(err)
	}
	fixRecurrenceCheckCalls(t, bd, []string{
		"bd list --all --limit 0 --json --include-infra",
		"bd dep add --type=related -- zz-cites zz-oldfix",
		"bd update --add-label=recurrence-of:zz-oldfix -- zz-cites",
		"bd update --add-label=recurred -- zz-oldfix",
		"bd update --add-label=chronic -- zz-oldfix",
		"gt escalate chronic fix zz-oldfix: 3 beads report it did not hold --severity medium --fingerprint fix-recurrence:zz-oldfix",
	})
}

func TestFixRecurrenceApplyRefusesAnUndocumentedKind(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{}
	var out bytes.Buffer
	err := applyFixRecurrenceActions(&out, fixRecurrenceTestDeps(bd), "/town/gastown", []fixrecurrence.Action{
		{Kind: "frobnicate", Args: []string{"gt-olda"}},
	})
	if err == nil || !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("an undocumented kind = %v, want it named", err)
	}
	if len(bd.calls) != 0 {
		t.Errorf("ran %v for an action no program runs", bd.calls)
	}
}

func TestFixRecurrenceApplyReportWriteErrorIsReturned(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{}
	err := applyFixRecurrenceActions(fixRecurrenceRefusingWriter{}, fixRecurrenceTestDeps(bd), "/town/gastown", []fixrecurrence.Action{
		{Kind: fixrecurrence.KindLabelAdd, Args: []string{"update", "--add-label=recurred", "--", "gt-olda"}},
	})
	if err == nil || !strings.Contains(err.Error(), "reporting the label-add write") {
		t.Fatalf("a refused report = %v, want it named", err)
	}
	fixRecurrenceCheckCalls(t, bd, []string{"bd update --add-label=recurred -- gt-olda"})
}

// TestFixRecurrenceScanApplyFlagReachesTheRunner pins that the flag a reader
// types is what turns the plan on.
func TestFixRecurrenceScanApplyFlagReachesTheRunner(t *testing.T) {
	t.Parallel()
	bd := &fixRecurrenceFakeBD{out: fixRecurrenceChronicProposals(t)}
	cmd := newFixRecurrenceScanCmd(fixRecurrenceTestDeps(bd))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--apply"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	fixRecurrenceCheckCalls(t, bd, fixRecurrenceChronicCalls)
}

// fixRecurrenceUnusedBD is the bd seam for a case about a program the plan
// never runs: a reach into bd fails the test it belongs to.
func fixRecurrenceUnusedBD(t *testing.T) fixRecurrenceBD {
	t.Helper()
	return func(string, []string) ([]byte, error) {
		t.Error("ran bd for a case about another program")
		return nil, errors.New("bd should not run")
	}
}

// fixRecurrenceUnusedGT is the gt seam for a case about a program the plan
// never runs.
func fixRecurrenceUnusedGT(t *testing.T) fixRecurrenceRun {
	t.Helper()
	return func(string, []string) ([]byte, error) {
		t.Error("ran gt for a case about another program")
		return nil, errors.New("gt should not run")
	}
}

func TestFixRecurrenceRunProgramRefusesWhatThePlanNeverNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "empty argv", want: "no program to run"},
		{name: "unknown program", argv: []string{"sh", "-c", "true"}, want: `refusing to run "sh"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := fixRecurrenceRunProgram(fixRecurrenceUnusedBD(t), fixRecurrenceUnusedGT(t))
			_, err := run("/town/gastown", tt.argv)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestFixRecurrenceRunProgramGivesBDToItsSeam(t *testing.T) {
	t.Parallel()
	var dir string
	var args []string
	runBD := func(d string, a []string) ([]byte, error) {
		dir, args = d, a
		return []byte("written"), nil
	}
	out, err := fixRecurrenceRunProgram(runBD, fixRecurrenceUnusedGT(t))("/town/gastown", []string{"bd", "dep", "add", "--", "gt-newa", "gt-olda"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "written" || dir != "/town/gastown" || strings.Join(args, " ") != "dep add -- gt-newa gt-olda" {
		t.Errorf("bd program ran as out=%q dir=%q args=%v", out, dir, args)
	}
}

func TestFixRecurrenceRunProgramGivesGTToItsExec(t *testing.T) {
	t.Parallel()
	var dir string
	var argv []string
	exec := func(d string, a []string) ([]byte, error) {
		dir, argv = d, a
		return []byte("raised"), nil
	}
	out, err := fixRecurrenceRunProgram(fixRecurrenceUnusedBD(t), exec)("/town/gastown", []string{"gt", "escalate", "chronic fix gt-olda"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "raised" || dir != "/town/gastown" || strings.Join(argv, " ") != "escalate chronic fix gt-olda" {
		t.Errorf("gt program ran as out=%q dir=%q argv=%v", out, dir, argv)
	}
}

// fixRecurrenceExitStatus is a failed program's exit status without captured
// output, as exec.ExitError carries it.
type fixRecurrenceExitStatus int

func (e fixRecurrenceExitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (e fixRecurrenceExitStatus) ExitCode() int { return int(e) }

// TestFixRecurrenceRunProgramWithholdsBdOutput pins that a failed bd call is
// reported by its command and exit status: CLIError puts bd's stderr in its
// message, and captured output can hold a credential, so none of it may reach
// the error.
func TestFixRecurrenceRunProgramWithholdsBdOutput(t *testing.T) {
	t.Parallel()
	const captured = "Error: a captured stderr line"
	runBD := func(string, []string) ([]byte, error) {
		return nil, &beads.CLIError{
			Args:   []string{"dep", "add"},
			Stderr: []byte(captured + "\n"),
			Err:    fixRecurrenceExitStatus(1),
		}
	}
	_, err := fixRecurrenceRunProgram(runBD, fixRecurrenceUnusedGT(t))("/town/gastown", []string{"bd", "dep", "add", "--", "gt-newa", "gt-olda"})
	if err == nil {
		t.Fatal("a failed bd write returned nil")
	}
	if msg := err.Error(); !strings.Contains(msg, "bd dep add -- gt-newa gt-olda") || !strings.Contains(msg, "exit status 1") {
		t.Errorf("error %q does not name the command and its exit status", msg)
	}
	if strings.Contains(err.Error(), captured) {
		t.Error("the error carries bd's captured stderr")
	}
}

// TestFixRecurrenceRunProgramReportsAFailureWithNoExitStatus pins the other
// refusal: an error that carries no status is still named by its command, and
// its text is withheld the same way.
func TestFixRecurrenceRunProgramReportsAFailureWithNoExitStatus(t *testing.T) {
	t.Parallel()
	const captured = "Error: a captured stderr line"
	runBD := func(string, []string) ([]byte, error) { return nil, errors.New(captured) }
	_, err := fixRecurrenceRunProgram(runBD, fixRecurrenceUnusedGT(t))("/town/gastown", []string{"bd", "list", "--json"})
	if err == nil {
		t.Fatal("a failed bd read returned nil")
	}
	if msg := err.Error(); !strings.Contains(msg, "bd list --json") || !strings.Contains(msg, "failed with no exit status") {
		t.Errorf("error %q does not name the command and its missing status", msg)
	}
	if strings.Contains(err.Error(), captured) {
		t.Error("the error carries the failed call's captured text")
	}
}

func TestFixRecurrenceRunProgramReturnsAFailedGT(t *testing.T) {
	t.Parallel()
	exec := func(string, []string) ([]byte, error) { return nil, fixRecurrenceExitStatus(1) }
	_, err := fixRecurrenceRunProgram(fixRecurrenceUnusedBD(t), exec)("/town/gastown", []string{"gt", "escalate"})
	if err == nil || !strings.Contains(err.Error(), "gt escalate") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error = %v, want the failed gt named", err)
	}
}

// TestFixRecurrenceGTBinaryFallsBack pins the order a gt write resolves its
// binary in: this program first, then gt from PATH, then the PATH name itself
// so the exec reports what it could not find.
func TestFixRecurrenceGTBinaryFallsBack(t *testing.T) {
	t.Parallel()
	executableErr := errors.New("no executable path")
	lookPathErr := errors.New("gt not on PATH")
	tests := []struct {
		name string
		// executableErr, when set, is what naming this binary returns.
		executableErr error
		// found, when set, is what the PATH lookup returns.
		found   string
		findErr error
		want    string
		// lookedUp is whether the PATH lookup must run at all.
		lookedUp bool
	}{
		{name: "this binary", want: "/opt/gt/bin/gt"},
		{
			name:          "gt from PATH when this binary cannot be named",
			executableErr: executableErr, found: "/usr/local/bin/gt", want: "/usr/local/bin/gt", lookedUp: true,
		},
		{
			name:          "the PATH name when neither lookup answers",
			executableErr: executableErr, findErr: lookPathErr, want: "gt", lookedUp: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var looked []string
			executable := func() (string, error) {
				if tt.executableErr != nil {
					return "", tt.executableErr
				}
				return "/opt/gt/bin/gt", nil
			}
			lookPath := func(name string) (string, error) {
				looked = append(looked, name)
				return tt.found, tt.findErr
			}
			if got := fixRecurrenceGTBinary(executable, lookPath); got != tt.want {
				t.Errorf("binary = %q, want %q", got, tt.want)
			}
			if tt.lookedUp {
				if len(looked) != 1 || looked[0] != "gt" {
					t.Errorf("PATH lookups = %v, want one for gt", looked)
				}
			} else if len(looked) != 0 {
				t.Errorf("PATH lookups = %v, want none with this binary available", looked)
			}
		})
	}
}
