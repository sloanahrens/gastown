package metrics

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// These tests build the archive the export writes: one gzip CSV per database
// with the events header and one row per event.

// eventsArchiveHeader is the header the export writes, in order.
var eventsArchiveHeader = []string{"issue_id", "event_type", "actor", "old_value", "new_value", "comment", "created_at", "id"}

// archiveRow is one archive record with the fields the tests set; comment and
// id stay empty.
func archiveRow(issue, kind, actor, oldValue, newValue, createdAt string) []string {
	return []string{issue, kind, actor, oldValue, newValue, "", createdAt, ""}
}

// eventsArchive builds one gzip CSV from rows.
func eventsArchive(t *testing.T, rows [][]string) []byte {
	t.Helper()
	return eventsArchiveWithHeader(t, eventsArchiveHeader, rows)
}

// eventsArchiveWithHeader builds one gzip CSV whose header is header, so a test
// can drop a column the rollup needs.
func eventsArchiveWithHeader(t *testing.T, header []string, rows [][]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	w := csv.NewWriter(zw)
	if err := w.Write(header); err != nil {
		t.Fatalf("write header: %v", err)
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			t.Fatalf("write row: %v", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatalf("flush rows: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// eventsFS is an archive directory whose files are the given gzip blobs.
func eventsFS(dir string, files map[string][]byte) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, data := range files {
		fsys[dir+"/"+name] = &fstest.MapFile{Data: data}
	}
	return fsys
}

// findBeadChange returns the row for database and issue.
func findBeadChange(t *testing.T, rows []BeadChangeRow, database, issue string) BeadChangeRow {
	t.Helper()
	for _, row := range rows {
		if row.Database == database && row.IssueID == issue {
			return row
		}
	}
	t.Fatalf("no row for %s/%s in %+v", database, issue, rows)
	return BeadChangeRow{}
}

func TestBeadChangesCountsEventTypesPerBead(t *testing.T) {
	t.Parallel()
	fsys := eventsFS("archive", map[string][]byte{
		"events-gastown.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-a", "created", "alice", "", `{"title":"a"}`, "2026-10-01T10:00:00Z"),
			archiveRow("gt-a", "updated", "alice", `{"description":"d"}`, `{"assignee":"bob"}`, "2026-10-01T10:10:00Z"),
			archiveRow("gt-a", "label_added", "carol", "", "handoff", "2026-10-01T10:20:00Z"),
			archiveRow("gt-a", "label_added", "carol", "", "x", "2026-10-01T10:21:00Z"),
			archiveRow("gt-a", "label_removed", "carol", "x", "", "2026-10-01T10:22:00Z"),
			archiveRow("gt-a", "claimed", "bob", "", "", "2026-10-01T10:30:00Z"),
			archiveRow("gt-a", "closed", "bob", "in_progress", "closed", "2026-10-01T12:00:00Z"),
			archiveRow("gt-a", "reopened", "dan", "closed", "open", "2026-10-01T13:00:00Z"),
			archiveRow("gt-b", "created", "erin", "", `{"title":"b"}`, "2026-10-02T00:00:00Z"),
			archiveRow("gt-b", "updated", "erin", `{"description":"e"}`, `{"priority":1}`, "2026-10-02T00:05:00Z"),
		}),
		"events-town.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-a", "created", "frank", "", `{"title":"a"}`, "2026-10-01T08:00:00Z"),
			archiveRow("gt-a", "updated", "frank", `{"description":"f"}`, `{"priority":2}`, "2026-10-01T09:00:00Z"),
			archiveRow("gt-a", "label_added", "frank", "", "bug", "2026-10-01T09:30:00Z"),
		}),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if dropped.Count() != 0 {
		t.Fatalf("dropped %d rows, want none: %+v", dropped.Count(), dropped.Items)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}

	gastownA := findBeadChange(t, rows, "gastown", "gt-a")
	if gastownA.UpdateCount != 1 || gastownA.TextUpdateCount != 0 {
		t.Errorf("gt-a updates = %d, text updates = %d, want 1 and 0", gastownA.UpdateCount, gastownA.TextUpdateCount)
	}
	if gastownA.LabelAddedCount != 2 || gastownA.LabelRemovedCount != 1 {
		t.Errorf("gt-a labels added = %d, removed = %d, want 2 and 1", gastownA.LabelAddedCount, gastownA.LabelRemovedCount)
	}
	if gastownA.ReopenedCount != 1 {
		t.Errorf("gt-a reopened = %d, want 1", gastownA.ReopenedCount)
	}
	if gastownA.ClaimedAt != "2026-10-01T10:30:00Z" {
		t.Errorf("gt-a claimed_at = %q, want the first claimed event", gastownA.ClaimedAt)
	}
	if gastownA.ClosedAt != "2026-10-01T12:00:00Z" {
		t.Errorf("gt-a closed_at = %q, want the closed event", gastownA.ClosedAt)
	}
	if gastownA.DistinctActorCount != 4 {
		t.Errorf("gt-a distinct actors = %d, want alice, carol, bob and dan", gastownA.DistinctActorCount)
	}

	gastownB := findBeadChange(t, rows, "gastown", "gt-b")
	if gastownB.UpdateCount != 1 || gastownB.DistinctActorCount != 1 {
		t.Errorf("gt-b updates = %d, actors = %d, want 1 and 1", gastownB.UpdateCount, gastownB.DistinctActorCount)
	}
	if gastownB.ClaimedAt != "" || gastownB.ClosedAt != "" || gastownB.ReopenedCount != 0 {
		t.Errorf("gt-b carries stamps it has no event for: %+v", gastownB)
	}

	townA := findBeadChange(t, rows, "town", "gt-a")
	if townA.UpdateCount != 1 || townA.LabelAddedCount != 1 || townA.DistinctActorCount != 1 {
		t.Errorf("town/gt-a updates = %d, labels = %d, actors = %d, want 1, 1 and 1",
			townA.UpdateCount, townA.LabelAddedCount, townA.DistinctActorCount)
	}
	if townA.ClosedAt != "" {
		t.Errorf("town/gt-a closed_at = %q, want empty: the databases are counted apart", townA.ClosedAt)
	}
}

func TestBeadChangesTextUpdatesAndFirstFiledDescription(t *testing.T) {
	t.Parallel()
	fsys := eventsFS("archive", map[string][]byte{
		"events-gastown.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-text", "created", "alice", "", `{"title":"t"}`, "2026-10-01T00:00:00Z"),
			archiveRow("gt-text", "updated", "alice", `{"description":"filed","created_at":"2026-10-01T00:00:00Z"}`, `{"description":"revised"}`, "2026-10-01T00:01:00Z"),
			archiveRow("gt-text", "updated", "bob", `{"description":"revised"}`, `{"assignee":"bob"}`, "2026-10-01T00:02:00Z"),
			archiveRow("gt-text", "updated", "bob", `{"description":"revised","notes":"n"}`, `{"notes":"more"}`, "2026-10-01T00:05:00Z"),
		}),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if dropped.Count() != 0 {
		t.Fatalf("dropped %d rows, want none: %+v", dropped.Count(), dropped.Items)
	}
	row := findBeadChange(t, rows, "gastown", "gt-text")
	if row.UpdateCount != 3 {
		t.Errorf("updates = %d, want 3: the assignee-only row is an update", row.UpdateCount)
	}
	if row.TextUpdateCount != 2 {
		t.Errorf("text updates = %d, want 2: description and notes rows only", row.TextUpdateCount)
	}
	if row.FirstFiledDescription != "filed" {
		t.Errorf("first_filed_description = %q, want the description from the earliest description update's old_value", row.FirstFiledDescription)
	}
	if row.SecondsFromCreatedToFirstTextUpdate == nil || *row.SecondsFromCreatedToFirstTextUpdate != 60 {
		t.Errorf("seconds from created to first text update = %v, want 60", row.SecondsFromCreatedToFirstTextUpdate)
	}
}

func TestBeadChangesDropsUnparseableRowsLoudly(t *testing.T) {
	t.Parallel()
	fsys := eventsFS("archive", map[string][]byte{
		"events-gastown.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-1", "created", "alice", "", `{"title":"t"}`, "2026-10-01T00:00:00Z"),
			archiveRow("gt-1", "updated", "alice", `{"description":"d"}`, `{"assignee":"bob"}`, "not-a-time"),
			archiveRow("gt-1", "updated", "alice", `{"description":"d"}`, `{"assignee":`, "2026-10-01T00:02:00Z"),
			archiveRow("gt-1", "updated", "alice", `{"description":"d"}`, `{"priority":1}`, "2026-10-01T00:03:00Z"),
		}),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].UpdateCount != 1 {
		t.Errorf("updates = %d, want 1: the readable update still counts", rows[0].UpdateCount)
	}
	if dropped.Count() != 2 {
		t.Fatalf("dropped %d items, want 2: %+v", dropped.Count(), dropped.Items)
	}
	var reasons []string
	for _, item := range dropped.Items {
		reasons = append(reasons, item.Reason)
		if !strings.Contains(item.Source, "events-gastown.csv.gz") {
			t.Errorf("drop source = %q, want the file it was read from", item.Source)
		}
	}
	joined := strings.Join(reasons, "\n")
	if !strings.Contains(joined, "created_at") {
		t.Errorf("drop reasons %q do not name the bad created_at", reasons)
	}
	if !strings.Contains(joined, "new_value") {
		t.Errorf("drop reasons %q do not name the bad new_value", reasons)
	}
}

func TestWriteBeadChangesCSVIsStable(t *testing.T) {
	t.Parallel()
	seconds := 60
	rows := []BeadChangeRow{
		{
			Database: "gastown", IssueID: "gt-b", CreatedAt: "2026-10-02T00:00:00Z",
			UpdateCount: 1, DistinctActorCount: 1,
		},
		{
			Database: "gastown", IssueID: "gt-a", CreatedAt: "2026-10-01T00:00:00Z",
			FirstUpdateAt: "2026-10-01T00:01:00Z", ClaimedAt: "2026-10-01T00:02:00Z",
			UpdateCount: 2, TextUpdateCount: 1, LabelAddedCount: 1,
			DistinctActorCount: 2, SecondsFromCreatedToFirstTextUpdate: &seconds,
			FirstFiledDescription: `one, "two"` + "\nthree",
		},
	}
	var first, second bytes.Buffer
	if err := WriteBeadChangesCSV(&first, rows); err != nil {
		t.Fatalf("first write: %v", err)
	}
	reversed := []BeadChangeRow{rows[1], rows[0]}
	if err := WriteBeadChangesCSV(&second, reversed); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("writes differ by input order:\n%q\n%q", first.String(), second.String())
	}

	records, err := csv.NewReader(bytes.NewReader(first.Bytes())).ReadAll()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want a header and two rows", len(records))
	}
	if !reflect.DeepEqual(records[0], BeadChangeColumns) {
		t.Errorf("header = %v, want %v", records[0], BeadChangeColumns)
	}
	if records[1][1] != "gt-a" {
		t.Errorf("first row is %q, want gt-a: rows sort by database then issue id", records[1][1])
	}
	description := records[1][len(BeadChangeColumns)-1]
	if description != `one, "two"`+"\nthree" {
		t.Errorf("description cell = %q, want the text as filed", description)
	}
}

// failsAfterWriter accepts allow writes and fails every one after them, so a test
// drives the CSV writer's failure paths.
type failsAfterWriter struct {
	allow int
	wrote int
}

func (w *failsAfterWriter) Write(p []byte) (int, error) {
	if w.wrote >= w.allow {
		return 0, errors.New("write failed")
	}
	w.wrote++
	return len(p), nil
}

func TestWriteBeadChangesCSVReportsWriteFailures(t *testing.T) {
	t.Parallel()
	rows := []BeadChangeRow{{Database: "gastown", IssueID: "gt-a"}}
	cases := []struct {
		name  string
		allow int
		want  string
	}{
		// The header is the first write; the first row is the second.
		{name: "header write fails", allow: 0, want: "header"},
		{name: "row write fails", allow: 1, want: "row 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := WriteBeadChangesCSV(&failsAfterWriter{allow: tc.allow}, rows)
			if err == nil {
				t.Fatalf("no error from a writer that fails after %d writes, want one", tc.allow)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestBeadChangesTakesLatestCloseAndEarliestClaim(t *testing.T) {
	t.Parallel()
	fsys := eventsFS("archive", map[string][]byte{
		"events-gastown.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-1", "created", "alice", "", `{"title":"t"}`, "2026-10-01T00:00:00Z"),
			archiveRow("gt-1", "claimed", "alice", "", "", "2026-10-01T00:10:00Z"),
			archiveRow("gt-1", "claimed", "bob", "", "", "2026-10-01T00:20:00Z"),
			archiveRow("gt-1", "closed", "bob", "", "", "2026-10-01T01:00:00Z"),
			archiveRow("gt-1", "reopened", "bob", "", "", "2026-10-01T02:00:00Z"),
			archiveRow("gt-1", "closed", "bob", "", "", "2026-10-01T03:00:00Z"),
		}),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	row := findBeadChange(t, rows, "gastown", "gt-1")
	if row.ClaimedAt != "2026-10-01T00:10:00Z" {
		t.Errorf("claimed_at = %q, want the first claimed event", row.ClaimedAt)
	}
	if row.ClosedAt != "2026-10-01T03:00:00Z" {
		t.Errorf("closed_at = %q, want the last close", row.ClosedAt)
	}
}

func TestBeadChangesFallsBackToOldValueCreatedAt(t *testing.T) {
	t.Parallel()
	fsys := eventsFS("archive", map[string][]byte{
		"events-gastown.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-old", "updated", "alice", `{"created_at":"2026-09-01T00:00:00Z","description":"d"}`, `{"description":"revised"}`, "2026-10-01T00:03:00Z"),
		}),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	row := findBeadChange(t, rows, "gastown", "gt-old")
	if row.CreatedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("created_at = %q, want the created_at from the earliest update's old_value", row.CreatedAt)
	}
	if row.SecondsFromCreatedToFirstTextUpdate == nil || *row.SecondsFromCreatedToFirstTextUpdate != 2592180 {
		t.Errorf("seconds from created to first text update = %v, want 2592180", row.SecondsFromCreatedToFirstTextUpdate)
	}
}

func TestBeadChangesAcceptsDoltDatetime(t *testing.T) {
	t.Parallel()
	fsys := eventsFS("archive", map[string][]byte{
		"events-gastown.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-1", "created", "alice", "", `{"title":"t"}`, "2026-10-01 00:00:00"),
			archiveRow("gt-1", "updated", "alice", `{"description":"d"}`, `{"description":"revised"}`, "2026-10-01 00:02:00"),
		}),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if dropped.Count() != 0 {
		t.Fatalf("dropped %d rows of a Dolt DATETIME export: %+v", dropped.Count(), dropped.Items)
	}
	row := findBeadChange(t, rows, "gastown", "gt-1")
	if row.CreatedAt != "2026-10-01 00:00:00" {
		t.Errorf("created_at = %q, want the export's own text", row.CreatedAt)
	}
	if row.SecondsFromCreatedToFirstTextUpdate == nil || *row.SecondsFromCreatedToFirstTextUpdate != 120 {
		t.Errorf("seconds from created to first text update = %v, want 120", row.SecondsFromCreatedToFirstTextUpdate)
	}
}

func TestBeadChangesRefusesBadRows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		row  []string
		want string
	}{
		{
			name: "empty issue id",
			row:  archiveRow("", "created", "alice", "", `{}`, "2026-10-01T00:00:00Z"),
			want: "issue_id",
		},
		{
			name: "unknown event type",
			row:  archiveRow("gt-1", "deleted", "alice", "", "", "2026-10-01T00:00:00Z"),
			want: "event_type",
		},
		{
			name: "bad created_at",
			row:  archiveRow("gt-1", "created", "alice", "", `{}`, "yesterday"),
			want: "created_at",
		},
		{
			name: "new_value is array",
			row:  archiveRow("gt-1", "updated", "alice", `{}`, `["x"]`, "2026-10-01T00:00:00Z"),
			want: "new_value",
		},
		{
			name: "new_value is null",
			row:  archiveRow("gt-1", "updated", "alice", `{}`, `null`, "2026-10-01T00:00:00Z"),
			want: "new_value",
		},
		{
			name: "old_value is not an object",
			row:  archiveRow("gt-1", "updated", "alice", `"d"`, `{"assignee":"bob"}`, "2026-10-01T00:00:00Z"),
			want: "old_value",
		},
		{
			name: "old_value description is not a string",
			row:  archiveRow("gt-1", "updated", "alice", `{"description":7}`, `{"assignee":"bob"}`, "2026-10-01T00:00:00Z"),
			want: "old_value.description",
		},
		{
			name: "old_value created_at is not a string",
			row:  archiveRow("gt-1", "updated", "alice", `{"created_at":7}`, `{"assignee":"bob"}`, "2026-10-01T00:00:00Z"),
			want: "old_value.created_at",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := eventsFS("archive", map[string][]byte{
				"events-gastown.csv.gz": eventsArchive(t, [][]string{
					archiveRow("gt-1", "created", "alice", "", `{"title":"t"}`, "2026-10-01T00:00:00Z"),
					tc.row,
					archiveRow("gt-2", "created", "alice", "", `{"title":"u"}`, "2026-10-01T00:01:00Z"),
				}),
			})
			var dropped Dropped
			rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(rows) != 2 || dropped.Count() != 1 {
				t.Fatalf("got %d rows and %d drops, want 2 rows and 1 drop: %+v", len(rows), dropped.Count(), dropped.Items)
			}
			if !strings.Contains(dropped.Items[0].Reason, tc.want) {
				t.Errorf("drop reason = %q, want it to name %q", dropped.Items[0].Reason, tc.want)
			}
		})
	}
}

func TestBeadChangesFailsUnreadableFiles(t *testing.T) {
	t.Parallel()
	fsys := eventsFS("archive", map[string][]byte{
		"events-gastown.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-1", "created", "alice", "", `{"title":"t"}`, "2026-10-01T00:00:00Z"),
		}),
		"events-broken.csv.gz": []byte("not gzip"),
		"events-.csv.gz":       eventsArchive(t, nil),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 || rows[0].Database != "gastown" {
		t.Fatalf("rows = %+v, want only gastown's: one unreadable file does not stop the others", rows)
	}
	wantFailed := []string{"archive:archive/events-.csv.gz", "archive:archive/events-broken.csv.gz"}
	if !reflect.DeepEqual(dropped.Failed, wantFailed) {
		t.Errorf("failed = %v, want %v", dropped.Failed, wantFailed)
	}
}

func TestBeadChangesFailsFileMissingColumn(t *testing.T) {
	t.Parallel()
	withoutActor := []string{"issue_id", "event_type", "old_value", "new_value", "comment", "created_at", "id"}
	fsys := eventsFS("archive", map[string][]byte{
		"events-noactor.csv.gz": eventsArchiveWithHeader(t, withoutActor, [][]string{
			{"gt-1", "created", "", `{"title":"t"}`, "", "2026-10-01T00:00:00Z", ""},
		}),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none: the file names no actor column", rows)
	}
	if len(dropped.Failed) != 1 || dropped.Failed[0] != "archive:archive/events-noactor.csv.gz" {
		t.Fatalf("failed = %v, want the file whose header lacks actor", dropped.Failed)
	}
	if !strings.Contains(dropped.Items[0].Reason, "actor") {
		t.Errorf("reason = %q, want it to name the missing column", dropped.Items[0].Reason)
	}
}

func TestBeadChangesStopsAtATruncatedFile(t *testing.T) {
	t.Parallel()
	whole := eventsArchive(t, [][]string{
		archiveRow("gt-1", "created", "alice", "", `{"title":"t"}`, "2026-10-01T00:00:00Z"),
	})
	truncated := whole[:len(whole)-8] // drop the gzip trailer
	fsys := eventsFS("archive", map[string][]byte{
		"events-gastown.csv.gz": truncated,
		"events-town.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-2", "created", "bob", "", `{"title":"u"}`, "2026-10-01T00:00:00Z"),
		}),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The rows before the truncation are complete records and still load; the
	// file itself is failed so a caller knows its tail is missing.
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want both databases' records before the truncation", rows)
	}
	want := "archive:archive/events-gastown.csv.gz"
	if !reflect.DeepEqual(dropped.Failed, []string{want}) {
		t.Fatalf("failed = %v, want %q: a truncated file is recorded", dropped.Failed, want)
	}
}

func TestBeadChangesDropsAWrongWidthRowAndKeepsReading(t *testing.T) {
	t.Parallel()
	fsys := eventsFS("archive", map[string][]byte{
		"events-gastown.csv.gz": eventsArchive(t, [][]string{
			archiveRow("gt-1", "created", "alice", "", `{"title":"t"}`, "2026-10-01T00:00:00Z"),
			{"gt-1", "updated", "alice"},
			archiveRow("gt-1", "updated", "alice", `{"description":"d"}`, `{"priority":1}`, "2026-10-01T00:01:00Z"),
		}),
	})
	var dropped Dropped
	rows, err := LoadBeadChangesArchive(fsys, "archive", &dropped)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 1 || rows[0].UpdateCount != 1 {
		t.Fatalf("rows = %+v, want the row after the short one to load", rows)
	}
	if dropped.Count() != 1 || len(dropped.Failed) != 0 {
		t.Fatalf("dropped %d items and %d failed files, want one bad row and no failed file", dropped.Count(), len(dropped.Failed))
	}
}

func TestBeadChangesRefusesUnlistableDirectory(t *testing.T) {
	t.Parallel()
	var dropped Dropped
	if _, err := LoadBeadChangesArchive(fstest.MapFS{}, "archive", &dropped); err == nil {
		t.Fatal("listing a missing archive directory returned no error, want an error")
	}
}

func TestBeadChangesRefusesNilFilesystem(t *testing.T) {
	t.Parallel()
	if _, err := LoadBeadChangesArchive(nil, "archive", nil); err == nil {
		t.Fatal("a nil filesystem returned no error, want an error")
	}
}
