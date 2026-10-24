package metrics

import (
	"encoding/csv"
	"fmt"
	"io"
)

// landingColumns is the contract landings.csv carries: the header the Python
// extract writes, in its order.
//
// It mirrors LANDING_COLUMNS in scripts/town_metrics/tables.py. Change both in
// one landing, or Join writes a header the extract did not.
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

// WriteLandings writes landings.csv the way the Python extract does: the
// header, then each row's cells in header order, for a column the row does not
// carry an empty cell, and CRLF line endings.
//
// The line ending is the extract's, not Go's csv default, so a file read by
// ReadLandings and written back with no columns added is byte for byte the
// file the extract wrote.
func WriteLandings(w io.Writer, header []string, rows []Row) error {
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("landings.csv header: %w", err)
	}
	rec := make([]string, len(header))
	for i, row := range rows {
		for j, name := range header {
			rec[j] = row[name]
		}
		if err := cw.Write(rec); err != nil {
			return fmt.Errorf("landings.csv row %d: %w", i+2, err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("landings.csv: %w", err)
	}
	return nil
}
