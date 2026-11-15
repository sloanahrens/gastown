package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// fixRecurrenceFakeBD answers the scan's one bd read from inline JSON and
// records every call, so a test sees the read's argv and its directory and no
// bd process starts.
type fixRecurrenceFakeBD struct {
	out   string
	err   error
	dirs  []string
	calls [][]string
}

func (f *fixRecurrenceFakeBD) run(dir string, args []string) ([]byte, error) {
	f.dirs = append(f.dirs, dir)
	f.calls = append(f.calls, args)
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
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", true); err != nil {
		t.Fatal(err)
	}
	want := `{"new":"gt-newamb","old":"gt-olda","relationship":"reference","confidence":0,"source":"ambiguous","ambiguous":true}
{"new":"gt-newre","old":"gt-olda","relationship":"recurrence","confidence":0.9,"source":"title"}
{"new":"gt-newreg","old":"gt-oldb","relationship":"regression","confidence":0.9,"source":"title"}
`
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
	// The scan is read-only: one bd list, in the rig directory, and nothing else.
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
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", false); err != nil {
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
			if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", true); err != nil {
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
	err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", true)
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
				if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", asJSON); err != nil {
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &fixRecurrenceFakeBD{out: tt.out}
			var out bytes.Buffer
			err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "", true)
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
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "7d", true); err != nil {
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
	if err := runFixRecurrenceScan(&out, fixRecurrenceTestDeps(bd), "banana", true); err == nil {
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
	err := runFixRecurrenceScan(&out, deps, "", true)
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
		err := runFixRecurrenceScan(fixRecurrenceRefusingWriter{}, fixRecurrenceTestDeps(bd), "", asJSON)
		if err == nil {
			t.Fatalf("a refused write (json=%t) returned nil", asJSON)
		}
	}
}
