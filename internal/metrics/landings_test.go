package metrics

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// landingsFixture is testdata/landings.v4.csv, the header this join writes,
// bytes included: its CRLF line endings are the extract's and the round trip is
// measured against them. v4 is v3 with the story-point columns appended.
func landingsFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/landings.v4.csv")
	if err != nil {
		t.Fatalf("read the landings fixture: %v", err)
	}
	return data
}

// TestReadLandingsReadsAPreJevFile: landings.v2.csv is the 34-column file the
// extract wrote before the Jev columns appended. It reads under its own header,
// and its rows carry no Jev keys rather than empty ones.
func TestReadLandingsReadsAPreJevFile(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/landings.v2.csv")
	if err != nil {
		t.Fatalf("read the pre-Jev fixture: %v", err)
	}
	header, rows, err := ReadLandings(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("read the pre-Jev fixture: %v", err)
	}
	if want := slices.Concat(legacyLandingColumns, appendedLandingColumns); !slices.Equal(header, want) {
		t.Errorf("header = %v, want the pre-Jev header %v", header, want)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the fixture's 3", len(rows))
	}
	if _, ok := rows[0]["jev_fits20"]; ok {
		t.Errorf("rows[0] carries jev_fits20 = %q, want no such key for a pre-Jev row", rows[0]["jev_fits20"])
	}
}

// TestReadLandingsReadsAPreChangeFile: landings.v1.csv is the 25-column file the
// extract wrote before the stage columns appended. It still reads as the
// contract it was, its header returned as written and its rows keyed by it, so
// a reader of an older file keeps working and does not see the appended
// columns as empty keys (gt-p9pok).
func TestReadLandingsReadsAPreChangeFile(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/landings.v1.csv")
	if err != nil {
		t.Fatalf("read the pre-change fixture: %v", err)
	}
	header, rows, err := ReadLandings(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("read the pre-change fixture: %v", err)
	}
	legacy := legacyLandingColumns
	if !slices.Equal(header, legacy) {
		t.Errorf("header = %v, want the pre-change header %v", header, legacy)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the fixture's 3", len(rows))
	}
	if got, want := rows[0]["model"], "opus"; got != want {
		t.Errorf("rows[0][model] = %q, want %q", got, want)
	}
	if _, ok := rows[0]["created_at"]; ok {
		t.Errorf("rows[0] carries created_at = %q, want a pre-change row to have no such key", rows[0]["created_at"])
	}
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

// TestLandingColumnsKeepThePinnedHeader: the pinned column list is the head of
// landingColumns, in its order, and the stage columns this join appends are the
// only ones after them. The pinned reference stays at the header the extract was
// ported with, so the Go contract extends it append-only: an existing column
// keeps its name and place (gt-p9pok).
func TestLandingColumnsKeepThePinnedHeader(t *testing.T) {
	t.Parallel()
	legacy := []string{
		"bead", "rig", "landed_at", "week", "route", "om_verdict", "gate_result",
		"lines_added", "lines_deleted", "lines_total", "files_touched",
		"file_entropy", "package_entropy", "attempts", "rejected_before_landing",
		"manual_rescue", "reached_overrun_stop", "unreadable", "type", "priority",
		"shaped", "label_count", "model", "om_threshold", "dominant_model",
	}
	if len(landingColumns) < len(legacy) {
		t.Fatalf("landingColumns has %d columns, want at least the pinned %d", len(landingColumns), len(legacy))
	}
	if got := landingColumns[:len(legacy)]; !slices.Equal(got, legacy) {
		t.Errorf("landingColumns starts %v, want the pinned header %v", got, legacy)
	}
	want := slices.Concat(appendedLandingColumns, jevLandingColumns, storyPointLandingColumns, omLandingColumns)
	if got := landingColumns[len(legacy):]; !slices.Equal(got, want) {
		t.Errorf("appended columns = %v, want %v", got, want)
	}
}

// polecatLedgerLine is one ledger.jsonl line for a landing, with the polecat
// members in extra (raw JSON members, or "" for a line from before they existed).
func polecatLedgerLine(extra string) string {
	base := `"bead":"gt-p1","rig":"gastown","landed_commit":"c-p1","landed_at":"2026-10-24T12:00:00Z",` +
		`"route":"daemon","om_verdict":"approve","gate_result":"build"`
	if extra == "" {
		return "{" + base + "}"
	}
	return "{" + base + "," + extra + "}"
}

// polecatCells reads one ledger line through LoadLedger and the BuildLandings
// join the extract runs, and returns the landing's row.
func polecatCells(t *testing.T, line string) Row {
	t.Helper()
	ledger := LoadLedger(map[string][]string{"gastown": {line}}, &Dropped{})
	rows := buildLedgerLandings(t, ledger, nil, nil, StageStamps{})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the one landing", len(rows))
	}
	return rows[0]
}

// TestPolecatCellsFollowTheLedgerLine: a ledger line that carries the polecat
// time, bound and agent fills the new columns, and a line from before those
// fields existed leaves every one of them empty (gt-5d9k2f.3).
func TestPolecatCellsFollowTheLedgerLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		extra string
		want  map[string]string
	}{
		{
			name:  "a measured landing",
			extra: `"polecat_secs":600,"agent":"sbx-haiku"`,
			want: map[string]string{"polecat_secs": "600", "polecat_lower_bound": "0",
				"agent": "sbx-haiku", "seat_class": "haiku", "sp_actual": "8"},
		},
		{
			name:  "a lower-bound time has no points",
			extra: `"polecat_secs":600,"polecat_lower_bound":true,"agent":"claude-sonnet"`,
			want: map[string]string{"polecat_secs": "600", "polecat_lower_bound": "1",
				"agent": "claude-sonnet", "seat_class": "sonnet", "sp_actual": ""},
		},
		{
			name:  "a bound with no time",
			extra: `"polecat_lower_bound":true`,
			want: map[string]string{"polecat_secs": "", "polecat_lower_bound": "1",
				"agent": "", "seat_class": "unknown", "sp_actual": ""},
		},
		{
			name:  "a line from before the fields existed",
			extra: "",
			want: map[string]string{"polecat_secs": "", "polecat_lower_bound": "",
				"agent": "", "seat_class": "unknown", "sp_actual": ""},
		},
		{
			name:  "zero seconds has no points",
			extra: `"polecat_secs":0`,
			want: map[string]string{"polecat_secs": "", "polecat_lower_bound": "",
				"agent": "", "seat_class": "unknown", "sp_actual": ""},
		},
		{
			name:  "a negative time reads as absent",
			extra: `"polecat_secs":-60`,
			want: map[string]string{"polecat_secs": "", "polecat_lower_bound": "",
				"agent": "", "seat_class": "unknown", "sp_actual": ""},
		},
		{
			name:  "twenty minutes is the twenty-one-point bucket",
			extra: `"polecat_secs":1200,"agent":"deepseek-v3"`,
			want: map[string]string{"polecat_secs": "1200", "polecat_lower_bound": "0",
				"agent": "deepseek-v3", "seat_class": "flash", "sp_actual": "21"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := polecatCells(t, polecatLedgerLine(tc.extra))
			for col, want := range tc.want {
				if got := row[col]; got != want {
					t.Errorf("%s = %q, want %q", col, got, want)
				}
			}
		})
	}
}

// TestPolecatAttemptsCellCountsTheRecordsSeats: polecat_attempts is the number of
// entries the landing record carries, and a record without the key, or with an
// empty list, leaves the cell empty rather than a count of zero (gt-gk99dq).
func TestPolecatAttemptsCellCountsTheRecordsSeats(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{
			name:  "two seats on the bead",
			extra: `"polecat_attempts":[{"seat":"gastown/a","secs":300},{"seat":"gastown/b","secs":120,"lower_bound":true}]`,
			want:  "2",
		},
		{
			name:  "one seat on the bead",
			extra: `"polecat_attempts":[{"seat":"gastown/a","secs":300}]`,
			want:  "1",
		},
		{
			name:  "a record without the key",
			extra: `"polecat_secs":600`,
			want:  "",
		},
		{
			name:  "an empty list",
			extra: `"polecat_attempts":[]`,
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := polecatCells(t, polecatLedgerLine(tc.extra))
			if got := row["polecat_attempts"]; got != tc.want {
				t.Errorf("polecat_attempts = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSeatClassFromTheAgentName: each agent the design names maps to its seat
// class through the extract, and a name in none of them is empty.
func TestSeatClassFromTheAgentName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"sbx-haiku":     "haiku",
		"claude-haiku":  "haiku",
		"sbx-flash":     "flash",
		"deepseek-v3":   "flash",
		"deepseek":      "flash",
		"claude-sonnet": "sonnet",
		"claude-opus":   "unknown",
		"sbx-haiku-2":   "unknown",
		"Deepseek-v3":   "unknown",
		"":              "unknown",
	}
	for agent, want := range cases {
		row := polecatCells(t, polecatLedgerLine(`"polecat_secs":600,"agent":"`+agent+`"`))
		if got := row["seat_class"]; got != want {
			t.Errorf("seat_class(%q) = %q, want %q", agent, got, want)
		}
	}
}

// buildLedgerLandings runs BuildLandings, the production join Extract calls,
// for ledger, so a test drives the model, stage stamps and time to land the
// extract actually writes rather than a helper beside it.
func buildLedgerLandings(t *testing.T, ledger []LedgerRow, beads map[string]BeadRow, usage map[string]Usage, stamps StageStamps) []Row {
	t.Helper()
	rows, err := BuildLandings(context.Background(), ledger, nil, fakeDiffSource{},
		beads, usage, nil, nil, nil, nil, stamps, nil, &Dropped{})
	if err != nil {
		t.Fatalf("BuildLandings: %v", err)
	}
	return rows
}

// TestLedgerNewColumnsAppendAtTheEnd: the stage, time-to-land and polecat-time
// columns extend the header, so a reader of the extract's existing columns finds
// each in the place it was written (gt-p9pok).
func TestLedgerNewColumnsAppendAtTheEnd(t *testing.T) {
	t.Parallel()
	legacy := []string{
		"bead", "rig", "landed_at", "week", "route", "om_verdict", "gate_result",
		"lines_added", "lines_deleted", "lines_total", "files_touched",
		"file_entropy", "package_entropy", "attempts", "rejected_before_landing",
		"manual_rescue", "reached_overrun_stop", "unreadable", "type", "priority",
		"shaped", "label_count", "model", "om_threshold", "dominant_model",
	}
	appended := []string{
		"created_at", "dispatched_at", "submitted_at", "time_to_land_s",
		"polecat_secs", "polecat_lower_bound", "agent", "seat_class", "sp_actual",
		"jev_fits20", "jev_size", "sp", "sp_rung", "sp_minutes", "sp_estimated_at",
		"om_rounds", "polecat_attempts",
	}
	if len(landingColumns) != len(legacy)+len(appended) {
		t.Fatalf("landingColumns has %d columns, want %d", len(landingColumns), len(legacy)+len(appended))
	}
	if got := landingColumns[:len(legacy)]; !slices.Equal(got, legacy) {
		t.Errorf("existing columns = %v, want the extract's %v", got, legacy)
	}
	if got := landingColumns[len(legacy):]; !slices.Equal(got, appended) {
		t.Errorf("appended columns = %v, want %v", got, appended)
	}
}

// TestLedgerModelFilledFromSeatRecords: a landing whose bead left no usage
// model takes the model of the seat that wrote the landed branch, read from
// that seat's other records; a landing no record names keeps the cell empty.
func TestLedgerModelFilledFromSeatRecords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		branch string
		files  map[string]UsageFile
		want   string
	}{
		{
			name:   "the landed seat's model answers a bead with no record of its own",
			branch: "polecat/alfa/gt-1",
			files: map[string]UsageFile{
				"gt-other": {Rig: "gastown", Seat: "alfa", Models: map[string]UsageModel{"claude-opus": {Calls: 2}}},
			},
			want: "claude-opus",
		},
		{
			name:   "the bead's own record answers when it names the landed seat",
			branch: "polecat/alfa/gt-1",
			files: map[string]UsageFile{
				"gt-1": {Rig: "gastown", Seat: "alfa", Models: map[string]UsageModel{"claude-haiku": {Calls: 1}}},
			},
			want: "claude-haiku",
		},
		{
			name:   "a record of another seat is not the landed attempt's model",
			branch: "polecat/alfa/gt-1",
			files: map[string]UsageFile{
				"gt-1":     {Rig: "gastown", Seat: "bravo", Models: map[string]UsageModel{"claude-haiku": {Calls: 1}}},
				"gt-other": {Rig: "gastown", Seat: "alfa", Models: map[string]UsageModel{"claude-opus": {Calls: 2}}},
			},
			want: "claude-opus",
		},
		{
			name:   "a landing with no seat record and no usage keeps the model empty",
			branch: "",
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ledger := []LedgerRow{{
				Bead: "gt-1", Rig: "gastown", LandedCommit: "c1",
				LandedAt: "2026-10-14T00:00:00Z", Branch: tc.branch,
			}}
			stamps := StageStamps{Seats: ReadUsageSeats(tc.files)}
			rows := buildLedgerLandings(t, ledger, map[string]BeadRow{}, LoadUsage(tc.files), stamps)
			if got := rows[0]["model"]; got != tc.want {
				t.Errorf("gt-1 model = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLedgerModelUnknownIsCountedNotDropped: the landing a record cannot name
// keeps its row with an empty model, and the footer count says how many are
// like it.
func TestLedgerModelUnknownIsCountedNotDropped(t *testing.T) {
	t.Parallel()
	ledger := []LedgerRow{
		{Bead: "gt-1", Rig: "gastown", LandedCommit: "c1", LandedAt: "2026-10-14T00:00:00Z"},
		{Bead: "gt-2", Rig: "gastown", LandedCommit: "c2", LandedAt: "2026-10-14T00:00:00Z", Branch: "crew/merlin/gt-2"},
	}
	rows := buildLedgerLandings(t, ledger, map[string]BeadRow{}, map[string]Usage{"gt-2": {Model: "opus"}}, StageStamps{})
	if got := len(rows); got != 2 {
		t.Fatalf("rows = %d, want both landings kept", got)
	}
	if got, want := CountLandingGaps(rows).ModelUnknown, 1; got != want {
		t.Errorf("model_unknown = %d, want %d", got, want)
	}
}

// TestLedgerTimeToLand: the cell is created_at to landed_at in seconds, and it
// is empty when a stamp is missing or the bead was created after it landed,
// with the footer counting each empty stamp.
func TestLedgerTimeToLand(t *testing.T) {
	t.Parallel()
	beads := map[string]BeadRow{
		"gt-a": {ID: "gt-a", CreatedAt: "2026-10-12T00:00:00Z"},
		"gt-b": {ID: "gt-b"},
		"gt-c": {ID: "gt-c", CreatedAt: "2026-10-15T00:00:00Z"},
	}
	ledger := []LedgerRow{
		{Bead: "gt-a", Rig: "gastown", LandedCommit: "c1", LandedAt: "2026-10-14T00:00:00Z"},
		{Bead: "gt-b", Rig: "gastown", LandedCommit: "c2", LandedAt: "2026-10-14T00:00:00Z"},
		{Bead: "gt-c", Rig: "gastown", LandedCommit: "c3", LandedAt: "2026-10-14T00:00:00Z"},
	}
	rows := buildLedgerLandings(t, ledger, beads, nil, StageStamps{})
	want := map[string]string{"gt-a": "172800", "gt-b": "", "gt-c": ""}
	for _, row := range rows {
		if got, ok := want[row["bead"]]; !ok || row["time_to_land_s"] != got {
			t.Errorf("%s time_to_land_s = %q, want %q", row["bead"], row["time_to_land_s"], got)
		}
	}
	// gt-b names no created_at; gt-c's is present but follows its landing, so
	// only the time to land is empty for it.
	if got, want := CountLandingGaps(rows).StageStampMissing["created_at"], 1; got != want {
		t.Errorf("stage_stamp_missing_created_at = %d, want %d", got, want)
	}
}

// TestLedgerSubmittedPrefersTheNoteStamp: the READY TO LAND block's stamp is
// the submission gt done wrote, so it answers before the feed's done event.
func TestLedgerSubmittedPrefersTheNoteStamp(t *testing.T) {
	t.Parallel()
	ledger := []LedgerRow{{Bead: "gt-a", Rig: "gastown", LandedCommit: "c1", LandedAt: "2026-10-14T00:00:00Z"}}
	beads := map[string]BeadRow{"gt-a": {ID: "gt-a", SubmittedAt: "2026-10-13T06:00:00Z"}}
	stamps := StageStamps{Submitted: map[string]string{"gt-a": "2026-10-13T09:00:00Z"}}
	rows := buildLedgerLandings(t, ledger, beads, nil, stamps)
	if got, want := rows[0]["submitted_at"], "2026-10-13T06:00:00Z"; got != want {
		t.Errorf("submitted_at = %q, want the READY TO LAND stamp %q", got, want)
	}
}

// TestLedgerDispatchedStampFromTheFeed: the feed's dispatch answers a landing's
// cell, and a bead the feed never slung keeps it empty and counted.
func TestLedgerDispatchedStampFromTheFeed(t *testing.T) {
	t.Parallel()
	ledger := []LedgerRow{
		{Bead: "gt-a", Rig: "gastown", LandedCommit: "c1", LandedAt: "2026-10-14T00:00:00Z"},
		{Bead: "gt-b", Rig: "gastown", LandedCommit: "c2", LandedAt: "2026-10-14T00:00:00Z"},
	}
	stamps := StageStamps{Dispatched: map[string]string{"gt-a": "2026-10-13T00:00:00Z"}}
	rows := buildLedgerLandings(t, ledger, map[string]BeadRow{}, nil, stamps)
	if got, want := rows[0]["dispatched_at"], "2026-10-13T00:00:00Z"; got != want {
		t.Errorf("gt-a dispatched_at = %q, want %q", got, want)
	}
	if got := rows[1]["dispatched_at"]; got != "" {
		t.Errorf("gt-b dispatched_at = %q, want empty for a bead the feed never slung", got)
	}
	if got, want := CountLandingGaps(rows).StageStampMissing["dispatched_at"], 1; got != want {
		t.Errorf("stage_stamp_missing_dispatched_at = %d, want %d", got, want)
	}
}

// TestLedgerSubmittedNoteStampIsGuarded: the READY TO LAND note is rewritten by
// a later run's submission, so a note stamp after the landing, or one that is
// not a timestamp, is not this landing's. The feed's in-time done event then
// answers, and a landing neither source stamps is counted missing.
func TestLedgerSubmittedNoteStampIsGuarded(t *testing.T) {
	t.Parallel()
	ledger := []LedgerRow{{Bead: "gt-a", Rig: "gastown", LandedCommit: "c1", LandedAt: "2026-10-14T00:00:00Z"}}
	tests := []struct {
		name        string
		note        string
		feed        map[string]string
		want        string
		wantMissing int
	}{
		{"a note stamp after the landing is refused", "2026-10-15T00:00:00Z", nil, "", 1},
		{"a malformed note stamp is refused", "not-a-time", nil, "", 1},
		{"the feed's in-time done event answers a refused note", "2026-10-15T00:00:00Z",
			map[string]string{"gt-a": "2026-10-13T00:00:00Z"}, "2026-10-13T00:00:00Z", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			beads := map[string]BeadRow{"gt-a": {ID: "gt-a", SubmittedAt: tc.note}}
			rows := buildLedgerLandings(t, ledger, beads, nil, StageStamps{Submitted: tc.feed})
			if got := rows[0]["submitted_at"]; got != tc.want {
				t.Errorf("submitted_at = %q, want %q", got, tc.want)
			}
			if got := CountLandingGaps(rows).StageStampMissing["submitted_at"]; got != tc.wantMissing {
				t.Errorf("stage_stamp_missing_submitted_at = %d, want %d", got, tc.wantMissing)
			}
		})
	}
}

// TestLedgerMalformedCreatedAtIsMissing: a created_at the join cannot parse is
// not a stamp, so the cell is empty, time_to_land_s is empty, and the footer
// counts the landing rather than the row carrying text no reader can use.
func TestLedgerMalformedCreatedAtIsMissing(t *testing.T) {
	t.Parallel()
	beads := map[string]BeadRow{"gt-a": {ID: "gt-a", CreatedAt: "not-a-time"}}
	ledger := []LedgerRow{{Bead: "gt-a", Rig: "gastown", LandedCommit: "c1", LandedAt: "2026-10-14T00:00:00Z"}}
	rows := buildLedgerLandings(t, ledger, beads, nil, StageStamps{})
	if got := rows[0]["created_at"]; got != "" {
		t.Errorf("created_at = %q, want an empty cell for an unparsable stamp", got)
	}
	if got := rows[0]["time_to_land_s"]; got != "" {
		t.Errorf("time_to_land_s = %q, want empty without a readable created_at", got)
	}
	if got, want := CountLandingGaps(rows).StageStampMissing["created_at"], 1; got != want {
		t.Errorf("stage_stamp_missing_created_at = %d, want %d", got, want)
	}
}

// TestLedgerSubmittedAfterTheLandingIsMissing: a done event after the landing
// belongs to a later run and is not this landing's submission stamp.
func TestLedgerSubmittedAfterTheLandingIsMissing(t *testing.T) {
	t.Parallel()
	stamps := StageStamps{Submitted: map[string]string{"gt-a": "2026-10-15T00:00:00Z"}}
	ledger := []LedgerRow{{Bead: "gt-a", Rig: "gastown", LandedCommit: "c1", LandedAt: "2026-10-14T00:00:00Z"}}
	rows := buildLedgerLandings(t, ledger, map[string]BeadRow{"gt-a": {ID: "gt-a"}}, nil, stamps)
	if got := rows[0]["submitted_at"]; got != "" {
		t.Errorf("submitted_at = %q, want empty for a done event after the landing", got)
	}
	if got, want := CountLandingGaps(rows).StageStampMissing["submitted_at"], 1; got != want {
		t.Errorf("stage_stamp_missing_submitted_at = %d, want %d", got, want)
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
