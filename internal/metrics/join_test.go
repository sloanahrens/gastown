package metrics

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"
)

// joinMaxAge is the window every case uses unless it is testing the window.
const joinMaxAge = 90 * time.Minute

// landing is one row of landings.csv with only landed_at set: the other cells
// are the extract's to fill and a join test does not read them.
func landing(at string) Row {
	row := make(Row, len(landingColumns))
	for _, name := range landingColumns {
		row[name] = ""
	}
	row["landed_at"] = at
	return row
}

// atQueue is a snapshot at ts whose only group reports a queue depth.
func atQueue(ts string, open float64) Record {
	return Record{V: Version, TS: ts, Groups: map[string]map[string]any{"queue": {"open": open}}}
}

func TestJoinPicksTheLatestEarlierSnapshotWithinMaxAge(t *testing.T) {
	t.Parallel()
	snaps := []Record{atQueue("2026-10-24T12:00:00Z", 1), atQueue("2026-10-24T12:30:00Z", 2), atQueue("2026-10-24T13:00:00Z", 3)}
	tests := []struct {
		name    string
		at      string
		snaps   []Record
		maxAge  time.Duration
		want    string
		wantAge string
	}{
		{"the latest earlier snapshot wins", "2026-10-24T12:45:00Z", snaps, joinMaxAge, "2", "15"},
		{"a snapshot at the landing's own ts is not after it", "2026-10-24T13:00:00Z", snaps, joinMaxAge, "3", "0"},
		{"a snapshot exactly at the landing's ts and no other", "2026-10-24T12:00:00Z", snaps, joinMaxAge, "1", "0"},
		{"a landing before every snapshot borrows none", "2026-10-24T11:59:00Z", snaps, joinMaxAge, "", ""},
		{"a snapshot exactly maxAge old is in force", "2026-10-24T14:30:00Z", snaps, joinMaxAge, "3", "90"},
		{"a snapshot older than maxAge is not", "2026-10-24T14:31:00Z", snaps, joinMaxAge, "", ""},
		{"a newer snapshot past maxAge does not hand over to an older one", "2026-10-24T14:00:00Z",
			[]Record{atQueue("2026-10-24T12:00:00Z", 1)}, joinMaxAge, "", ""},
		{"a short window", "2026-10-24T13:10:00Z", snaps, 5 * time.Minute, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, joined, _ := Join(landingColumns, []Row{landing(tc.at)}, tc.snaps, tc.maxAge)
			if got := joined[0]["snap_queue_open"]; got != tc.want {
				t.Errorf("snap_queue_open = %q, want %q", got, tc.want)
			}
			if got := joined[0]["snap_age_min"]; got != tc.wantAge {
				t.Errorf("snap_age_min = %q, want %q", got, tc.wantAge)
			}
		})
	}
}

func TestJoinFlattensNullGroupsNullValuesAndPlacement(t *testing.T) {
	t.Parallel()
	snaps := []Record{
		{
			V:         Version,
			TS:        "2026-10-24T12:00:00Z",
			Groups:    map[string]map[string]any{"host": {"cpu_idle_pct": nil, "open": 3}, "money": nil},
			Placement: map[string]any{"dolt": nil, "overseer": "mac-host"},
		},
		{
			V:      Version,
			TS:     "2026-10-24T12:30:00Z",
			Groups: map[string]map[string]any{"money": {"spend": 4.5}},
		},
	}
	header, joined, _ := Join(landingColumns, []Row{landing("2026-10-24T12:15:00Z"), landing("2026-10-24T13:00:00Z")}, snaps, joinMaxAge)

	wantTail := []string{
		"snap_age_min", "snap_host_cpu_idle_pct", "snap_host_open",
		"snap_money_spend", "snap_place_dolt", "snap_place_overseer",
	}
	if got := header[len(landingColumns):]; strings.Join(got, ",") != strings.Join(wantTail, ",") {
		t.Fatalf("header tail = %v, want %v", got, wantTail)
	}

	// The first landing is joined to the 12:00 snapshot: money is a null
	// group, cpu_idle_pct a null value and dolt a null placement.
	first := joined[0]
	wantFirst := map[string]string{
		"snap_age_min":           "15",
		"snap_host_open":         "3",
		"snap_place_overseer":    "mac-host",
		"snap_host_cpu_idle_pct": "",
		"snap_money_spend":       "",
		"snap_place_dolt":        "",
	}
	for name, want := range wantFirst {
		if got := first[name]; got != want {
			t.Errorf("landing 1 %s = %q, want %q", name, got, want)
		}
	}

	// The second is joined to 12:30, which has no host group and no placement:
	// both are empty, and money's one column carries its value.
	second := joined[1]
	wantSecond := map[string]string{
		"snap_age_min":           "30",
		"snap_money_spend":       "4.5",
		"snap_host_open":         "",
		"snap_host_cpu_idle_pct": "",
		"snap_place_overseer":    "",
	}
	for name, want := range wantSecond {
		if got := second[name]; got != want {
			t.Errorf("landing 2 %s = %q, want %q", name, got, want)
		}
	}
}

func TestJoinWithNoSnapshotsWritesTheInputByteForByte(t *testing.T) {
	t.Parallel()
	want := landingsFixture(t)
	header, rows, err := ReadLandings(bytes.NewReader(want))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	header, joined, cov := Join(header, rows, nil, joinMaxAge)
	var buf bytes.Buffer
	if err := WriteLandings(&buf, header, joined); err != nil {
		t.Fatalf("write the join: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("joined bytes\n got: %q\nwant: %q", buf.String(), want)
	}
	if cov != (Coverage{}) {
		t.Errorf("coverage = %+v, want the zero value with no snapshots", cov)
	}
}

func TestJoinKeepsTheHeaderItWasGiven(t *testing.T) {
	t.Parallel()
	input := []byte("rig,bead,notes\r\ngastown,gt-a,\"x,y\"\r\ngastown,gt-b,\r\n")
	columns, rows, err := ReadLandings(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("read the input: %v", err)
	}
	header, joined, cov := Join(columns, rows, nil, joinMaxAge)
	if want := []string{"rig", "bead", "notes"}; !slices.Equal(header, want) {
		t.Errorf("header = %v, want the input's %v", header, want)
	}
	var buf bytes.Buffer
	if err := WriteLandings(&buf, header, joined); err != nil {
		t.Fatalf("write the join: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), input) {
		t.Errorf("joined bytes\n got: %q\nwant: %q", buf.String(), input)
	}
	if cov != (Coverage{}) {
		t.Errorf("coverage = %+v, want the zero value with no snapshots", cov)
	}

	// A snapshot in force leaves the input's columns leading and joins the
	// snapshot's to the sorted tail, whatever the input's order was.
	header, _, _ = Join([]string{"rig", "notes"}, []Row{{"rig": "gastown", "notes": ""}},
		[]Record{atQueue("2026-10-24T12:00:00Z", 2)}, joinMaxAge)
	if want := []string{"rig", "notes", "snap_age_min", "snap_queue_open"}; !slices.Equal(header, want) {
		t.Errorf("header = %v, want the input's columns then the snapshot's %v", header, want)
	}
}

func TestJoinReportsSnapshotsItCouldNotIndex(t *testing.T) {
	t.Parallel()
	rows := []Row{landing("2026-10-24T12:10:00Z")}
	unreadable := Record{V: Version, TS: "yesterday", Groups: map[string]map[string]any{"queue": {"open": 9}}}
	readable := atQueue("2026-10-24T12:00:00Z", 2)
	tests := []struct {
		name  string
		snaps []Record
		open  string
		want  Coverage
	}{
		{"no snapshots at all", nil, "", Coverage{}},
		{"only an unreadable snapshot", []Record{unreadable}, "", Coverage{Skipped: 1}},
		{"an unreadable snapshot beside a readable one", []Record{readable, unreadable}, "2", Coverage{Cells: 2, Filled: 2, Pct: 100, Skipped: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, joined, cov := Join(landingColumns, rows, tc.snaps, joinMaxAge)
			if cov != tc.want {
				t.Errorf("coverage = %+v, want %+v", cov, tc.want)
			}
			if got := joined[0]["snap_queue_open"]; got != tc.open {
				t.Errorf("snap_queue_open = %q, want %q", got, tc.open)
			}
		})
	}
}

func TestJoinResolvesColumnNameCollisions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rec  Record
		want map[string]string
	}{
		{
			"a group named place does not take the placement namespace",
			Record{V: Version, TS: "2026-10-24T12:00:00Z",
				Groups:    map[string]map[string]any{"place": {"dolt": "from the group"}},
				Placement: map[string]any{"dolt": "from placement"}},
			map[string]string{"snap_place_dolt": "from placement"},
		},
		{
			"an underscore in a group name collides with one in a column name and the group sorting first keeps the cell",
			Record{V: Version, TS: "2026-10-24T12:00:00Z", Groups: map[string]map[string]any{
				"host_a": {"b": "from host_a"},
				"host":   {"a_b": "from host"},
			}},
			map[string]string{"snap_host_a_b": "from host"},
		},
		{
			"a group named age with a column min feeds the column the lag owns and the lag keeps it",
			Record{V: Version, TS: "2026-10-24T12:00:00Z", Groups: map[string]map[string]any{"age": {"min": 42}}},
			map[string]string{"snap_age_min": "0"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, joined, _ := Join(landingColumns, []Row{landing("2026-10-24T12:00:00Z")}, []Record{tc.rec}, joinMaxAge)
			for name, want := range tc.want {
				if got := joined[0][name]; got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}

	// A row that already carries a snap_ cell loses it to the snapshot: the
	// cell is Join's to write, so a second join replaces rather than keeps it.
	row := landing("2026-10-24T12:10:00Z")
	row["snap_queue_open"] = "stale"
	_, joined, _ := Join(landingColumns, []Row{row}, []Record{atQueue("2026-10-24T12:00:00Z", 3)}, joinMaxAge)
	if got := joined[0]["snap_queue_open"]; got != "3" {
		t.Errorf("snap_queue_open = %q, want the snapshot's 3 over the row's stale cell", got)
	}
}

func TestJoinCarriesColumnsItDoesNotKnow(t *testing.T) {
	t.Parallel()
	row := landing("2026-10-24T13:00:00Z")
	row["future_column"] = "kept"
	row["another"] = "also kept"
	header, joined, _ := Join(landingColumns, []Row{row}, []Record{atQueue("2026-10-24T12:00:00Z", 1)}, joinMaxAge)
	wantTail := []string{"another", "future_column", "snap_age_min", "snap_queue_open"}
	if got := header[len(landingColumns):]; strings.Join(got, ",") != strings.Join(wantTail, ",") {
		t.Errorf("header tail = %v, want the unknown columns then the snapshot's, sorted", got)
	}
	if got := joined[0]["future_column"]; got != "kept" {
		t.Errorf("future_column = %q, want the unknown column carried", got)
	}
}

func TestJoinLeavesSnapCellsEmptyWithoutALandingTime(t *testing.T) {
	t.Parallel()
	for _, at := range []string{"", "not-a-time", "2026-10-24 13:00:00"} {
		_, joined, cov := Join(landingColumns, []Row{landing(at)}, []Record{atQueue("2026-10-24T12:00:00Z", 1)}, joinMaxAge)
		if got := joined[0]["snap_queue_open"]; got != "" {
			t.Errorf("landed_at %q: snap_queue_open = %q, want empty", at, got)
		}
		if got := joined[0]["snap_age_min"]; got != "" {
			t.Errorf("landed_at %q: snap_age_min = %q, want empty", at, got)
		}
		if cov.Filled != 0 {
			t.Errorf("landed_at %q: coverage %+v, want no filled cells", at, cov)
		}
	}
}

func TestJoinCoverageCountsTheSnapshotCellsItFilled(t *testing.T) {
	t.Parallel()
	snaps := []Record{atQueue("2026-10-24T12:00:00Z", 3)}
	tests := []struct {
		name string
		rows []Row
		want Coverage
	}{
		{
			"one of two landings joined, two snapshot columns",
			[]Row{landing("2026-10-24T12:10:00Z"), landing("2026-10-24T11:00:00Z")},
			Coverage{Cells: 4, Filled: 2, Pct: 50},
		},
		{"no landings", nil, Coverage{}},
		{"a landing with no snapshot in force", []Row{landing("2026-10-24T11:00:00Z")}, Coverage{Cells: 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, cov := Join(landingColumns, tc.rows, snaps, joinMaxAge)
			if cov != tc.want {
				t.Errorf("coverage = %+v, want %+v", cov, tc.want)
			}
		})
	}
}

func TestJoinRendersSnapshotValuesAsCells(t *testing.T) {
	t.Parallel()
	snap := Record{V: Version, TS: "2026-10-24T12:00:00Z", Groups: map[string]map[string]any{"host": {
		"cpu_idle_pct": 41.5,
		"open":         float64(12),
		"quiet":        true,
		"names":        []any{"a", "b"},
	}}}
	_, joined, _ := Join(landingColumns, []Row{landing("2026-10-24T12:00:00Z")}, []Record{snap}, joinMaxAge)
	want := map[string]string{
		"snap_host_cpu_idle_pct": "41.5",
		"snap_host_open":         "12",
		"snap_host_quiet":        "true",
		"snap_host_names":        `["a","b"]`,
	}
	for name, value := range want {
		if got := joined[0][name]; got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}
