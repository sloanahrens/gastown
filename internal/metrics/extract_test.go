package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The cases below drive the extract from fake sources, with every file it
// writes kept in memory: the same fixture town the ported cases use, so a case
// that replaces one has the same inputs and expected values.

// extractLedgerLine is one ledger.jsonl line of the fixture: a bead that landed
// one way, at one time.
func extractLedgerLine(t *testing.T, bead, rig, route, at string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"bead": bead, "rig": rig, "landed_commit": "c-" + bead, "landed_at": at,
		"route": route, "om_verdict": "approve", "gate_result": "build",
	})
	if err != nil {
		t.Fatalf("ledger line for %s: %v", bead, err)
	}
	return string(data)
}

// extractBdList is the fixture's `bd list`: gt-a is a shaped bead that a
// landing worker rejected once and that carries the handoff label; gt-b landed
// by hand and carries neither.
const extractBdList = `[
 {"id": "gt-a", "created_at": "2026-10-12T00:00:00Z", "closed_at": "2026-10-14T00:00:00Z", "issue_type": "task",
  "labels": ["handoff"], "dependency_count": 1, "comment_count": 0, "acceptance_criteria": "- [ ] x",
  "notes": "MERGE REJECTION (attempt 1): conflict"},
 {"id": "gt-b", "created_at": "2026-10-12T00:00:00Z", "closed_at": "2026-10-14T00:00:00Z", "issue_type": "task",
  "labels": [], "dependency_count": 3, "comment_count": 0}
]`

// extractBdShow is the fixture's `bd show`: a handoff bead whose notes and
// comments are all the size reader has to measure.
const extractBdShow = `[{"notes": "handoff notes", "comments": []}]`

// extractBd answers a list with the listing and any other call with the shown
// bead, recording every call.
func extractBd(calls *[]bdCall) BdRun {
	return func(dir string, args []string) ([]byte, bool) {
		*calls = append(*calls, bdCall{dir, args})
		if len(args) > 0 && args[0] == "list" {
			return []byte(extractBdList), true
		}
		return []byte(extractBdShow), true
	}
}

// extractFakeDiffs is the fixture's diff source: every commit has the same
// stats.
type extractFakeDiffs struct{}

func (extractFakeDiffs) Stats(context.Context, string, string) (DiffStat, error) {
	return DiffStat{LinesAdded: 5, LinesDeleted: 5, FilesTouched: 1}, nil
}

// extractNoDiffs is a diff source where no commit's diff can be read.
type extractNoDiffs struct{}

func (extractNoDiffs) Stats(context.Context, string, string) (DiffStat, error) {
	return DiffStat{}, fmt.Errorf("%w: the fixture holds no diffs", ErrDiffUnavailable)
}

// extractFiles stands in for the output directory: it records the directory the
// extract makes and the name of each file it opens, in order, and keeps the
// bytes each file received, so no test writes to or reads from the host.
type extractFiles struct {
	made  string
	names []string
	data  map[string]*bytes.Buffer
}

// newExtractFiles is an empty output directory.
func newExtractFiles() *extractFiles {
	return &extractFiles{data: map[string]*bytes.Buffer{}}
}

func (f *extractFiles) mkdirAll(dir string) error {
	f.made = dir
	return nil
}

func (f *extractFiles) create(name string) (io.WriteCloser, error) {
	f.names = append(f.names, name)
	buf := &bytes.Buffer{}
	f.data[name] = buf
	return bufferWriteCloser{buf}, nil
}

// has reports whether the extract opened name.
func (f *extractFiles) has(name string) bool {
	_, ok := f.data[name]
	return ok
}

// rows parses one captured CSV under its own header.
func (f *extractFiles) rows(t *testing.T, name string) []Row {
	t.Helper()
	buf, ok := f.data[name]
	if !ok {
		t.Fatalf("%s was not written; wrote %v", name, f.names)
	}
	rows, err := ReadCSV(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return rows
}

// bufferWriteCloser is one captured output file.
type bufferWriteCloser struct{ *bytes.Buffer }

func (bufferWriteCloser) Close() error { return nil }

// extractCase is the fixture town's env over an in-memory output directory: two
// landings, one shaped bead that was rejected once and carries the handoff
// label, one bead that landed by hand, a usage file and an overrun stop.
func extractCase(t *testing.T) (ExtractEnv, *extractFiles) {
	t.Helper()
	files := newExtractFiles()
	return ExtractEnv{
		LedgerLines: map[string][]string{"gastown": {
			extractLedgerLine(t, "gt-a", "gastown", "daemon", "2026-10-14T00:00:00Z"),
			extractLedgerLine(t, "gt-b", "gastown", "overseer-manual", "2026-10-14T00:00:00Z"),
		}},
		// No review lines: the rejection history comes from the bead notes.
		ReviewLines: []string{},
		Diffs:       extractFakeDiffs{},
		RunBd:       extractBd(&[]bdCall{}),
		BeadDirs:    []string{"/g"},
		PrefixToRig: map[string]string{"gt": "gastown"},
		UsageFiles: map[string]UsageFile{"gt-a": {
			Models:   map[string]UsageModel{"m": {Calls: 1}},
			Sessions: []json.RawMessage{json.RawMessage(`{}`)},
		}},
		OverrunStates: map[string]OverrunState{
			"gt-a": {},
			"gt-b": {StopBeganAt: "2026-10-14T00:00:00Z"},
		},
		OMCommits: map[string][]OMCommit{"gastown": {
			{At: "2026-09-01T00:00:00Z", Text: `{"threshold": 0.7}`},
		}},
		OutDir:   "out/metrics/2026-10-23",
		Today:    "2026-10-23",
		MkdirAll: files.mkdirAll,
		Create:   files.create,
	}, files
}

// extractRun extracts the env and fails the test on an error.
func extractRun(t *testing.T, env ExtractEnv) ExtractResult {
	t.Helper()
	res, err := Extract(context.Background(), env)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return res
}

// extractLandings reads back the landings.csv the extract wrote, keyed by bead
// and in file order.
func extractLandings(t *testing.T, files *extractFiles) ([]string, map[string]Row) {
	t.Helper()
	rows := files.rows(t, landingsName)
	order := make([]string, 0, len(rows))
	by := make(map[string]Row, len(rows))
	for _, row := range rows {
		order = append(order, row["bead"])
		by[row["bead"]] = row
	}
	return order, by
}

// extractLanding finds one landing row, failing the test when the bead is not
// among them.
func extractLanding(t *testing.T, rows []Row, bead string) Row {
	t.Helper()
	for _, row := range rows {
		if row["bead"] == bead {
			return row
		}
	}
	t.Fatalf("no %s landing in %v", bead, rows)
	return nil
}

// TestExtractWritesTheFourFilesWithOneRowPerLedgerLine checks the four tables
// are written, one landing row per ledger line, with the rework, rescue,
// overrun and shape flags the sources supply.
func TestExtractWritesTheFourFilesWithOneRowPerLedgerLine(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	extractRun(t, env)
	for _, name := range []string{landingsName, sidecarName, beadsName, weeklyName} {
		if !files.has(name) {
			t.Errorf("%s was not written; wrote %v", name, files.names)
		}
	}
	// weekly.csv carries the full header, and reading it back holds every row to
	// that header: ReadCSV refuses a row whose width differs from it.
	weekly, ok := files.data[weeklyName]
	if !ok {
		t.Fatalf("%s was not written; wrote %v", weeklyName, files.names)
	}
	if got, want := strings.SplitN(weekly.String(), "\r\n", 2)[0], strings.Join(WeeklyColumns, ","); got != want {
		t.Errorf("%s header = %q, want %q", weeklyName, got, want)
	}
	if rows := files.rows(t, weeklyName); len(rows) == 0 {
		t.Errorf("%s has no rows", weeklyName)
	}
	order, by := extractLandings(t, files)
	if !reflect.DeepEqual(order, []string{"gt-a", "gt-b"}) {
		t.Fatalf("landings.csv beads = %v, want gt-a then gt-b", order)
	}
	for _, tc := range []struct{ bead, column, want string }{
		{"gt-a", "rejected_before_landing", "1"}, // from the notes: no review lines here
		{"gt-b", "manual_rescue", "1"},
		{"gt-b", "reached_overrun_stop", "1"},
		{"gt-a", "reached_overrun_stop", "0"}, // tracked by the guard, never stopped
		{"gt-a", "shaped", "1"},
		// The bead and the usage file reach the landing's own columns.
		{"gt-a", "label_count", "1"},
		{"gt-a", "model", "m"},
		{"gt-a", "om_threshold", "0.7"},
		{"gt-b", "model", ""}, // no usage file: an empty cell, not a guess
	} {
		if got := by[tc.bead][tc.column]; got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.bead, tc.column, got, tc.want)
		}
	}
}

// TestExtractWiresEverySourceIntoTheWeeklyRow holds the town row the fixture's
// week makes to one figure from each source: the landings from the ledger, the
// attempts from the notes, the handoff size from bd show, the sessions from the
// usage file, the density from the beads, the threshold from the om history and
// the model from the usage. A source wired to the wrong builder shows up here
// as the wrong figure, not as a missing file.
func TestExtractWiresEverySourceIntoTheWeeklyRow(t *testing.T) {
	t.Parallel()
	env, _ := extractCase(t)
	res := extractRun(t, env)
	town, ok := extractWeek(res.Weekly, "ALL", "2026-10-12")
	if !ok {
		t.Fatalf("no ALL row for the week of 2026-10-12 in %v", res.Weekly)
	}
	for _, tc := range []struct {
		column string
		got    *float64
		want   float64
	}{
		{"size_median", town.SizeMedian, 10},
		{"size_p90", town.SizeP90, 10},
		{"file_entropy_mean", town.FileEntropyMean, 0},
		{"attempts_mean", town.AttemptsMean, 1.5},
		{"rejected_share", town.RejectedShare, 0.5},
		{"first_try_share", town.FirstTryShare, 0.5},
		{"h_labels", town.HLabels, 0},
		{"handoff_bytes_median", town.HandoffBytesMedian, 13},
		{"sessions_median", town.SessionsMedian, 1},
		{"rho", town.Rho, 2},
		{"dependency_density", town.DependencyDensity, 2},
		{"conflict_gate_share", town.ConflictGateShare, 1},
		{"unknown_share", town.UnknownShare, 0},
		{"om_threshold", town.OMThreshold, 0.7},
	} {
		if tc.got == nil {
			t.Errorf("%s = nil, want %v", tc.column, tc.want)
			continue
		}
		if math.Abs(*tc.got-tc.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", tc.column, *tc.got, tc.want)
		}
	}
	if town.Landings != 2 || town.HandoffBytesTotal != 13 || town.Rejections != 1 || town.ManualRescueCount != 1 {
		t.Errorf("landings, handoff_bytes_total, rejections, manual_rescue_count = %d, %d, %d, %d; want 2, 13, 1, 1",
			town.Landings, town.HandoffBytesTotal, town.Rejections, town.ManualRescueCount)
	}
	if town.DominantModel == nil || *town.DominantModel != "m" {
		t.Errorf("dominant_model = %v, want m", town.DominantModel)
	}
	if !town.Thin {
		t.Error("thin = false, want the fixture's two landings to be a thin week")
	}
}

// TestExtractWritesBeadsCSVWithHandoffBytesAndSessions checks beads.csv carries
// the rig's name for a bead's prefix, the handoff size the reader fetched under
// the default cap, and the sessions the bead's usage file recorded. A bead
// neither source covered is an empty cell, not a zero.
func TestExtractWritesBeadsCSVWithHandoffBytesAndSessions(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	extractRun(t, env)
	rows := files.rows(t, beadsName)
	by := make(map[string]Row, len(rows))
	for _, row := range rows {
		by[row["id"]] = row
	}
	for _, tc := range []struct{ bead, column, want string }{
		// The gt prefix's rig name, not the prefix.
		{"gt-a", "rig", "gastown"},
		{"gt-a", "labels", "handoff"},
		// "handoff notes", fetched under the env's default cap.
		{"gt-a", "handoff_bytes", "13"},
		{"gt-a", "sessions", "1"},
		{"gt-a", "dependency_count", "1"},
		// Not fetched, and no usage file: unknown reads as empty, not zero.
		{"gt-b", "handoff_bytes", ""},
		{"gt-b", "sessions", ""},
	} {
		if got := by[tc.bead][tc.column]; got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.bead, tc.column, got, tc.want)
		}
	}
}

// TestExtractWritesItsFilesInOrder checks the files go out in the extract's
// order, and that a bd listing that fails leaves beads.csv out rather than
// writing an empty one.
func TestExtractWritesItsFilesInOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		runBd func(*[]bdCall) BdRun
		want  []string
	}{
		{
			name:  "every source read",
			runBd: extractBd,
			want:  []string{landingsName, sidecarName, weeklyName, beadsName, footerName},
		},
		{
			name: "every bd list failed",
			runBd: func(*[]bdCall) BdRun {
				return func(string, []string) ([]byte, bool) { return nil, false }
			},
			want: []string{landingsName, sidecarName, weeklyName, footerName},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, files := extractCase(t)
			var calls []bdCall
			env.RunBd = tc.runBd(&calls)
			extractRun(t, env)

			if files.made != env.OutDir {
				t.Errorf("MkdirAll(%q), want the output directory %q", files.made, env.OutDir)
			}
			if !reflect.DeepEqual(files.names, tc.want) {
				t.Errorf("files written = %v, want %v", files.names, tc.want)
			}
		})
	}
}

// TestExtractDropsACorruptLedgerLineAndNamesIt checks a line the loader cannot
// parse is a logged drop, counted in the footer, and the extract still
// publishes the rows it could read.
func TestExtractDropsACorruptLedgerLineAndNamesIt(t *testing.T) {
	t.Parallel()
	env, _ := extractCase(t)
	env.LedgerLines = map[string][]string{"gastown": {
		extractLedgerLine(t, "gt-a", "gastown", "daemon", "2026-10-14T00:00:00Z"),
		"{broken",
	}}
	res := extractRun(t, env)
	if len(res.Landings) != 1 {
		t.Errorf("landings = %d, want 1", len(res.Landings))
	}
	if want := [][]string{{"ledger:gastown", "unparseable json"}}; !reflect.DeepEqual(res.Footer.RowsDropped, want) {
		t.Errorf("rows_dropped = %v, want %v", res.Footer.RowsDropped, want)
	}
	if got := res.Footer.RowsRead["ledger"]; got != 2 {
		t.Errorf("rows_read ledger = %d, want 2", got)
	}
}

// TestExtractTakesRejectionHistoryFromNotesWithoutExtraBdCalls checks the rework
// depth comes from the bead's notes, so the extract makes one list and one show
// (the handoff bead), never a show per rejected bead.
func TestExtractTakesRejectionHistoryFromNotesWithoutExtraBdCalls(t *testing.T) {
	t.Parallel()
	env, _ := extractCase(t)
	var calls []bdCall
	env.RunBd = extractBd(&calls)
	res := extractRun(t, env)
	a := extractLanding(t, res.Landings, "gt-a")
	if got := [2]string{a["rejected_before_landing"], a["attempts"]}; got != [2]string{"1", "2"} {
		t.Errorf("gt-a rejected_before_landing, attempts = %v, want [1 2]", got)
	}
	lists, shows := 0, 0
	for _, call := range calls {
		switch call.args[0] {
		case "list":
			lists++
		case "show":
			shows++
		}
	}
	if lists != 1 {
		t.Errorf("bd list calls = %d, want 1", lists)
	}
	if shows != 1 {
		t.Errorf("bd show calls = %d, want 1 (the handoff bead alone)", shows)
	}
}

// TestExtractRejectionKindReachesTheWeeklyTable checks a conflict rejection on
// the bead is the town's row's rejection, and its share of the rejections.
func TestExtractRejectionKindReachesTheWeeklyTable(t *testing.T) {
	t.Parallel()
	env, _ := extractCase(t)
	res := extractRun(t, env)
	town, ok := extractWeek(res.Weekly, "ALL", "2026-10-12")
	if !ok {
		t.Fatalf("no ALL row for the week of 2026-10-12 in %v", res.Weekly)
	}
	if town.Rejections != 1 {
		t.Errorf("rejections = %d, want 1", town.Rejections)
	}
	if town.ConflictGateShare == nil || *town.ConflictGateShare != 1.0 {
		t.Errorf("conflict_gate_share = %v, want 1", town.ConflictGateShare)
	}
}

// extractWeek finds one weekly row.
func extractWeek(rows []WeeklyMetrics, rig, week string) (WeeklyMetrics, bool) {
	for _, row := range rows {
		if row.Rig == rig && row.Week == week {
			return row, true
		}
	}
	return WeeklyMetrics{}, false
}

// TestExtractWritesBeadsCSVForAnEmptyButSuccessfulList checks an empty listing
// is the town's real answer, not a failed read, so beads.csv is written and no
// source is named unreadable.
func TestExtractWritesBeadsCSVForAnEmptyButSuccessfulList(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	env.RunBd = func(string, []string) ([]byte, bool) { return []byte("[]"), true }
	res := extractRun(t, env)
	if !files.has(beadsName) {
		t.Errorf("%s was not written; wrote %v", beadsName, files.names)
	}
	if len(res.Footer.SourcesUnreadable) != 0 {
		t.Errorf("sources_unreadable = %v, want none", res.Footer.SourcesUnreadable)
	}
}

// TestExtractUnreadableDiffsAreGapsNotDroppedRows checks a diff that cannot be
// read costs the landing its size cells, which the footer names as a gap, and
// never the row itself.
func TestExtractUnreadableDiffsAreGapsNotDroppedRows(t *testing.T) {
	t.Parallel()
	env, _ := extractCase(t)
	env.Diffs = extractNoDiffs{}
	res := extractRun(t, env)
	if len(res.Landings) != 2 {
		t.Errorf("landings = %d, want 2", len(res.Landings))
	}
	if len(res.Footer.RowsDropped) != 0 {
		t.Errorf("rows_dropped = %v, want none", res.Footer.RowsDropped)
	}
	if len(res.Footer.Gaps) != 2 {
		t.Errorf("gaps = %d, want 2", len(res.Footer.Gaps))
	}
}

// TestExtractRefusesAReviewsLoaderThatLosesARow checks a loader that returns a
// row fewer than its lines account for trips the row-count assertion rather
// than publishing a short table.
func TestExtractRefusesAReviewsLoaderThatLosesARow(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	env.ReviewLines = []string{
		`{"bead": "gt-a", "rig": "gastown", "attempt": 1, "score": 0.5, "verdict": "request_changes", "at": "2026-10-13T00:00:00Z"}`,
		`{"bead": "gt-b", "rig": "gastown", "attempt": 1, "score": 0.5, "verdict": "approve", "at": "2026-10-14T00:00:00Z"}`,
	}
	env.ReviewLoader = func(lines []string, dropped *Dropped) []Review {
		rows := LoadReviews(lines, dropped)
		return rows[:len(rows)-1]
	}
	_, err := Extract(context.Background(), env)
	var short *RowCountError
	if !errors.As(err, &short) {
		t.Fatalf("Extract error = %v, want a RowCountError", err)
	}
	if short.Source != "reviews" {
		t.Errorf("RowCountError source = %q, want reviews", short.Source)
	}
	assertNoFilesWritten(t, files)
}

// assertNoFilesWritten checks a refused extract opened no output file, so
// nothing reads a partial output directory as a finished extract.
func assertNoFilesWritten(t *testing.T, files *extractFiles) {
	t.Helper()
	for _, name := range files.names {
		t.Errorf("%s was written before the refusal", name)
	}
}

// TestExtractRefusesALedgerLoaderThatLosesARowSilently checks the row count is
// the only thing that can notice a row went missing when no drop is logged, and
// that it names both the counts it compared.
func TestExtractRefusesALedgerLoaderThatLosesARowSilently(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	env.LedgerLoader = func(sources map[string][]string, dropped *Dropped) []LedgerRow {
		rows := LoadLedger(sources, dropped)
		return rows[:len(rows)-1]
	}
	_, err := Extract(context.Background(), env)
	var short *RowCountError
	if !errors.As(err, &short) {
		t.Fatalf("Extract error = %v, want a RowCountError", err)
	}
	if want := "1 rows + 0 logged drops != 2 source lines"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	if short.Rows != 1 || short.Drops != 0 || short.Lines != 2 {
		t.Errorf("RowCountError = %d rows, %d drops, %d lines; want 1, 0, 2", short.Rows, short.Drops, short.Lines)
	}
	assertNoFilesWritten(t, files)
}

// TestExtractFailingBdLeavesNoBeadsCSVAndNamesTheSource checks every listing
// failing leaves beads.csv out, names the source unreadable, records the
// failure as a gap rather than a dropped row, and leaves the label and
// dependency figures unknown rather than zero.
func TestExtractFailingBdLeavesNoBeadsCSVAndNamesTheSource(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	env.RunBd = func(string, []string) ([]byte, bool) { return nil, false }
	res := extractRun(t, env)
	if files.has(beadsName) {
		t.Errorf("%s was written, want it left out", beadsName)
	}
	if !files.has(weeklyName) {
		t.Errorf("%s was not written; wrote %v", weeklyName, files.names)
	}
	if want := []string{"bd:/g"}; !reflect.DeepEqual(res.Footer.SourcesUnreadable, want) {
		t.Errorf("sources_unreadable = %v, want %v", res.Footer.SourcesUnreadable, want)
	}
	if len(res.Footer.RowsDropped) != 0 {
		t.Errorf("rows_dropped = %v, want a failed listing recorded as a gap", res.Footer.RowsDropped)
	}
	if want := [][]string{{"bd:/g", "bd list failed in /g"}}; !reflect.DeepEqual(res.Footer.Gaps, want) {
		t.Errorf("gaps = %v, want %v", res.Footer.Gaps, want)
	}
	town, ok := extractWeek(res.Weekly, "ALL", "2026-10-12")
	if !ok {
		t.Fatalf("no ALL row in %v", res.Weekly)
	}
	if town.HLabels != nil {
		t.Errorf("h_labels = %v, want nil: unknown is not zero", town.HLabels)
	}
	if town.DependencyDensity != nil {
		t.Errorf("dependency_density = %v, want nil", town.DependencyDensity)
	}
}

// TestExtractWritesFooterJSONInFieldOrder checks footer.json holds the four
// counts in field order, with empty lists where no row was dropped, and no
// trailing newline.
func TestExtractWritesFooterJSONInFieldOrder(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	extractRun(t, env)
	buf, ok := files.data[footerName]
	if !ok {
		t.Fatalf("%s was not written; wrote %v", footerName, files.names)
	}
	want := `{"rows_read":{"ledger":2,"reviews":0,"beads":2,"usage":1},"rows_dropped":[],"gaps":[],"sources_unreadable":[]}`
	if got := buf.String(); got != want {
		t.Errorf("%s = %s, want %s", footerName, got, want)
	}
}

// TestExtractNamesTheFileAWriteFailsOn checks an output file that cannot be
// opened, written or closed is the extract's error, named by the file it was
// writing, so a partial output directory is never read as a finished extract.
func TestExtractNamesTheFileAWriteFailsOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		create func(name string) (io.WriteCloser, error)
		want   string
	}{
		{
			name: "landings cannot be opened",
			create: func(string) (io.WriteCloser, error) {
				return nil, os.ErrPermission
			},
			want: landingsName,
		},
		{
			name: "the sidecar cannot be opened",
			create: func(string) (io.WriteCloser, error) {
				return nil, os.ErrPermission
			},
			want: sidecarName,
		},
		{
			name: "weekly cannot be written",
			create: func(string) (io.WriteCloser, error) {
				return failingWriteCloser{writeErr: os.ErrClosed}, nil
			},
			want: weeklyName,
		},
		{
			name: "beads cannot be opened",
			create: func(string) (io.WriteCloser, error) {
				return nil, os.ErrPermission
			},
			want: beadsName,
		},
		{
			name: "the footer cannot be written",
			create: func(string) (io.WriteCloser, error) {
				return failingWriteCloser{writeErr: os.ErrClosed}, nil
			},
			want: footerName,
		},
		{
			name: "the footer cannot be closed",
			create: func(string) (io.WriteCloser, error) {
				return failingWriteCloser{closeErr: os.ErrClosed}, nil
			},
			want: footerName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, _ := extractCase(t)
			failing := tc.create
			env.Create = func(name string) (io.WriteCloser, error) {
				if name == tc.want {
					return failing(name)
				}
				return nopWriteCloser{io.Discard}, nil
			}
			_, err := Extract(context.Background(), env)
			if err == nil {
				t.Fatal("Extract succeeded, want the failed write to be an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// failingWriteCloser stands in for an output file that will not take a write or
// a close, so a test can drive the extract's error from either.
type failingWriteCloser struct{ writeErr, closeErr error }

func (f failingWriteCloser) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f failingWriteCloser) Close() error { return f.closeErr }

// failingDiffs is a diff source whose read fails for a reason other than the
// diff being absent, which the extract reports rather than counting the landing
// unreadable.
type failingDiffs struct{}

func (failingDiffs) Stats(context.Context, string, string) (DiffStat, error) {
	return DiffStat{}, errors.New("git failed")
}

// TestExtractReturnsTheFailureThatStoppedIt checks a source that fails in a way
// the extract cannot record as a gap stops the extract with that error, rather
// than a table written as if the source had said nothing.
func TestExtractReturnsTheFailureThatStoppedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*ExtractEnv)
		want   string
	}{
		{
			name: "the output directory cannot be made",
			mutate: func(env *ExtractEnv) {
				env.MkdirAll = func(string) error { return os.ErrPermission }
			},
			want: os.ErrPermission.Error(),
		},
		{
			name: "a review's timestamp does not parse",
			mutate: func(env *ExtractEnv) {
				env.ReviewLines = []string{
					`{"bead": "gt-a", "rig": "gastown", "attempt": 1, "score": 0.5, "verdict": "request_changes", "at": "not a time"}`,
				}
			},
			want: "invalid ISO timestamp",
		},
		{
			name: "an om threshold is not a number",
			mutate: func(env *ExtractEnv) {
				env.OMCommits = map[string][]OMCommit{"gastown": {
					{At: "2026-09-01T00:00:00Z", Text: `{"threshold": {"n": 1}}`},
				}}
			},
			want: "not a number",
		},
		{
			name:   "a diff fails for a reason other than its absence",
			mutate: func(env *ExtractEnv) { env.Diffs = failingDiffs{} },
			want:   "git failed",
		},
		{
			name: "a bead's created_at does not parse",
			mutate: func(env *ExtractEnv) {
				env.RunBd = func(_ string, args []string) ([]byte, bool) {
					if len(args) > 0 && args[0] == "list" {
						return []byte(`[{"id": "gt-c", "created_at": "not a time", "issue_type": "task"}]`), true
					}
					return []byte(extractBdShow), true
				}
			},
			want: `bead "gt-c" created_at`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, _ := extractCase(t)
			tc.mutate(&env)
			_, err := Extract(context.Background(), env)
			if err == nil {
				t.Fatal("Extract succeeded, want the source's failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestWriteJSONNamesTheFileItCannotWrite checks a value the encoder refuses and
// a writer that refuses the bytes are both named by the file, so a partial
// footer is never read as a finished extract.
func TestWriteJSONNamesTheFileItCannotWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		w    io.Writer
		v    any
		want string
	}{
		{"a value the encoder refuses", io.Discard, make(chan int), "unsupported type"},
		{"a writer that refuses the bytes", failingWriteCloser{writeErr: os.ErrClosed}, map[string]int{}, os.ErrClosed.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := writeJSON(tc.w, footerName, tc.v)
			if err == nil {
				t.Fatal("writeJSON succeeded, want a refusal")
			}
			for _, want := range []string{footerName, tc.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestExtractRefusesAnEnvWithNoOutputOrDiffSource checks a seam the extract
// cannot work without is refused with an error naming it, never dereferenced.
func TestExtractRefusesAnEnvWithNoOutputOrDiffSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*ExtractEnv)
		want   string
	}{
		{"no output directory", func(env *ExtractEnv) { env.OutDir = "" }, "OutDir"},
		{"no diff source", func(env *ExtractEnv) { env.Diffs = nil }, "Diffs"},
		{"no bd runner", func(env *ExtractEnv) { env.RunBd = nil }, "RunBd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, _ := extractCase(t)
			tc.mutate(&env)
			_, err := Extract(context.Background(), env)
			if err == nil {
				t.Fatal("Extract succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestExtractRefusesAWeeklyColumnNoCellFills checks a header column the weekly
// metrics cannot fill stops the extract with an error naming the column, before
// it writes a file, rather than publishing an empty cell the report would read
// as a measurement.
func TestExtractRefusesAWeeklyColumnNoCellFills(t *testing.T) {
	t.Parallel()
	env, files := extractCase(t)
	env.WeeklyColumns = []string{"week", "no_such_column"}
	_, err := Extract(context.Background(), env)
	if err == nil {
		t.Fatal("Extract succeeded, want the header's unknown column refused")
	}
	if !strings.Contains(err.Error(), "no_such_column") {
		t.Errorf("error = %q, want it to name the column", err)
	}
	assertNoFilesWritten(t, files)
}
