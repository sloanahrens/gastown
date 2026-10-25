package metrics

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"path/filepath"
	"slices"
	"time"
)

// peakFields is the contract between a peak row the sampler writes and the
// landing column it fills. The key is the field in scripts/resource-sampler.py's
// PEAK_FIELDS; the column is the name the joined landings carry. A column is
// never renamed.
var peakFields = []struct{ key, column string }{
	{"peak_mem_bytes", "sandbox_peak_mem_bytes"},
	{"peak_cpu_pct", "sandbox_peak_cpu_pct"},
	{"mem_limit_bytes", "sandbox_mem_limit_bytes"},
}

// peaksFile is the sampler's per-bead peak rows, one JSON object per line,
// under the town's resources directory. The sampler does not gzip this file,
// but a .gz sibling is read too, as the older rows, in case one is written.
const peaksFile = "bead-peaks.jsonl"

// Peak is one sandbox row of the sampler: the cells it contributes to the
// landing of its bead, and when its sandbox ended, which picks the bead's
// latest row. ended is false for a row whose ended_at is absent or unparseable;
// such a row still contributes its cells, and is only passed over for a row
// that can be placed.
type Peak struct {
	bead    string
	endedAt time.Time
	ended   bool
	cells   map[string]string
}

// ReadPeaks reads the sampler's peak rows from the town's resources directory:
// the .gz sibling first, then the plain file, each row in file order. No file
// at all is no peaks, not an error.
//
// A .gz sibling holds the sampler's rotated older rows, so one that cannot be
// read or decompressed costs those rows only and the join still runs. The skip
// is reported on the process log with the file and its error, so a damaged
// sibling is not mistaken for beads with no rows, and every skipped file is
// returned by name so the analysis receipt can name the loss on its own output.
// A read error on the plain file is returned: that is the file the sampler
// writes, so its loss is not one the join can carry past.
//
// A line that is not a JSON object is skipped, as is a row with no bead: the
// sampler appends while this reads, so the last line may be partial, and a row
// with no bead names no landing. Unknown keys are ignored.
func ReadPeaks(env Env) ([]Peak, []string, error) {
	return readPeaks(env, log.Printf)
}

// readPeaks is ReadPeaks taking the report's sink, so a test sees a skip without
// the process log.
func readPeaks(env Env, reportf func(format string, args ...any)) ([]Peak, []string, error) {
	dir := filepath.Join(env.Town, ".runtime", "metrics", "resources")
	var peaks []Peak
	var skipped []string
	for _, name := range []string{peaksFile + ".gz", peaksFile} {
		compressed := filepath.Ext(name) == ".gz"
		data, err := env.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil && compressed {
			data, err = gunzip(data)
		}
		if err != nil {
			if !compressed {
				return nil, nil, fmt.Errorf("%s: %w", name, err)
			}
			reportf("metrics: %s: %v; its rows are skipped, not the join", name, err)
			skipped = append(skipped, name)
			continue
		}
		peaks = append(peaks, parsePeaks(data)...)
	}
	return peaks, skipped, nil
}

// gunzip returns the decompressed bytes of a gzip file.
func gunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	return out, zr.Close()
}

// parsePeaks reads one peak per line, in file order. A number is kept as the
// text the sampler wrote, so a byte count beyond float64's exact range is not
// rounded on the way into the CSV.
func parsePeaks(data []byte) []Peak {
	var peaks []Peak
	for _, line := range bytes.Split(data, []byte("\n")) {
		var fields map[string]any
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&fields); err != nil {
			continue
		}
		bead, _ := fields["bead"].(string)
		if bead == "" {
			continue
		}
		ended, _ := fields["ended_at"].(string)
		endedAt, err := time.Parse(time.RFC3339, ended)
		cells := make(map[string]string, len(peakFields))
		for _, f := range peakFields {
			cells[f.column] = cell(fields[f.key])
		}
		peaks = append(peaks, Peak{bead: bead, endedAt: endedAt, ended: err == nil, cells: cells})
	}
	return peaks
}

// JoinPeaks adds the sandbox peak columns to every landing: the peak of the
// bead's latest sandbox row, matched on the bead id exactly as the extract
// wrote it, never on a branch name. A landing whose bead has no row keeps
// every peak cell empty. The daily analysis calls it over the rows Join
// produced, so the columns reach the published landings.
//
// With no peak rows there is nothing to join, and the header and rows come back
// unchanged, so the output is the join's own. Otherwise the peak columns are
// appended to the header in peakFields order, unless the header already has
// them, and the rows are copies with those cells set.
func JoinPeaks(header []string, rows []Row, peaks []Peak) ([]string, []Row) {
	if len(peaks) == 0 {
		return header, rows
	}
	latest := latestPeaks(peaks)
	header = slices.Clone(header)
	for _, f := range peakFields {
		if !slices.Contains(header, f.column) {
			header = append(header, f.column)
		}
	}
	joined := make([]Row, len(rows))
	for i, row := range rows {
		out := make(Row, len(row)+len(peakFields))
		for name, value := range row {
			out[name] = value
		}
		peak, ok := latest[row["bead"]]
		for _, f := range peakFields {
			out[f.column] = ""
			if ok {
				out[f.column] = peak.cells[f.column]
			}
		}
		joined[i] = out
	}
	return header, joined
}

// latestPeaks keeps each bead's row whose sandbox ended last, and a row with no
// end time only when the bead has no row that has one. A tie goes to the row
// read later, which is the one the sampler appended later.
func latestPeaks(peaks []Peak) map[string]Peak {
	latest := make(map[string]Peak, len(peaks))
	for _, p := range peaks {
		if prev, ok := latest[p.bead]; ok && !p.supersedes(prev) {
			continue
		}
		latest[p.bead] = p
	}
	return latest
}

// supersedes reports whether p is the row the join keeps over prev: a row with
// an end time beats one without, and otherwise the row read later wins.
func (p Peak) supersedes(prev Peak) bool {
	if p.ended != prev.ended {
		return p.ended
	}
	return !p.ended || !p.endedAt.Before(prev.endedAt)
}
