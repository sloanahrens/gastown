package metrics

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// landingsFixture is testdata/landings.v1.csv, bytes included: its CRLF line
// endings are the extract's and the round trip is measured against them.
func landingsFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/landings.v1.csv")
	if err != nil {
		t.Fatalf("read the landings fixture: %v", err)
	}
	return data
}

func TestReadLandingsReadsTheExtractHeader(t *testing.T) {
	t.Parallel()
	header, rows, err := ReadLandings(bytes.NewReader(landingsFixture(t)))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	if strings.Join(header, ",") != strings.Join(landingColumns, ",") {
		t.Errorf("header = %v, want the extract's columns %v", header, landingColumns)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the fixture's 3", len(rows))
	}
	if got := rows[0]["bead"]; got != "gt-r2d2a" {
		t.Errorf("rows[0][bead] = %q, want gt-r2d2a", got)
	}
	if got := rows[1]["lines_added"]; got != "" {
		t.Errorf("rows[1][lines_added] = %q, want an empty cell for a dropped diff", got)
	}
	if got := rows[2]["rig"]; got != "beads" {
		t.Errorf("rows[2][rig] = %q, want beads", got)
	}
}

// pythonLandingColumns reads LANDING_COLUMNS out of the Python extract, the
// other half of the contract landingColumns mirrors.
func pythonLandingColumns(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "town_metrics", "tables.py"))
	if err != nil {
		t.Fatalf("read the Python extract's tables.py: %v", err)
	}
	list := regexp.MustCompile(`(?s)LANDING_COLUMNS\s*=\s*\[(.*?)\]`).FindSubmatch(data)
	if list == nil {
		t.Fatal("tables.py has no LANDING_COLUMNS = [...] literal")
	}
	var columns []string
	for _, quoted := range regexp.MustCompile(`"([^"]*)"`).FindAllSubmatch(list[1], -1) {
		columns = append(columns, string(quoted[1]))
	}
	return columns
}

func TestLandingColumnsMatchThePythonExtract(t *testing.T) {
	t.Parallel()
	want := pythonLandingColumns(t)
	if len(want) == 0 {
		t.Fatal("read no columns out of tables.py")
	}
	if !slices.Equal(landingColumns, want) {
		t.Errorf("landingColumns = %v, want the extract's %v", landingColumns, want)
	}
}

func TestReadLandingsRejectsInputItCannotAttribute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"a row narrower than the header", "a,b\r\n1\r\n", "row 2"},
		{"a row wider than the header", "a,b\n1,2,3\n", "row 2"},
		{"no header at all", "", "empty file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, rows, err := ReadLandings(strings.NewReader(tc.in))
			if err == nil {
				t.Fatalf("read %q succeeded with %d rows; want an error", tc.in, len(rows))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %s", err, tc.want)
			}
		})
	}
}

func TestWriteLandingsWritesTheExtractFormat(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := []Row{{"a": "1", "b": ""}, {"a": "x,y", "b": "2"}}
	if err := WriteLandings(&buf, []string{"a", "b"}, rows); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := "a,b\r\n1,\r\n\"x,y\",2\r\n"
	if got := buf.String(); got != want {
		t.Errorf("bytes\n got: %q\nwant: %q", got, want)
	}
}

func TestWriteLandingsKeepsLineEndingsInACell(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, cell, want string }{
		{"a bare carriage return", "a\rb", "x\r\n\"a\rb\"\r\n"},
		{"a bare line feed", "a\nb", "x\r\n\"a\nb\"\r\n"},
		{"a CRLF pair", "a\r\nb", "x\r\n\"a\r\nb\"\r\n"},
		{"a quote beside a comma", `a",b`, "x\r\n\"a\"\",b\"\r\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := WriteLandings(&buf, []string{"x"}, []Row{{"x": tc.cell}}); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("bytes\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestReadLandingsKeepsACarriageReturnAndFoldsACRLFPair(t *testing.T) {
	t.Parallel()
	input := "x\r\n\"a\rb\"\r\n\"a\nb\"\r\n\"a\r\nb\"\r\n"
	_, rows, err := ReadLandings(strings.NewReader(input))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []string{"a\rb", "a\nb", "a\nb"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for i, want := range want {
		if got := rows[i]["x"]; got != want {
			t.Errorf("rows[%d][x] = %q, want %q", i, got, want)
		}
	}
}

func TestLandingsRoundTripIsByteForByte(t *testing.T) {
	t.Parallel()
	want := landingsFixture(t)
	header, rows, err := ReadLandings(bytes.NewReader(want))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteLandings(&buf, header, rows); err != nil {
		t.Fatalf("write the fixture back: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("round trip bytes\n got: %q\nwant: %q", buf.String(), want)
	}
}
