package metrics

import (
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The events archive is the Dolt events table exported for analysis: one
// events-<database>.csv.gz per database under one directory, each with a header
// and one row per event. LoadBeadChangesArchive rolls the rows up to one
// BeadChangeRow per bead, and WriteBeadChangesCSV publishes them for a dataset
// loader. A bead created before the archive's retention start has no created
// event; its creation time is then the created_at inside an updated row's
// old_value, which is the issue as it stood before that change.
//
// Every row the loader refuses is recorded in the Dropped log with its file and
// row number, so the row counts a report publishes always account for the
// archive.

const (
	eventsArchivePrefix = "events-"
	eventsArchiveSuffix = ".csv.gz"
)

// archiveEventTypes are the event_type values the rollup accepts; any other
// names a row it does not understand, which it refuses rather than count as
// nothing.
var archiveEventTypes = map[string]bool{
	"updated":          true,
	"label_added":      true,
	"label_removed":    true,
	"status_changed":   true,
	"closed":           true,
	"created":          true,
	"dependency_added": true,
	"reopened":         true,
	"claimed":          true,
	"commented":        true,
}

// archiveTextFields are the new_value keys that make an updated row a text
// update: the fields that carry the bead's prose.
var archiveTextFields = []string{"description", "design", "acceptance_criteria", "notes"}

// archiveRequiredColumns are the events columns the rollup needs. The others
// (comment, id) are carried by the export and ignored here.
var archiveRequiredColumns = []string{"issue_id", "event_type", "actor", "old_value", "new_value", "created_at"}

// BeadChangeColumns is the header WriteBeadChangesCSV writes, in column order.
var BeadChangeColumns = []string{
	"database",
	"issue_id",
	"created_at",
	"first_update_at",
	"closed_at",
	"claimed_at",
	"reopened_count",
	"update_count",
	"text_update_count",
	"label_added_count",
	"label_removed_count",
	"distinct_actor_count",
	"seconds_from_created_to_first_text_update",
	"first_filed_description",
}

// BeadChangeRow is one bead's change history from the events archive: when it
// was created, first updated, claimed and last closed, what changed, and the
// description it was filed with. A stamp the archive does not carry is empty,
// and SecondsFromCreatedToFirstTextUpdate is nil when no update changed the
// bead's text or its creation time is unknown, so neither reads as zero.
type BeadChangeRow struct {
	Database                            string
	IssueID                             string
	CreatedAt                           string
	FirstUpdateAt                       string
	ClosedAt                            string
	ClaimedAt                           string
	ReopenedCount                       int
	UpdateCount                         int
	TextUpdateCount                     int
	LabelAddedCount                     int
	LabelRemovedCount                   int
	DistinctActorCount                  int
	SecondsFromCreatedToFirstTextUpdate *int
	FirstFiledDescription               string
}

// LoadBeadChangesArchive reads every events-*.csv.gz file directly under dir of
// fsys and returns one BeadChangeRow per database and issue id, sorted by
// database then issue id. A row it cannot parse is recorded in dropped and the
// other rows still load; a file it cannot read is recorded as failed. A
// directory that cannot be listed is an error, since no rows there does not
// mean no changes were archived.
func LoadBeadChangesArchive(fsys fs.FS, dir string, dropped *Dropped) ([]BeadChangeRow, error) {
	if fsys == nil {
		return nil, errors.New("events archive: nil filesystem")
	}
	if dropped == nil {
		dropped = &Dropped{}
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("events archive: listing %s: %w", dir, err)
	}
	aggs := make(map[string]*beadChangeAgg)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, eventsArchivePrefix) || !strings.HasSuffix(name, eventsArchiveSuffix) {
			continue
		}
		filePath := path.Join(dir, name)
		database := strings.TrimSuffix(strings.TrimPrefix(name, eventsArchivePrefix), eventsArchiveSuffix)
		if database == "" {
			dropped.Fail("archive:"+filePath, "file name names no database")
			continue
		}
		readEventsArchiveFile(fsys, filePath, database, aggs, dropped)
	}
	rows := make([]BeadChangeRow, 0, len(aggs))
	for _, agg := range aggs {
		rows = append(rows, agg.finish())
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Database != rows[j].Database {
			return rows[i].Database < rows[j].Database
		}
		return rows[i].IssueID < rows[j].IssueID
	})
	return rows, nil
}

// beadChangeAgg is one bead's counts and earliest stamps while its rows are
// folded in. The text is the archive's own, so a stamp the report writes is the
// one the event carried rather than a reformatted copy.
type beadChangeAgg struct {
	database              string
	issueID               string
	createdAt             time.Time
	createdAtText         string
	fallbackCreatedAt     time.Time
	fallbackCreatedAtText string
	firstUpdateAt         time.Time
	firstUpdateAtText     string
	closedAt              time.Time
	closedAtText          string
	claimedAt             time.Time
	claimedAtText         string
	firstTextUpdateAt     time.Time
	descriptionAt         time.Time
	descriptionText       string
	descriptionSet        bool
	reopenedCount         int
	updateCount           int
	textUpdateCount       int
	labelAddedCount       int
	labelRemovedCount     int
	actors                map[string]struct{}
}

// finish renders the aggregate as the row the archive publishes.
func (a *beadChangeAgg) finish() BeadChangeRow {
	row := BeadChangeRow{
		Database:              a.database,
		IssueID:               a.issueID,
		CreatedAt:             a.createdAtText,
		FirstUpdateAt:         a.firstUpdateAtText,
		ClosedAt:              a.closedAtText,
		ClaimedAt:             a.claimedAtText,
		ReopenedCount:         a.reopenedCount,
		UpdateCount:           a.updateCount,
		TextUpdateCount:       a.textUpdateCount,
		LabelAddedCount:       a.labelAddedCount,
		LabelRemovedCount:     a.labelRemovedCount,
		DistinctActorCount:    len(a.actors),
		FirstFiledDescription: a.descriptionText,
	}
	created := a.createdAt
	if created.IsZero() {
		created = a.fallbackCreatedAt
		row.CreatedAt = a.fallbackCreatedAtText
	}
	if !created.IsZero() && !a.firstTextUpdateAt.IsZero() {
		seconds := int(a.firstTextUpdateAt.Sub(created) / time.Second)
		row.SecondsFromCreatedToFirstTextUpdate = &seconds
	}
	return row
}

// readEventsArchiveFile reads one gzip CSV and folds its rows into aggs.
func readEventsArchiveFile(fsys fs.FS, path, database string, aggs map[string]*beadChangeAgg, dropped *Dropped) {
	source := "archive:" + path
	f, err := fsys.Open(path)
	if err != nil {
		dropped.Fail(source, fmt.Sprintf("opening: %v", err))
		return
	}
	// The file is only read, and a gzip checksum failure surfaces from Read at
	// the end of the stream, not from either Close.
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		dropped.Fail(source, fmt.Sprintf("not gzip: %v", err))
		return
	}
	defer func() { _ = zr.Close() }()
	readEventsArchiveCSV(csv.NewReader(zr), source, database, aggs, dropped)
}

// readEventsArchiveCSV reads the header and every record of one archive file.
func readEventsArchiveCSV(r *csv.Reader, source, database string, aggs map[string]*beadChangeAgg, dropped *Dropped) {
	header, err := r.Read()
	if err != nil {
		dropped.Fail(source, fmt.Sprintf("header: %v", err))
		return
	}
	columns := make(map[string]int, len(header))
	for i, name := range header {
		columns[strings.TrimSpace(name)] = i
	}
	var missing []string
	for _, name := range archiveRequiredColumns {
		if _, ok := columns[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		dropped.Fail(source, "header is missing columns: "+strings.Join(missing, ", "))
		return
	}
	for n := 2; ; n++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			// A record of the wrong width is one bad row and the reader moves
			// on. Any other read error — a truncated gzip, an unreadable
			// stream — leaves the reader at the same error for every later
			// call, so the file stops there and is recorded as failed rather
			// than looped over.
			if errors.Is(err, csv.ErrFieldCount) {
				dropped.Add(source, fmt.Sprintf("row %d: %v", n, err))
				continue
			}
			dropped.Fail(source, fmt.Sprintf("row %d: %v; later rows not read", n, err))
			return
		}
		ev, err := parseArchiveEvent(rec, columns)
		if err != nil {
			dropped.Add(source, fmt.Sprintf("row %d: %v", n, err))
			continue
		}
		agg := aggs[database+"\x00"+ev.issue]
		if agg == nil {
			agg = &beadChangeAgg{database: database, issueID: ev.issue, actors: map[string]struct{}{}}
			aggs[database+"\x00"+ev.issue] = agg
		}
		agg.apply(ev)
	}
}

// archiveEvent is one events row as the rollup reads it: the fields it counts
// and, for an updated row, the changed fields the new_value object names and
// the issue the old_value object describes.
type archiveEvent struct {
	issue   string
	kind    string
	actor   string
	at      time.Time
	atText  string
	changed map[string]json.RawMessage
	old     map[string]json.RawMessage
}

// parseArchiveEvent validates one CSV record and decodes the JSON the rollup
// needs from it. A record outside the archive's shape is an error naming the
// field, so a Dropped entry says which part failed.
func parseArchiveEvent(rec []string, columns map[string]int) (archiveEvent, error) {
	cell := func(name string) string {
		i := columns[name]
		if i >= len(rec) {
			return ""
		}
		return rec[i]
	}
	ev := archiveEvent{
		issue:  strings.TrimSpace(cell("issue_id")),
		kind:   strings.TrimSpace(cell("event_type")),
		actor:  strings.TrimSpace(cell("actor")),
		atText: strings.TrimSpace(cell("created_at")),
	}
	if ev.issue == "" {
		return archiveEvent{}, errors.New("empty issue_id")
	}
	if !archiveEventTypes[ev.kind] {
		return archiveEvent{}, fmt.Errorf("unknown event_type %q", ev.kind)
	}
	at, err := parseArchiveTimestamp(ev.atText)
	if err != nil {
		return archiveEvent{}, fmt.Errorf("created_at %q: %w", ev.atText, err)
	}
	ev.at = at
	if ev.kind != "updated" {
		return ev, nil
	}
	newValue := cell("new_value")
	if err := json.Unmarshal([]byte(newValue), &ev.changed); err != nil || ev.changed == nil {
		// A JSON null decodes without error but names no changed fields, so it
		// is not the object the archive writes for an update.
		return archiveEvent{}, errors.New("new_value is not a JSON object")
	}
	if oldValue := strings.TrimSpace(cell("old_value")); oldValue != "" {
		if err := json.Unmarshal([]byte(oldValue), &ev.old); err != nil || ev.old == nil {
			return archiveEvent{}, errors.New("old_value is not a JSON object")
		}
		// Both fields are read as strings: description becomes the row's filed
		// description, created_at the creation stamp when no created event
		// exists. A value of another JSON type is corrupt, not empty.
		for _, name := range []string{"description", "created_at"} {
			raw, ok := ev.old[name]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return archiveEvent{}, fmt.Errorf("old_value.%s is not a string", name)
			}
		}
	}
	return ev, nil
}

// parseArchiveTimestamp parses the export's date form — a Dolt DATETIME or an
// ISO 8601 stamp — as UTC when it carries no zone.
func parseArchiveTimestamp(ts string) (time.Time, error) {
	if t, err := ParseTS(ts); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, ts, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("invalid timestamp")
}

// apply folds ev into the aggregate: counts, the earliest stamp of each kind,
// and the latest close, since a bead closed and reopened is closed at its last
// close. An empty actor names no one and is left out of the distinct count.
func (a *beadChangeAgg) apply(ev archiveEvent) {
	if ev.actor != "" {
		a.actors[ev.actor] = struct{}{}
	}
	switch ev.kind {
	case "created":
		if a.createdAtText == "" || ev.at.Before(a.createdAt) {
			a.createdAt = ev.at
			a.createdAtText = ev.atText
		}
	case "updated":
		a.updateCount++
		if a.firstUpdateAtText == "" || ev.at.Before(a.firstUpdateAt) {
			a.firstUpdateAt = ev.at
			a.firstUpdateAtText = ev.atText
		}
		if archiveHasTextField(ev.changed) {
			a.textUpdateCount++
			if a.firstTextUpdateAt.IsZero() || ev.at.Before(a.firstTextUpdateAt) {
				a.firstTextUpdateAt = ev.at
			}
		}
		if _, changesDescription := ev.changed["description"]; changesDescription {
			if !a.descriptionSet || ev.at.Before(a.descriptionAt) {
				a.descriptionSet = true
				a.descriptionAt = ev.at
				a.descriptionText = archiveStringField(ev.old, "description")
			}
		}
		if a.fallbackCreatedAtText == "" && ev.old != nil {
			if created := archiveStringField(ev.old, "created_at"); created != "" {
				if at, err := parseArchiveTimestamp(created); err == nil {
					a.fallbackCreatedAt = at
					a.fallbackCreatedAtText = created
				}
			}
		}
	case "claimed":
		if a.claimedAtText == "" || ev.at.Before(a.claimedAt) {
			a.claimedAt = ev.at
			a.claimedAtText = ev.atText
		}
	case "closed":
		if a.closedAtText == "" || ev.at.After(a.closedAt) {
			a.closedAt = ev.at
			a.closedAtText = ev.atText
		}
	case "reopened":
		a.reopenedCount++
	case "label_added":
		a.labelAddedCount++
	case "label_removed":
		a.labelRemovedCount++
	}
}

// archiveHasTextField reports whether the changed fields name one of the bead's
// prose fields.
func archiveHasTextField(fields map[string]json.RawMessage) bool {
	for _, name := range archiveTextFields {
		if _, ok := fields[name]; ok {
			return true
		}
	}
	return false
}

// archiveStringField returns the named field of a decoded JSON object as a
// string, empty when the field is absent or not a JSON string.
func archiveStringField(fields map[string]json.RawMessage, name string) string {
	raw, ok := fields[name]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// WriteBeadChangesCSV writes rows to w as a CSV with one header row, ordered by
// database then issue id and then by the remaining cells, so the same rows in
// any input order produce byte-identical output. A bead text cell is written as
// the archive carried it, quoted when it holds a comma, a quote or a newline.
func WriteBeadChangesCSV(w io.Writer, rows []BeadChangeRow) error {
	sorted := append([]BeadChangeRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return beadChangeLess(sorted[i], sorted[j]) })
	if err := writeRecord(w, BeadChangeColumns); err != nil {
		return fmt.Errorf("bead changes csv header: %w", err)
	}
	for i, row := range sorted {
		if err := writeRecord(w, beadChangeCells(row)); err != nil {
			return fmt.Errorf("bead changes csv row %d: %w", i+2, err)
		}
	}
	return nil
}

// beadChangeCells returns row's cells in BeadChangeColumns order.
func beadChangeCells(row BeadChangeRow) []string {
	seconds := ""
	if row.SecondsFromCreatedToFirstTextUpdate != nil {
		seconds = strconv.Itoa(*row.SecondsFromCreatedToFirstTextUpdate)
	}
	return []string{
		row.Database,
		row.IssueID,
		row.CreatedAt,
		row.FirstUpdateAt,
		row.ClosedAt,
		row.ClaimedAt,
		strconv.Itoa(row.ReopenedCount),
		strconv.Itoa(row.UpdateCount),
		strconv.Itoa(row.TextUpdateCount),
		strconv.Itoa(row.LabelAddedCount),
		strconv.Itoa(row.LabelRemovedCount),
		strconv.Itoa(row.DistinctActorCount),
		seconds,
		row.FirstFiledDescription,
	}
}

// beadChangeLess orders rows by database and issue id; rows that agree on both
// are ordered by every cell, so a repeated row cannot make the output depend on
// the input order.
func beadChangeLess(a, b BeadChangeRow) bool {
	ca, cb := beadChangeCells(a), beadChangeCells(b)
	for i := range ca {
		if ca[i] != cb[i] {
			return ca[i] < cb[i]
		}
	}
	return false
}
