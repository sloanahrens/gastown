package metrics

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

// splitRows builds n landings landed one day apart in bead order, so sorting by
// landed_at leaves the order the slice already has.
func splitRows(n int) []Row {
	rows := make([]Row, n)
	for i := range rows {
		rows[i] = Row{"bead": fmt.Sprintf("gt-b%02d", i), "landed_at": fmt.Sprintf("2026-10-%02dT00:00:00Z", i+1)}
	}
	return rows
}

func TestSplitByDateMatchesThePythonSplit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rows     int
		fraction float64
		want     int
	}{
		{10, 0.7, 7},
		{10, 1.0, 10},
		{11, 0.7, 7},
		{11, 1.0, 11},
		{0, 0.7, 0},
		{0, 1.0, 0},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%d rows at %v", tc.rows, tc.fraction), func(t *testing.T) {
			t.Parallel()
			rows := splitRows(tc.rows)
			train, holdout := SplitByDate(rows, tc.fraction)
			if len(train) != tc.want {
				t.Errorf("train = %d rows, want the Python split's %d", len(train), tc.want)
			}
			if len(holdout) != tc.rows-tc.want {
				t.Errorf("holdout = %d rows, want %d", len(holdout), tc.rows-tc.want)
			}
			if tc.want > 0 && train[tc.want-1]["bead"] != rows[tc.want-1]["bead"] {
				t.Errorf("train ends at %q, want the earliest %d rows ending at %q",
					train[tc.want-1]["bead"], tc.want, rows[tc.want-1]["bead"])
			}
		})
	}
}

func TestSplitByDateClampsOutOfRangeFractions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fraction  float64
		wantTrain int
	}{
		{-0.5, 0},
		{1.5, 10},
		{2.0, 10},
		{math.NaN(), 0},
		{math.Inf(1), 10},
		{math.Inf(-1), 0},
		{1e300, 10},
		{-1e-300, 0},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("fraction %v", tc.fraction), func(t *testing.T) {
			t.Parallel()
			train, holdout := SplitByDate(splitRows(10), tc.fraction)
			if len(train) != tc.wantTrain {
				t.Errorf("train = %d rows, want the cut clamped to %d", len(train), tc.wantTrain)
			}
			if len(holdout) != 10-tc.wantTrain {
				t.Errorf("holdout = %d rows, want %d", len(holdout), 10-tc.wantTrain)
			}
		})
	}
}

func TestSplitByDateOrdersByLandedAtAndLeavesTheInputAlone(t *testing.T) {
	t.Parallel()
	rows := []Row{
		{"bead": "late", "landed_at": "2026-10-03T00:00:00Z"},
		{"bead": "early", "landed_at": "2026-10-01T00:00:00Z"},
		{"bead": "middle", "landed_at": "2026-10-02T00:00:00Z"},
	}
	train, holdout := SplitByDate(rows, 2.0/3.0)
	if len(train) != 2 || train[0]["bead"] != "early" || train[1]["bead"] != "middle" {
		t.Fatalf("train = %v, want early then middle", beadIDs(train))
	}
	if len(holdout) != 1 || holdout[0]["bead"] != "late" {
		t.Errorf("holdout = %v, want late", beadIDs(holdout))
	}
	if got := beadIDs(rows); strings.Join(got, ",") != "late,early,middle" {
		t.Errorf("input order = %v, want the caller's order unchanged", got)
	}
}

func TestSplitByDateKeepsInputOrderForEqualDates(t *testing.T) {
	t.Parallel()
	rows := []Row{
		{"bead": "first", "landed_at": "2026-10-01T00:00:00Z"},
		{"bead": "second", "landed_at": "2026-10-01T00:00:00Z"},
		{"bead": "third", "landed_at": "2026-10-01T00:00:00Z"},
	}
	train, holdout := SplitByDate(rows, 1.0/3.0)
	if got := strings.Join(beadIDs(train), ","); got != "first" {
		t.Errorf("train = %s, want first", got)
	}
	if got := strings.Join(beadIDs(holdout), ","); got != "second,third" {
		t.Errorf("holdout = %s, want second,third", got)
	}
}

func beadIDs(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r["bead"]
	}
	return out
}

// TestPrepareDropsEachRule pins one drop rule per table row: a header holding
// only that rule's columns must lose them, so deleting the rule from Prepare
// fails this row. The last row holds route under a target it does not define,
// where the leak rule must leave it in.
func TestPrepareDropsEachRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		header    []string
		explain   []string
		target    string
		predictor []string
	}{
		{"explain-only", []string{"gate_result", "lines_total", "rig"}, []string{"gate_result", "lines_total"},
			"rejected_before_landing", []string{"rig"}},
		{"targets", []string{"rejected_before_landing", "reached_overrun_stop", "rig"}, nil,
			"reached_overrun_stop", []string{"rig"}},
		{"identity", []string{"bead", "landed_at", "week", "rig"}, nil,
			"rejected_before_landing", []string{"rig"}},
		{"duplicate", []string{"model", "rig"}, nil,
			"rejected_before_landing", []string{"rig"}},
		{"route under another target", []string{"route", "rig"}, nil,
			"reached_overrun_stop", []string{"route", "rig"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := Prepare(tc.header, splitRows(3), tc.explain, tc.target)
			if got := strings.Join(p.Predictors, ","); got != strings.Join(tc.predictor, ",") {
				t.Errorf("predictors = %s, want %s", got, strings.Join(tc.predictor, ","))
			}
		})
	}
}

func TestPrepareBuildsPredictorsFromTheExtractHeader(t *testing.T) {
	t.Parallel()
	explainOnly := []string{"lines_added", "lines_deleted", "lines_total", "files_touched", "file_entropy",
		"package_entropy", "attempts", "gate_result", "om_verdict", "unreadable"}
	header := append(append([]string{}, landingColumns...), "unknown_metric")
	// Every target drops the whole target list and none defines a column of its
	// own, so the rows share one header; a per-target drop rule fails the row it
	// applies to.
	const predictors = "rig,route,manual_rescue,type,priority,shaped,label_count,om_threshold,dominant_model,unknown_metric"
	tests := []struct {
		target string
		want   string
	}{
		{"rejected_before_landing", predictors},
		{"reached_overrun_stop", predictors},
	}
	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			t.Parallel()
			p := Prepare(header, splitRows(10), explainOnly, tc.target)
			if got := strings.Join(p.Predictors, ","); got != tc.want {
				t.Errorf("predictors = %s, want %s", got, tc.want)
			}
			if got := strings.Join(p.Columns, ","); got != tc.want+","+tc.target {
				t.Errorf("columns = %s, want the predictors then %s", got, tc.target)
			}
		})
	}
}

func TestPrepareCountsPositivesFromTheTargetCell(t *testing.T) {
	t.Parallel()
	rows := splitRows(10)
	want := map[int]string{0: "1", 3: "1", 8: "1"}
	for i, r := range rows {
		r["rejected_before_landing"] = "0"
		if v, ok := want[i]; ok {
			r["rejected_before_landing"] = v
		}
	}
	rows[5]["manual_rescue"] = "1" // another target's positive is not this target's
	rows[2]["rejected_before_landing"] = ""

	p := Prepare(landingColumns, rows, nil, "rejected_before_landing")
	if len(p.Train) != 7 {
		t.Fatalf("train = %d rows, want the 0.7 split's 7", len(p.Train))
	}
	if p.TrainPositives != 2 {
		t.Errorf("train positives = %d, want 2 (rows 0 and 3)", p.TrainPositives)
	}
	if p.HoldoutPositives != 1 {
		t.Errorf("holdout positives = %d, want 1 (row 8)", p.HoldoutPositives)
	}
	if got := p.LastPositiveAt; got != rows[8]["landed_at"] {
		t.Errorf("last positive = %q, want row 8's %q", got, rows[8]["landed_at"])
	}
}

func TestPrepareCountsAPositiveWrittenAnotherWay(t *testing.T) {
	t.Parallel()
	rows := splitRows(10)
	for _, r := range rows {
		r["rejected_before_landing"] = "0"
	}
	rows[0]["rejected_before_landing"] = "1.0"
	rows[1]["rejected_before_landing"] = "TRUE"
	rows[3]["rejected_before_landing"] = "0.0"
	rows[4]["rejected_before_landing"] = "false"
	rows[8]["rejected_before_landing"] = "1.0"

	p := Prepare(landingColumns, rows, nil, "rejected_before_landing")
	if p.TrainPositives != 2 {
		t.Errorf("train positives = %d, want 2 (rows 0 and 1)", p.TrainPositives)
	}
	if p.HoldoutPositives != 1 {
		t.Errorf("holdout positives = %d, want 1 (row 8)", p.HoldoutPositives)
	}
	if got := p.LastPositiveAt; got != rows[8]["landed_at"] {
		t.Errorf("last positive = %q, want row 8's %q", got, rows[8]["landed_at"])
	}
}

func TestPrepareWithoutPositivesLeavesTheDateEmpty(t *testing.T) {
	t.Parallel()
	rows := splitRows(10)
	for _, r := range rows {
		r["reached_overrun_stop"] = "0"
	}
	p := Prepare(landingColumns, rows, nil, "reached_overrun_stop")
	if p.TrainPositives != 0 || p.HoldoutPositives != 0 {
		t.Errorf("positives = %d/%d, want none", p.TrainPositives, p.HoldoutPositives)
	}
	if p.LastPositiveAt != "" {
		t.Errorf("last positive = %q, want empty", p.LastPositiveAt)
	}
}

func TestReadSidecarReadsExplainOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"the sidecar's list", `{"explain_only": ["lines_total", "attempts"]}`, []string{"lines_total", "attempts"}},
		{"unknown keys are ignored", `{"explain_only": ["attempts"], "written_at": "2026-10-24"}`, []string{"attempts"}},
		{"a missing key is an empty list", `{}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadSidecar(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("read %s: %v", tc.in, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("explain_only = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadSidecarRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`{"explain_only": "lines_total"}`, `{"explain_only": [`} {
		if _, err := ReadSidecar(strings.NewReader(in)); err == nil {
			t.Errorf("read %s succeeded; want an error", in)
		}
	}
}

// bufferFiles is the WriteSplit seam held in memory: the path it was opened
// under maps to what was written.
type bufferFiles map[string]*bytes.Buffer

func (b bufferFiles) open(path string) (io.WriteCloser, error) {
	buf := &bytes.Buffer{}
	b[path] = buf
	return nopWriteCloser{buf}, nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestWriteSplitWritesThePredictorsAndTarget(t *testing.T) {
	t.Parallel()
	p := Prepared{
		Train:      []Row{{"rig": "gastown", "rejected_before_landing": "1", "lines_total": "9"}, {"rig": "beads", "rejected_before_landing": "0"}},
		Holdout:    []Row{{"rig": "hq", "rejected_before_landing": "1"}},
		Predictors: []string{"rig"},
		Columns:    []string{"rig", "rejected_before_landing"},
	}
	files := bufferFiles{}
	if err := WriteSplit(p, "run", files.open); err != nil {
		t.Fatalf("write split: %v", err)
	}
	want := map[string]string{
		filepath.Join("run", "train.csv"): "rig,rejected_before_landing\r\ngastown,1\r\nbeads,0\r\n",
		filepath.Join("run", "test.csv"):  "rig,rejected_before_landing\r\nhq,1\r\n",
	}
	if len(files) != len(want) {
		t.Fatalf("wrote %d files, want train.csv and test.csv", len(files))
	}
	for path, wantBody := range want {
		buf, ok := files[path]
		if !ok {
			t.Errorf("no file at %s", path)
			continue
		}
		if got := buf.String(); got != wantBody {
			t.Errorf("%s =\n%q\nwant\n%q", path, got, wantBody)
		}
	}
}

func TestWriteSplitReportsTheFailingFile(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk full")
	err := WriteSplit(Prepared{Columns: []string{"rig"}}, "run", func(path string) (io.WriteCloser, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), filepath.Join("run", "train.csv")) {
		t.Errorf("error = %v, want it to wrap %v and name train.csv", err, boom)
	}
}

// failFile is a split file a test makes fail, at its writes or at its close.
// It records that Close ran, which every path that opened the file owes the OS.
type failFile struct {
	writeErr, closeErr error
	closed             bool
}

func (f *failFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *failFile) Close() error {
	f.closed = true
	return f.closeErr
}

func TestWriteSplitReportsTheSecondFilesOpenFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("permission denied")
	train := &failFile{}
	err := WriteSplit(Prepared{Columns: []string{"rig"}}, "run", func(path string) (io.WriteCloser, error) {
		if filepath.Base(path) == "test.csv" {
			return nil, boom
		}
		return train, nil
	})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), filepath.Join("run", "test.csv")) {
		t.Errorf("error = %v, want it to wrap %v and name test.csv", err, boom)
	}
	if !train.closed {
		t.Error("train.csv was left open")
	}
}

func TestWriteSplitReportsAWriteFailureAndClosesTheFile(t *testing.T) {
	t.Parallel()
	writeErr, closeErr := errors.New("broken pipe"), errors.New("close failed")
	f := &failFile{writeErr: writeErr, closeErr: closeErr}
	err := WriteSplit(Prepared{Columns: []string{"rig"}, Train: []Row{{"rig": "gastown"}}}, "run",
		func(string) (io.WriteCloser, error) { return f, nil })
	if !errors.Is(err, writeErr) || !strings.Contains(err.Error(), filepath.Join("run", "train.csv")) {
		t.Errorf("error = %v, want it to wrap %v and name train.csv", err, writeErr)
	}
	if errors.Is(err, closeErr) {
		t.Errorf("error = %v, want the write failure rather than the close that followed it", err)
	}
	if !f.closed {
		t.Error("the failing file was left open")
	}
}

func TestWriteSplitReportsACloseFailure(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("flush failed")
	err := WriteSplit(Prepared{Columns: []string{"rig"}}, "run", func(path string) (io.WriteCloser, error) {
		f := &failFile{}
		if filepath.Base(path) == "test.csv" {
			f.closeErr = closeErr
		}
		return f, nil
	})
	if !errors.Is(err, closeErr) || !strings.Contains(err.Error(), filepath.Join("run", "test.csv")) {
		t.Errorf("error = %v, want it to wrap %v and name test.csv", err, closeErr)
	}
}
