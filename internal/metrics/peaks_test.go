package metrics

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
)

// peaksPath is the plain peak file under the town peaksEnv names.
const peaksPath = "/town/.runtime/metrics/resources/bead-peaks.jsonl"

// peaksEnv is an Env whose only seam is ReadFile, over files keyed by their
// full path. A path not in files answers fs.ErrNotExist, as os.ReadFile does
// for a missing file.
func peaksEnv(files map[string][]byte) Env {
	return Env{
		Town: "/town",
		ReadFile: func(path string) ([]byte, error) {
			data, ok := files[path]
			if !ok {
				return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
			}
			return data, nil
		},
	}
}

// peaksFixture is testdata/bead-peaks.v1.jsonl: three rows the sampler wrote
// for gt-tj446, ids and values kept.
func peaksFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/bead-peaks.v1.jsonl")
	if err != nil {
		t.Fatalf("read the peaks fixture: %v", err)
	}
	return data
}

// gzipped is data compressed the way the sampler's rotation writes a sibling.
func gzipped(t *testing.T, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(data)); err != nil {
		t.Fatalf("gzip the peaks: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close the gzip writer: %v", err)
	}
	return buf.Bytes()
}

// beadLanding is a landing of bead, the only cell a peak join reads.
func beadLanding(bead string) Row {
	row := landing("2026-10-24T18:00:00Z")
	row["bead"] = bead
	return row
}

// peaksJoin reads the peaks env names and joins them onto one landing of bead.
func peaksJoin(t *testing.T, env Env, bead string) (Row, []string) {
	t.Helper()
	peaks, err := ReadPeaks(env)
	if err != nil {
		t.Fatalf("ReadPeaks: %v", err)
	}
	header, joined := JoinPeaks(landingColumns, []Row{beadLanding(bead)}, peaks)
	return joined[0], header
}

func TestJoinPeaksFillsTheBeadsLatestRowAndEmptiesTheRest(t *testing.T) {
	t.Parallel()
	latest := map[string]string{
		"sandbox_peak_mem_bytes":  "2877583360",
		"sandbox_peak_cpu_pct":    "170.6",
		"sandbox_mem_limit_bytes": "8589934592",
	}
	empty := map[string]string{
		"sandbox_peak_mem_bytes":  "",
		"sandbox_peak_cpu_pct":    "",
		"sandbox_mem_limit_bytes": "",
	}
	tests := []struct {
		name string
		bead string
		want map[string]string
	}{
		{"a landing whose bead has rows carries the latest row's three values", "gt-tj446", latest},
		{"a landing whose bead has no row keeps empty cells", "dv-9f8b", empty},
	}
	env := peaksEnv(map[string][]byte{peaksPath: peaksFixture(t)})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row, header := peaksJoin(t, env, tc.bead)
			for column, want := range tc.want {
				if got := row[column]; got != want {
					t.Errorf("%s = %q, want %q", column, got, want)
				}
			}
			tail := header[len(landingColumns):]
			if want := []string{"sandbox_peak_mem_bytes", "sandbox_peak_cpu_pct", "sandbox_mem_limit_bytes"}; !slices.Equal(tail, want) {
				t.Errorf("appended columns = %v, want %v", tail, want)
			}
		})
	}
}

func TestJoinPeaksWithNoPeaksFileLeavesTheJoinOutputByteForByte(t *testing.T) {
	t.Parallel()
	snaps := []Record{atQueue("2026-10-24T12:00:00Z", 4)}
	rows := []Row{landing("2026-10-24T12:30:00Z"), beadLanding("gt-tj446")}
	header, joined, _ := Join(landingColumns, rows, snaps, joinMaxAge)
	var want bytes.Buffer
	if err := WriteLandings(&want, header, joined); err != nil {
		t.Fatalf("write the join's output: %v", err)
	}

	peaks, err := ReadPeaks(peaksEnv(nil))
	if err != nil {
		t.Fatalf("ReadPeaks with no file: %v", err)
	}
	gotHeader, gotRows := JoinPeaks(header, joined, peaks)
	var got bytes.Buffer
	if err := WriteLandings(&got, gotHeader, gotRows); err != nil {
		t.Fatalf("write the peak join's output: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Errorf("output changed with no peaks file:\n got %q\nwant %q", got.String(), want.String())
	}
}

func TestReadPeaksReadsTheGzipSibling(t *testing.T) {
	t.Parallel()
	env := peaksEnv(map[string][]byte{
		peaksPath + ".gz": gzipped(t, `{"v":1,"bead":"dv-9f8b","ended_at":"2026-10-24T17:33:15Z","peak_mem_bytes":5243572224,"peak_cpu_pct":272.57,"mem_limit_bytes":8589934592}`+"\n"),
	})
	row, _ := peaksJoin(t, env, "dv-9f8b")
	if got := row["sandbox_peak_mem_bytes"]; got != "5243572224" {
		t.Errorf("sandbox_peak_mem_bytes = %q, want the gzip row's 5243572224", got)
	}
	if got := row["sandbox_peak_cpu_pct"]; got != "272.57" {
		t.Errorf("sandbox_peak_cpu_pct = %q, want the gzip row's 272.57", got)
	}
}

func TestLatestPeaksPickTheRowEndedLast(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rows []string
		want string
	}{
		{
			"the row that ended last wins even when it is read first",
			[]string{
				`{"v":1,"bead":"gt-q1","ended_at":"2026-10-24T18:00:00Z","peak_mem_bytes":999}`,
				`{"v":1,"bead":"gt-q1","ended_at":"2026-10-24T17:00:00Z","peak_mem_bytes":111}`,
			},
			"999",
		},
		{
			"a tie goes to the row read later",
			[]string{
				`{"v":1,"bead":"gt-q1","ended_at":"2026-10-24T18:00:00Z","peak_mem_bytes":5}`,
				`{"v":1,"bead":"gt-q1","ended_at":"2026-10-24T18:00:00Z","peak_mem_bytes":6}`,
			},
			"6",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := peaksEnv(map[string][]byte{peaksPath: []byte(strings.Join(tc.rows, "\n") + "\n")})
			row, _ := peaksJoin(t, env, "gt-q1")
			if got := row["sandbox_peak_mem_bytes"]; got != tc.want {
				t.Errorf("sandbox_peak_mem_bytes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadPeaksSkipsWhatItCannotAttributeAndKeepsWhatItCan(t *testing.T) {
	t.Parallel()
	data := strings.Join([]string{
		// A number beyond float64's exact range is kept as written; a null
		// value is an empty cell; a key the schema does not name is ignored.
		`{"v":1,"bead":"gt-q1","ended_at":"2026-10-24T18:00:00Z","peak_mem_bytes":9007199254740993,"peak_cpu_pct":null,"mem_limit_bytes":1,"added_later":{"x":1}}`,
		`{"v":1,"bead":"gt-q2","ended_at":"2026-10-24T18:00:00Z","peak_mem_bytes":5`,
		`{"v":1,"bead":null,"ended_at":"2026-10-24T18:00:00Z","peak_mem_bytes":7}`,
		``,
		`not json`,
	}, "\n")
	env := peaksEnv(map[string][]byte{peaksPath: []byte(data)})

	row, header := peaksJoin(t, env, "gt-q1")
	if got := row["sandbox_peak_mem_bytes"]; got != "9007199254740993" {
		t.Errorf("sandbox_peak_mem_bytes = %q, want the number kept as written", got)
	}
	if got := row["sandbox_peak_cpu_pct"]; got != "" {
		t.Errorf("sandbox_peak_cpu_pct = %q, want an empty cell for null", got)
	}
	if got := row["sandbox_mem_limit_bytes"]; got != "1" {
		t.Errorf("sandbox_mem_limit_bytes = %q, want 1", got)
	}
	if !slices.Contains(header, "sandbox_peak_mem_bytes") {
		t.Errorf("header = %v, want the peak columns", header)
	}

	gone, _ := peaksJoin(t, env, "gt-q2")
	if got := gone["sandbox_peak_mem_bytes"]; got != "" {
		t.Errorf("a partial last line gave gt-q2 %q, want no row", got)
	}
}

func TestReadPeaksFailsOnAFileItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  Env
		want string
	}{
		{
			"a read that is not a missing file",
			Env{Town: "/town", ReadFile: func(string) ([]byte, error) { return nil, errors.New("permission denied") }},
			"bead-peaks.jsonl.gz: permission denied",
		},
		{
			"a gzip sibling that is not gzip",
			peaksEnv(map[string][]byte{peaksPath + ".gz": []byte("not gzip")}),
			"bead-peaks.jsonl.gz:",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadPeaks(tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ReadPeaks error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
