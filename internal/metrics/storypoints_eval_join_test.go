package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/storypoints/estlog"
)

// evalJoinAt is an hour on the fixture day, so landings written out of order
// have distinct, comparable times.
func evalJoinAt(hour int) time.Time {
	return time.Date(2026, 10, 1, hour, 0, 0, 0, time.UTC)
}

// evalJoinTown is a fake town with the directories the join reads.
func evalJoinTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	for _, dir := range []string{"landings", "storypoints"} {
		if err := os.MkdirAll(filepath.Join(town, ".runtime", dir), 0o700); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
	}
	return town
}

// evalJoinLanding is one landing record as the landing writer records it. It
// carries no rig: the ledger's filename names the rig.
func evalJoinLanding(bead string, secs float64, landed time.Time, lower, upper bool) land.LandingRecord {
	return land.LandingRecord{
		BeadID:            bead,
		LandedAt:          landed,
		PolecatSecs:       secs,
		PolecatLowerBound: lower,
		PolecatUpperBound: upper,
	}
}

// evalJoinWriteLandings writes recs as one rig's ledger, one JSON record per
// line as the landing writer appends them.
func evalJoinWriteLandings(t *testing.T, town, rig string, recs ...land.LandingRecord) {
	t.Helper()
	var b bytes.Buffer
	for _, rec := range recs {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal landing %s: %v", rec.BeadID, err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	path := filepath.Join(town, ".runtime", "landings", rig+".jsonl")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s ledger: %v", rig, err)
	}
}

// evalJoinAppendEstimate appends one estimate-log line through the production
// writer.
func evalJoinAppendEstimate(t *testing.T, town string, rec estlog.Record) {
	t.Helper()
	if err := estlog.Append(town, rec); err != nil {
		t.Fatalf("append estimate for %s: %v", rec.BeadID, err)
	}
}

// evalJoinEstimate is one estimate-log line: the blend and its inputs in the
// layout the join reads.
func evalJoinEstimate(bead string, blended float64, shape, embed, jev *float64) estlog.Record {
	return estlog.Record{
		BeadID:         bead,
		BlendedMinutes: blended,
		Minutes:        estlog.Minutes{Shape: shape, Embed: embed, Jev: jev},
	}
}

// checkEvalJoinFloat compares one optional minutes input of a row.
func checkEvalJoinFloat(t *testing.T, name string, got, want *float64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %v, want nil", name, *got)
	case want != nil && got == nil:
		t.Errorf("%s = nil, want %v", name, *want)
	case want != nil && *got != *want:
		t.Errorf("%s = %v, want %v", name, *got, *want)
	}
}

// TestStoryPointsEvalJoinRows: the row carries the latest estimate the log
// holds for the bead, the ledger's rig and the measured time, and the rows come
// back ordered by landed time even when the landings were written across two
// ledgers out of order.
func TestStoryPointsEvalJoinRows(t *testing.T) {
	t.Parallel()
	town := evalJoinTown(t)
	evalJoinWriteLandings(t, town, "gastown",
		evalJoinLanding("gt-a", 600, evalJoinAt(2), false, false),
		evalJoinLanding("gt-b", 1200, evalJoinAt(1), false, false),
	)
	evalJoinWriteLandings(t, town, "beads",
		evalJoinLanding("bd-c", 180, evalJoinAt(3), false, false),
	)
	// gt-a was estimated twice; the later line is the estimate in force.
	evalJoinAppendEstimate(t, town, evalJoinEstimate("gt-a", 30, floatPtr(20), floatPtr(40), floatPtr(45)))
	evalJoinAppendEstimate(t, town, evalJoinEstimate("gt-a", 12.5, floatPtr(10), nil, floatPtr(15)))
	evalJoinAppendEstimate(t, town, evalJoinEstimate("gt-b", 24, floatPtr(24), nil, nil))
	evalJoinAppendEstimate(t, town, evalJoinEstimate("bd-c", 3.3, floatPtr(3), floatPtr(3.6), nil))

	rows, counts, err := joinEval(town)
	if err != nil {
		t.Fatalf("joinEval: %v", err)
	}
	if want := (EvalCounts{Records: 3, Joined: 3}); counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	wantOrder := []string{"gt-b", "gt-a", "bd-c"}
	for i, want := range wantOrder {
		if rows[i].Bead != want {
			t.Errorf("rows[%d].Bead = %q, want %q (rows ordered by landed time)", i, rows[i].Bead, want)
		}
	}
	// gt-b landed first, with only the shape input.
	if got := rows[0]; got.Rig != "gastown" || !got.LandedAt.Equal(evalJoinAt(1)) {
		t.Errorf("rows[0] rig, landed = %q, %s; want %q, %s", got.Rig, got.LandedAt, "gastown", evalJoinAt(1))
	}
	if got := rows[0]; got.ActualMinutes != 20 || got.BlendedMinutes != 24 {
		t.Errorf("rows[0] actual, blended = %v, %v; want 20, 24", got.ActualMinutes, got.BlendedMinutes)
	}
	checkEvalJoinFloat(t, "gt-b shape", rows[0].ShapeMinutes, floatPtr(24))
	checkEvalJoinFloat(t, "gt-b embed", rows[0].EmbedMinutes, nil)
	checkEvalJoinFloat(t, "gt-b jev", rows[0].JevMinutes, nil)
	// gt-a carries the later of its two estimates, and no embedding input.
	if got := rows[1]; got.ActualMinutes != 10 || got.BlendedMinutes != 12.5 {
		t.Errorf("rows[1] actual, blended = %v, %v; want 10, 12.5 (the later estimate)", got.ActualMinutes, got.BlendedMinutes)
	}
	checkEvalJoinFloat(t, "gt-a shape", rows[1].ShapeMinutes, floatPtr(10))
	checkEvalJoinFloat(t, "gt-a embed", rows[1].EmbedMinutes, nil)
	checkEvalJoinFloat(t, "gt-a jev", rows[1].JevMinutes, floatPtr(15))
	// A bead from the other rig takes that ledger's name.
	if got := rows[2]; got.Rig != "beads" {
		t.Errorf("rows[2].Rig = %q, want %q (the ledger names the rig)", got.Rig, "beads")
	}
	checkEvalJoinFloat(t, "bd-c embed", rows[2].EmbedMinutes, floatPtr(3.6))
}

// TestStoryPointsEvalJoinDropsUnmeasuredTimes: a lower-bound, an upper-bound and
// an absent polecat time are each dropped and counted under their own reason,
// and a landing with no estimate is counted as such rather than under a time
// reason.
func TestStoryPointsEvalJoinDropsUnmeasuredTimes(t *testing.T) {
	t.Parallel()
	town := evalJoinTown(t)
	evalJoinWriteLandings(t, town, "gastown",
		evalJoinLanding("gt-lower", 300, evalJoinAt(1), true, false),
		evalJoinLanding("gt-upper", 400, evalJoinAt(2), false, true),
		evalJoinLanding("gt-absent", 0, evalJoinAt(3), false, false),
		evalJoinLanding("gt-nolog", 500, evalJoinAt(4), false, false),
		evalJoinLanding("gt-measured", 600, evalJoinAt(5), false, false),
	)
	for _, bead := range []string{"gt-lower", "gt-upper", "gt-absent", "gt-measured"} {
		evalJoinAppendEstimate(t, town, evalJoinEstimate(bead, 10, floatPtr(10), nil, nil))
	}

	rows, counts, err := joinEval(town)
	if err != nil {
		t.Fatalf("joinEval: %v", err)
	}
	want := EvalCounts{Records: 5, Joined: 1, NoEstimate: 1, LowerBound: 1, UpperBound: 1, Absent: 1}
	if counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
	if len(rows) != 1 || rows[0].Bead != "gt-measured" {
		t.Fatalf("rows = %+v, want only gt-measured", rows)
	}
}

// TestStoryPointsEvalJoinAbsentInputs: a town that has never landed anything, or
// has landings but no estimate log, has no input to read rather than an unreadable
// one: the join returns no rows and no error.
func TestStoryPointsEvalJoinAbsentInputs(t *testing.T) {
	t.Parallel()
	t.Run("no runtime at all", func(t *testing.T) {
		t.Parallel()
		rows, counts, err := joinEval(t.TempDir())
		if err != nil {
			t.Fatalf("joinEval: %v", err)
		}
		if len(rows) != 0 || counts != (EvalCounts{}) {
			t.Errorf("rows, counts = %+v, %+v; want none and zero", rows, counts)
		}
	})
	t.Run("landings but no log", func(t *testing.T) {
		t.Parallel()
		town := evalJoinTown(t)
		evalJoinWriteLandings(t, town, "gastown", evalJoinLanding("gt-a", 600, evalJoinAt(1), false, false))
		rows, counts, err := joinEval(town)
		if err != nil {
			t.Fatalf("joinEval: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("rows = %+v, want none without an estimate", rows)
		}
		if want := (EvalCounts{Records: 1, NoEstimate: 1}); counts != want {
			t.Errorf("counts = %+v, want %+v", counts, want)
		}
	})
}

// TestStoryPointsEvalJoinCountsMalformedLogLines: a torn estimate-log line is
// skipped and counted by the log reader, and the lines around it still join.
func TestStoryPointsEvalJoinCountsMalformedLogLines(t *testing.T) {
	t.Parallel()
	town := evalJoinTown(t)
	evalJoinWriteLandings(t, town, "gastown", evalJoinLanding("gt-a", 600, evalJoinAt(1), false, false))
	evalJoinAppendEstimate(t, town, evalJoinEstimate("gt-a", 10, floatPtr(10), nil, nil))
	torn := filepath.Join(town, ".runtime", "storypoints", "estimates.jsonl")
	f, err := os.OpenFile(torn, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the estimate log: %v", err)
	}
	if _, err := f.WriteString("not an estimate\n"); err != nil {
		t.Fatalf("write the torn line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close the estimate log: %v", err)
	}

	rows, counts, err := joinEval(town)
	if err != nil {
		t.Fatalf("joinEval: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %+v, want the one bead with an estimate", rows)
	}
	if want := (EvalCounts{Records: 1, Joined: 1, MalformedLog: 1}); counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
}

// evalJoinAppendRaw appends line to a file under the town's .runtime as a torn
// write leaves it: bytes its reader cannot decode.
func evalJoinAppendRaw(t *testing.T, town, rel, line string) {
	t.Helper()
	path := filepath.Join(town, ".runtime", rel)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", rel, err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("write the torn line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", rel, err)
	}
}

// TestStoryPointsEvalJoinCountsMalformedLandingLines: a torn landing line is
// skipped by the landing reader and counted, as a torn log line is.
func TestStoryPointsEvalJoinCountsMalformedLandingLines(t *testing.T) {
	t.Parallel()
	town := evalJoinTown(t)
	evalJoinWriteLandings(t, town, "gastown", evalJoinLanding("gt-a", 600, evalJoinAt(1), false, false))
	evalJoinAppendRaw(t, town, "landings/gastown.jsonl", "not a landing")
	evalJoinAppendEstimate(t, town, evalJoinEstimate("gt-a", 10, floatPtr(10), nil, nil))

	_, counts, err := joinEval(town)
	if err != nil {
		t.Fatalf("joinEval: %v", err)
	}
	if want := (EvalCounts{Records: 1, Joined: 1, MalformedLanding: 1}); counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}
}

// TestStoryPointsEvalJoinCountsMalformedLines pins how the two malformed counts
// are taken: a torn landing line among good ones is counted and the good ones
// still join, each torn line counts, the counts of two ledgers add, and a torn
// line in one input never lands in the other's count.
func TestStoryPointsEvalJoinCountsMalformedLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		ledgers map[string][]string // rig -> its lines: a bead id, or "torn"
		logTorn int                 // torn lines appended to the estimate log
		want    EvalCounts
	}{
		{"one torn among good", map[string][]string{"gastown": {"gt-a", "torn", "gt-b"}}, 0, EvalCounts{Records: 2, Joined: 2, MalformedLanding: 1}},
		{"two torn", map[string][]string{"gastown": {"gt-a", "torn", "torn"}}, 0, EvalCounts{Records: 1, Joined: 1, MalformedLanding: 2}},
		{"torn log only", map[string][]string{"gastown": {"gt-a"}}, 1, EvalCounts{Records: 1, Joined: 1, MalformedLog: 1}},
		{"torn in both", map[string][]string{"gastown": {"gt-a", "torn"}}, 1, EvalCounts{Records: 1, Joined: 1, MalformedLog: 1, MalformedLanding: 1}},
		{"two ledgers add", map[string][]string{"gastown": {"gt-a", "torn"}, "beaver": {"gt-b", "torn", "torn"}}, 0, EvalCounts{Records: 2, Joined: 2, MalformedLanding: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := evalJoinTown(t)
			for rig, lines := range tc.ledgers {
				var b bytes.Buffer
				for i, line := range lines {
					if line == "torn" {
						b.WriteString("not a landing\n")
						continue
					}
					data, err := json.Marshal(evalJoinLanding(line, 600, evalJoinAt(i+1), false, false))
					if err != nil {
						t.Fatalf("marshal landing %s: %v", line, err)
					}
					b.Write(append(data, '\n'))
					evalJoinAppendEstimate(t, town, evalJoinEstimate(line, 10, floatPtr(10), nil, nil))
				}
				if err := os.WriteFile(filepath.Join(town, ".runtime", "landings", rig+".jsonl"), b.Bytes(), 0o600); err != nil {
					t.Fatalf("write %s ledger: %v", rig, err)
				}
			}
			for range tc.logTorn {
				evalJoinAppendRaw(t, town, "storypoints/estimates.jsonl", "not an estimate")
			}
			_, counts, err := joinEval(town)
			if err != nil {
				t.Fatalf("joinEval: %v", err)
			}
			if counts != tc.want {
				t.Errorf("counts = %+v, want %+v", counts, tc.want)
			}
		})
	}
}

// TestStoryPointsEvalJoinErrorsNameTheFile: every input the join cannot read,
// and every landing record it cannot place, is an error naming the file or the
// town root, never an empty answer that reads as a town with nothing to
// evaluate.
func TestStoryPointsEvalJoinErrorsNameTheFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T) (town, want string) // the town root and the path the error must name
		err   error                                  // sentinel the error must match, when set
	}{
		{
			name: "no town root",
			setup: func(t *testing.T) (string, string) {
				return "", ""
			},
			err: errEvalJoinNoTown,
		},
		{
			name: "landings directory is a file",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				town := t.TempDir()
				path := filepath.Join(town, ".runtime", "landings")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("make .runtime: %v", err)
				}
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatalf("write the landings path: %v", err)
				}
				return town, path
			},
		},
		{
			name: "unreadable ledger",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				town := evalJoinTown(t)
				// A directory where the ledger should be: the reader opens it
				// and cannot read a record from it.
				path := filepath.Join(town, ".runtime", "landings", "gastown.jsonl")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("make the ledger path: %v", err)
				}
				return town, path
			},
		},
		{
			name: "ledger that cannot be opened",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				town := evalJoinTown(t)
				// A loop opens with ELOOP, a failure that is not a missing file
				// and that the reader alone does not name.
				path := filepath.Join(town, ".runtime", "landings", "gastown.jsonl")
				if err := os.Symlink(path, path); err != nil {
					t.Fatalf("make the ledger loop: %v", err)
				}
				return town, path
			},
		},
		{
			name: "unreadable estimate log",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				town := evalJoinTown(t)
				path := filepath.Join(town, ".runtime", "storypoints", "estimates.jsonl")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("make the log path: %v", err)
				}
				return town, path
			},
		},
		{
			name: "record names no bead",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				town := evalJoinTown(t)
				evalJoinWriteLandings(t, town, "gastown", land.LandingRecord{LandedAt: evalJoinAt(1)})
				return town, filepath.Join(town, ".runtime", "landings", "gastown.jsonl")
			},
		},
		{
			name: "record has no landed time",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				town := evalJoinTown(t)
				evalJoinWriteLandings(t, town, "gastown", land.LandingRecord{BeadID: "gt-a", PolecatSecs: 60})
				return town, filepath.Join(town, ".runtime", "landings", "gastown.jsonl")
			},
		},
		{
			name: ".jsonl file names no rig",
			setup: func(t *testing.T) (string, string) {
				t.Helper()
				town := evalJoinTown(t)
				path := filepath.Join(town, ".runtime", "landings", ".jsonl")
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatalf("write the rigless file: %v", err)
				}
				return town, path
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, want := tc.setup(t)
			_, _, err := joinEval(town)
			if err == nil {
				t.Fatalf("joinEval(%q) = nil error, want an error", tc.name)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("error = %v, want %v", err, tc.err)
			}
			if want != "" && !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to name %q", err, want)
			}
		})
	}
}
