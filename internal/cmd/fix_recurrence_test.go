package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

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

// TestFixRecurrenceScanSuppressesAPairWithAnEdgeEitherWay pins that a relation
// bd prints on one row suppresses the pair whichever end it appears on: the
// reverse relation exists only in the other bead's dependencies array, so the
// row bd printed it on is the wrong end to search.
func TestFixRecurrenceScanSuppressesAPairWithAnEdgeEitherWay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
	}{
		{
			name: "citing bead carries the edge",
			out: `[
  {"id":"gt-newe1","title":"recurrence of gt-olde1","status":"open","created_at":"2026-11-01T00:00:00Z","dependencies":[{"depends_on_id":"gt-olde1"}]},
  {"id":"gt-olde1","title":"fix","status":"closed","created_at":"2026-10-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"}
]`,
		},
		{
			name: "cited bead carries the edge",
			out: `[
  {"id":"gt-newe1","title":"recurrence of gt-olde1","status":"open","created_at":"2026-11-01T00:00:00Z"},
  {"id":"gt-olde1","title":"fix","status":"closed","created_at":"2026-10-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z","dependencies":[{"depends_on_id":"gt-newe1"}]}
]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: tt.out}
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
const fixRecurrenceChronicProposals = `[
  {"id":"gt-newa","title":"recurrence of gt-olda","status":"open","created_at":"2026-11-01T00:00:00Z"},
  {"id":"gt-newb","title":"first claim","status":"open","created_at":"2026-11-02T00:00:00Z","dependencies":[{"depends_on_id":"gt-olda","type":"related"}]},
  {"id":"gt-newc","title":"second claim","status":"open","created_at":"2026-11-03T00:00:00Z","dependencies":[{"depends_on_id":"gt-olda","type":"related"}]},
  {"id":"gt-olda","title":"fix one","status":"closed","created_at":"2026-10-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"}
]`

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
	bd := &fixRecurrenceFakeBD{out: fixRecurrenceChronicProposals}
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
			bd := &fixRecurrenceFakeBD{out: `[
  {"id":"gt-newa","title":"recurrence of gt-olda","status":"open","created_at":"2026-11-01T00:00:00Z"},
  {"id":"gt-newb","title":"first claim","status":"open","created_at":"2026-11-02T00:00:00Z","dependencies":[{"depends_on_id":"gt-olda","type":"` + tt.depType + `"}]},
  {"id":"gt-newc","title":"second claim","status":"open","created_at":"2026-11-03T00:00:00Z","dependencies":[{"depends_on_id":"gt-olda","type":"` + tt.depType + `"}]},
  {"id":"gt-olda","title":"fix one","status":"closed","created_at":"2026-10-01T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"}
]`}
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
	bd := &fixRecurrenceFakeBD{out: fixRecurrenceChronicProposals}
	cmd := newFixRecurrenceScanCmd(fixRecurrenceTestDeps(bd))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--apply"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	fixRecurrenceCheckCalls(t, bd, fixRecurrenceChronicCalls)
}

func TestFixRecurrenceRunProgramRefusesWhatThePlanNeverNames(t *testing.T) {
	t.Parallel()
	exec := func(string, []string) ([]byte, error) { return nil, errors.New("ran a gt program") }
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
			_, err := fixRecurrenceRunProgram(exec)("/town/gastown", tt.argv)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
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
	out, err := fixRecurrenceRunProgram(exec)("/town/gastown", []string{"gt", "escalate", "chronic fix gt-olda"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "raised" || dir != "/town/gastown" || strings.Join(argv, " ") != "escalate chronic fix gt-olda" {
		t.Errorf("gt program ran as out=%q dir=%q argv=%v", out, dir, argv)
	}
}

func TestFixRecurrenceRunProgramReturnsAFailedGT(t *testing.T) {
	t.Parallel()
	exec := func(string, []string) ([]byte, error) { return nil, errors.New("exit status 1") }
	_, err := fixRecurrenceRunProgram(exec)("/town/gastown", []string{"gt", "escalate"})
	if err == nil || !strings.Contains(err.Error(), "running gt") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error = %v, want the failed gt named", err)
	}
}
