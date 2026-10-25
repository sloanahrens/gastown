package metrics

import (
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Every column Join adds carries the snap_ prefix, so a snapshot cell is
// distinguishable from one the extract wrote and a second Join overwrites
// rather than duplicates them.
const (
	snapPrefix  = "snap_"
	placePrefix = "snap_place_"
	ageColumn   = "snap_age_min"
)

// Coverage says how much of the joined table the snapshots actually filled,
// and how many of the snapshots could not be indexed at all.
type Coverage struct {
	// Cells is the number of snap_ cells across the joined landings.
	Cells int
	// Filled is how many of those cells carry a value.
	Filled int
	// Pct is Filled as a percent of Cells, and 0 when there are no cells.
	Pct float64
	// Skipped is how many of the given snapshots had a ts that does not
	// parse, so they neither matched a landing nor named a column. It is how
	// a caller tells a snapshot log it could not read from an empty result.
	Skipped int
}

// Join appends to every landing the snapshot in force when it landed: the
// latest snapshot taken no more than maxAge before the landing's landed_at,
// never one taken after it. Snapshot groups become snap_<group>_<column>,
// placement becomes snap_place_<component>, and the lag becomes snap_age_min;
// a landing with no snapshot in force keeps every snap_ cell empty.
//
// The output header is columns in their order, then every other column the
// rows or the snapshots name, sorted, so it is the same for every call with
// the same inputs. With no snapshot to join (none given, or none with a ts
// that parses), Join adds no column of its own and no cell changes, whatever
// header the input carried.
func Join(columns []string, rows []Row, snaps []Record, maxAge time.Duration) (header []string, joined []Row, cov Coverage) {
	flat := flattenSnapshots(snaps)
	header = joinHeader(columns, rows, flat)
	joined = make([]Row, len(rows))
	for i, row := range rows {
		joined[i] = joinedRow(row, header, flat, maxAge)
	}
	cov = coverage(joined, header)
	cov.Skipped = len(snaps) - len(flat)
	return header, joined, cov
}

// flatSnap is one snapshot as the cells it contributes, flattened once for
// every landing it may be joined to. The age is absent because it belongs to
// the landing, not the snapshot.
type flatSnap struct {
	at    time.Time
	cells map[string]string
}

// flattenSnapshots indexes the snapshots Join may match, oldest first. A
// record whose ts does not parse has no place in time, so it is neither
// matched nor allowed to name columns. A null group names no columns either,
// so its cells come out empty wherever another snapshot defines them.
func flattenSnapshots(snaps []Record) []flatSnap {
	flat := make([]flatSnap, 0, len(snaps))
	for _, rec := range snaps {
		at, err := time.Parse(time.RFC3339, rec.TS)
		if err != nil {
			continue
		}
		flat = append(flat, flatSnap{at: at, cells: flattenCells(rec)})
	}
	sort.SliceStable(flat, func(i, j int) bool { return flat[i].at.Before(flat[j].at) })
	return flat
}

// flattenCells is one snapshot's cells keyed by output column. Two cells can
// flatten to one name: a group named place reaches into the placement
// namespace, and an underscore inside a group or column name is
// indistinguishable from the separator. Placement is written first and the
// groups in sorted order, each keeping a name an earlier one took, so every
// run on one snapshot resolves the collision the same way.
func flattenCells(rec Record) map[string]string {
	cells := make(map[string]string, len(rec.Groups)+len(rec.Placement))
	for component, place := range rec.Placement {
		cells[placePrefix+component] = cell(place)
	}
	for _, group := range slices.Sorted(maps.Keys(rec.Groups)) {
		cols := rec.Groups[group]
		for _, name := range slices.Sorted(maps.Keys(cols)) {
			key := snapPrefix + group + "_" + name
			if _, taken := cells[key]; taken {
				continue
			}
			cells[key] = cell(cols[name])
		}
	}
	return cells
}

// joinHeader lists the output's columns: the input's own header in its order,
// then every column the rows or the snapshots name that it does not. The tail
// is sorted, and a column outside the header still travels because a Row is
// the only record that the extract wrote it.
func joinHeader(columns []string, rows []Row, flat []flatSnap) []string {
	header := make([]string, 0, len(columns)+16)
	seen := make(map[string]bool, len(columns)+16)
	for _, name := range columns {
		header = append(header, name)
		seen[name] = true
	}
	var tail []string
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		tail = append(tail, name)
	}
	for _, row := range rows {
		for name := range row {
			add(name)
		}
	}
	for _, snap := range flat {
		for name := range snap.cells {
			add(name)
		}
	}
	if len(flat) > 0 {
		add(ageColumn)
	}
	sort.Strings(tail)
	return append(header, tail...)
}

// joinedRow is one landing with its snapshot cells filled. A snap_ cell is
// always written by Join, so a value an earlier join left in the input is
// replaced rather than reported as this snapshot's.
func joinedRow(row Row, header []string, flat []flatSnap, maxAge time.Duration) Row {
	out := make(Row, len(header))
	for _, name := range header {
		if strings.HasPrefix(name, snapPrefix) && len(flat) > 0 {
			out[name] = ""
			continue
		}
		out[name] = row[name]
	}
	if len(flat) == 0 {
		return out
	}
	at, err := time.Parse(time.RFC3339, row["landed_at"])
	if err != nil {
		return out
	}
	// Every snapshot older than the landing is a candidate; the last of them
	// is the one in force. The nearest-after is never considered, so a landing
	// before the first snapshot stays unmatched rather than borrowing it.
	i := sort.Search(len(flat), func(i int) bool { return flat[i].at.After(at) })
	if i == 0 {
		return out
	}
	snap := flat[i-1]
	age := at.Sub(snap.at)
	if age > maxAge {
		return out
	}
	for name, value := range snap.cells {
		out[name] = value
	}
	out[ageColumn] = cell(age.Minutes())
	return out
}

// coverage counts the snap_ cells Join wrote and how many of them have a
// value, so a caller can say how much of the joined table is snapshot data.
func coverage(joined []Row, header []string) Coverage {
	var cov Coverage
	for _, name := range header {
		if !strings.HasPrefix(name, snapPrefix) {
			continue
		}
		cov.Cells += len(joined)
		for _, row := range joined {
			if row[name] != "" {
				cov.Filled++
			}
		}
	}
	if cov.Cells > 0 {
		cov.Pct = 100 * float64(cov.Filled) / float64(cov.Cells)
	}
	return cov
}

// cell renders one snapshot value as a CSV cell: nothing for a null value,
// a plain decimal for a number, and JSON for a shape the table has no column
// for rather than an error.
func cell(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}
