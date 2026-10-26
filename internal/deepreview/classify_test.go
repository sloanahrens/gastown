package deepreview

import (
	"reflect"
	"strings"
	"testing"
)

// ts builds a Timestamp from the ledger's spelling for a table row; a row that
// names a timestamp the parser refuses is a broken row.
func ts(s string) Timestamp {
	t, err := ParseTimestamp(s)
	if err != nil {
		panic(err)
	}
	return t
}

// recordDiff reports how got differs from want, empty when they match. The
// fields are compared one by one so a diff names the field at fault.
func recordDiff(got, want Record) string {
	if got.Bead != want.Bead {
		return "bead " + got.Bead + ", want " + want.Bead
	}
	if got.Head != want.Head {
		return "head " + got.Head + ", want " + want.Head
	}
	if got.LandedCommit != want.LandedCommit {
		return "landed_commit " + got.LandedCommit + ", want " + want.LandedCommit
	}
	if !got.LandedAt.Time.Equal(want.LandedAt.Time) || got.LandedAt.Text != want.LandedAt.Text {
		return "landed_at " + got.LandedAt.String() + ", want " + want.LandedAt.String()
	}
	if !reflect.DeepEqual(got.RiskPaths, want.RiskPaths) {
		return "risk_paths " + strings.Join(got.RiskPaths, ",") + ", want " + strings.Join(want.RiskPaths, ",")
	}
	return ""
}

// TestClassifyLine has a row per rule classify_landings applies: the blank
// skip, the bad line, and each drift condition.
func TestClassifyLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		line   string
		blank  bool
		kind   Kind
		record Record
	}{
		{name: "an empty line is skipped", line: "", blank: true},
		{name: "a whitespace line is skipped", line: " \t \r", blank: true},
		{name: "a line that is not JSON is bad", line: `{"bead":"gt-1"`, kind: KindBad},
		{name: "trailing garbage after a JSON object is bad", line: `{"landed_at":"2026-10-10T06:00:00Z"} x`, kind: KindBad},
		{name: "a JSON null is bad, as a failed parse is", line: `null`, kind: KindBad},
		{name: "an array is drift", line: `[1,2]`, kind: KindDrift},
		{name: "a string is drift", line: `"x"`, kind: KindDrift},
		{name: "a number is drift", line: `42`, kind: KindDrift},
		{name: "a missing landed_at is drift", line: `{"bead":"gt-1"}`, kind: KindDrift},
		{name: "a null landed_at is drift", line: `{"landed_at":null}`, kind: KindDrift},
		{name: "a landed_at that is not a time is drift", line: `{"landed_at":"yesterday"}`, kind: KindDrift},
		{name: "a landed_at that is not a string is drift", line: `{"landed_at":5}`, kind: KindDrift},
		{name: "a risk_paths that is not a list is drift", line: `{"landed_at":"2026-10-10T06:00:00Z","risk_paths":"x"}`, kind: KindDrift},
		{name: "a risk_paths holding a non-string is drift", line: `{"landed_at":"2026-10-10T06:00:00Z","risk_paths":["x",1]}`, kind: KindDrift},
		{name: "a bead that is not a string is drift", line: `{"landed_at":"2026-10-10T06:00:00Z","bead":7}`, kind: KindDrift},
		{name: "a head that is not a string is drift", line: `{"landed_at":"2026-10-10T06:00:00Z","head":[]}`, kind: KindDrift},
		{name: "a landed_commit that is not a string is drift", line: `{"landed_at":"2026-10-10T06:00:00Z","landed_commit":{}}`, kind: KindDrift},
		{
			name:   "a null risk_paths is no path",
			line:   `{"landed_at":"2026-10-10T06:00:00Z","risk_paths":null}`,
			kind:   KindOK,
			record: Record{LandedAt: ts("2026-10-10T06:00:00Z"), RiskPaths: []string{}},
		},
		{
			name:   "a record with no risk_paths field is no path",
			line:   `{"landed_at":"2026-10-10T06:00:00Z"}`,
			kind:   KindOK,
			record: Record{LandedAt: ts("2026-10-10T06:00:00Z"), RiskPaths: []string{}},
		},
		{
			name: "a full record is carried",
			line: `{"bead":"gt-3","rig":"gastown","head":"h3","landed_commit":"c3",` +
				`"landed_at":"2026-10-10T03:00:00.123456Z","risk_paths":["internal/land/y.go","internal/daemon/x.go"]}`,
			kind: KindOK,
			record: Record{
				Bead:         "gt-3",
				Head:         "h3",
				LandedCommit: "c3",
				LandedAt:     ts("2026-10-10T03:00:00.123456Z"),
				RiskPaths:    []string{"internal/land/y.go", "internal/daemon/x.go"},
			},
		},
	} {
		got, blank := classifyLine(1, tc.line)
		if blank != tc.blank {
			t.Errorf("%s: classifyLine(%q) blank = %v, want %v", tc.name, tc.line, blank, tc.blank)
			continue
		}
		if tc.blank {
			continue
		}
		if got.Kind != tc.kind {
			t.Errorf("%s: classifyLine(%q) kind = %v, want %v", tc.name, tc.line, got.Kind, tc.kind)
			continue
		}
		if diff := recordDiff(got.Record, tc.record); diff != "" {
			t.Errorf("%s: classifyLine(%q) record differs: %s", tc.name, tc.line, diff)
		}
		if got.Kind != KindOK && !reflect.DeepEqual(got.Record, Record{}) {
			t.Errorf("%s: classifyLine(%q) carries a record on a %v line", tc.name, tc.line, got.Kind)
		}
	}
}

// TestClassifyLandings covers the file-level rules: the counts, the torn last
// line, and the three refusals.
func TestClassifyLandings(t *testing.T) {
	t.Parallel()
	const good = `{"bead":"gt-1","head":"h1","landed_commit":"c1","landed_at":"2026-10-10T01:00:00.5Z","risk_paths":["internal/daemon/x.go"]}` + "\n"
	for _, tc := range []struct {
		name     string
		data     string
		recs     int
		unparsed int
		total    int
		wantErr  string
	}{
		{
			name: "a healthy file with a trailing newline",
			data: good + `{"bead":"gt-2","head":"h2","landed_commit":"c2","landed_at":"2026-10-10T02:00:00Z"}` + "\n",
			recs: 2, total: 2,
		},
		{
			name: "a file with no trailing newline",
			data: good[:len(good)-1],
			recs: 1, total: 1,
		},
		{
			name: "a torn last line is tolerated",
			data: good + "{torn\n",
			recs: 1, unparsed: 1, total: 2,
		},
		{
			name: "a torn last line without a newline is tolerated",
			data: good + "{torn",
			recs: 1, unparsed: 1, total: 2,
		},
		{
			name: "a JSON null as the last line is a torn line, not drift",
			data: good + "null\n",
			recs: 1, unparsed: 1, total: 2,
		},
		{
			name: "blank lines are counted but not classified",
			data: "\n" + good + "  \n\t\n",
			recs: 1, total: 4,
		},
		{
			name:    "a line that is not JSON before the last refuses the file",
			data:    "garbage\n" + good,
			wantErr: "1 line(s) before the last are not JSON",
		},
		{
			name:    "a JSON null before the last refuses the file",
			data:    "null\n" + good,
			wantErr: "1 line(s) before the last are not JSON",
		},
		{
			name:    "a drifted line refuses the file",
			data:    good + `{"bead":"gt-9","landed_at":"yesterday","risk_paths":["x"]}` + "\n",
			wantErr: "the record shape may have changed",
		},
		{
			name:    "an empty file holds no record",
			data:    "",
			wantErr: "no readable landing record",
		},
		{
			name:    "blank lines alone hold no record",
			data:    "\n \n\t\n",
			wantErr: "no readable landing record",
		},
	} {
		got, err := ClassifyLandings([]byte(tc.data))
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: ClassifyLandings error = %v, want one naming %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: ClassifyLandings error: %v", tc.name, err)
			continue
		}
		if got.Records != tc.recs || got.Unparsed != tc.unparsed || got.Total != tc.total {
			t.Errorf("%s: counts = %d records, %d unparsed, total %d; want %d, %d, %d",
				tc.name, got.Records, got.Unparsed, got.Total, tc.recs, tc.unparsed, tc.total)
		}
	}
}

func TestClassifyLandingsCarriesFileOrder(t *testing.T) {
	t.Parallel()
	data := `{"bead":"b","landed_at":"2026-10-10T02:00:00Z"}` + "\n" +
		"\n" +
		`{"bead":"a","landed_at":"2026-10-10T01:00:00Z"}` + "\n" +
		`{torn` + "\n"
	got, err := ClassifyLandings([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(got.Lines))
	}
	for i, want := range []struct {
		number int
		kind   Kind
	}{{1, KindOK}, {3, KindOK}, {4, KindBad}} {
		if got.Lines[i].Number != want.number || got.Lines[i].Kind != want.kind {
			t.Errorf("line %d = number %d kind %v, want number %d kind %v",
				i, got.Lines[i].Number, got.Lines[i].Kind, want.number, want.kind)
		}
	}
}
