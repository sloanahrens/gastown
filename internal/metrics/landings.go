package metrics

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// landingColumns is the header landings.csv is written with, in its order.
var landingColumns = []string{
	"bead", "rig", "landed_at", "week", "route", "om_verdict", "gate_result",
	"lines_added", "lines_deleted", "lines_total", "files_touched",
	"file_entropy", "package_entropy", "attempts", "rejected_before_landing",
	"manual_rescue", "reached_overrun_stop", "unreadable", "type", "priority",
	"shaped", "label_count", "model", "om_threshold", "dominant_model",
}

// Row is one landing: the CSV's header mapped to its cells, with a cell the
// input left empty read back as "".
type Row map[string]string

// ReadLandings reads landings.csv: its first record is the header and every
// later record is a Row keyed by it, so a column this package has never heard
// of is carried rather than dropped. The header is returned as written, in its
// own order.
//
// A record whose width differs from the header is an error, not a short row:
// the extract writes every column of every landing, so a ragged record means
// the file is not the contract and its cells cannot be attributed.
//
// A CRLF pair inside a quoted cell is read as the LF that encoding/csv makes
// of a line ending inside a field; every other cell byte is kept.
func ReadLandings(r io.Reader) (header []string, rows []Row, err error) {
	cr := csv.NewReader(r)
	header, err = cr.Read()
	if err == io.EOF {
		return nil, nil, fmt.Errorf("landings.csv: empty file")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("landings.csv header: %w", err)
	}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return header, rows, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("landings.csv row %d: %w", len(rows)+2, err)
		}
		row := make(Row, len(header))
		for i, name := range header {
			row[name] = rec[i]
		}
		rows = append(rows, row)
	}
}

// WriteLandings writes landings.csv in the extract's format: the
// header, then each row's cells in header order, for a column the row does not
// carry an empty cell, and CRLF line endings.
//
// The line ending is the extract's, not Go's csv default, so a file read by
// ReadLandings and written back with no columns added is byte for byte the
// file the extract wrote, as long as no cell carries the CRLF pair ReadLandings
// folds to an LF.
func WriteLandings(w io.Writer, header []string, rows []Row) error {
	if err := writeRecord(w, header); err != nil {
		return fmt.Errorf("landings.csv header: %w", err)
	}
	rec := make([]string, len(header))
	for i, row := range rows {
		for j, name := range header {
			rec[j] = row[name]
		}
		if err := writeRecord(w, rec); err != nil {
			return fmt.Errorf("landings.csv row %d: %w", i+2, err)
		}
	}
	return nil
}

// writeRecord writes one record the way the extract's csv writer does: a cell
// carrying a comma, a quote, a CR or an LF is quoted with its quotes doubled,
// and the record ends CRLF.
//
// The cells are copied verbatim, which encoding/csv's CRLF mode does not do: it
// drops a bare CR and rewrites every LF in a cell as CRLF, so a value a
// snapshot carried would not reach the file as written (gt-yr3kn).
func writeRecord(w io.Writer, cells []string) error {
	var b strings.Builder
	for i, c := range cells {
		if i > 0 {
			b.WriteByte(',')
		}
		if strings.ContainsAny(c, ",\"\r\n") {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(c, `"`, `""`))
			b.WriteByte('"')
			continue
		}
		b.WriteString(c)
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}
