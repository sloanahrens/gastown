package metrics

import (
	"bytes"
	"os"
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
