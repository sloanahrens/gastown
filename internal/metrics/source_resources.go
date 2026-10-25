package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
)

// The resources group: the CPU and memory each class and component used over
// the last hour, rolled up from the resource sampler's files. The sampler
// writes one row per sample to <town>/.runtime/metrics/resources/<UTC date>.jsonl
// and gzips a file two days after it was written. The hour can cross midnight,
// so today's file and yesterday's are read, each as its .gz sibling when the
// plain file is gone.
//
// For each class and component the group holds four columns. A cluster is
// part of the name when the row carries one. cpu_pct is of one core, as the
// sampler writes it.
//
//	res_<class>_<component>_cpu_mean   mean cpu_pct over the hour's samples
//	res_<class>_<component>_cpu_max    the largest cpu_pct
//	res_<class>_<component>_mem_mean   mean mem_bytes
//	res_<class>_<component>_mem_max    the largest mem_bytes
//
// The counts say what a nil or an absent column means:
//
//	res_rows_1h            rows of the hour that were read; 0 means the sampler
//	                       wrote nothing in it, not that nothing was measured
//	res_skipped_rows       rows of another v, and rows that are not a JSON object
//	                       with a timestamp, a class and a component
//	res_unmeasured_values  cpu_pct or mem_bytes values of the hour that are null
//	                       or not a number; they are never averaged as zero
//	res_unreadable_files   files that could not be read or decompressed, whose
//	                       rows are in no column
//
// The sampler appends while this reads, so the last segment of a file, which
// has no newline yet, is a row still being written and is not counted.
//
// When the newest sampler file was last written before the hour the snapshot
// sits in, the sampler has stopped: the group is null and an errors entry names
// the file and its last write time. An hour with no rows would otherwise read
// as an idle town instead of a dead sampler (gt-vfsi6).
type resourcesSource struct{}

// resourcesSchemaVersion is the row version this source reads; another v is
// skipped, not parsed under this layout.
const resourcesSchemaVersion = 1

// resourcesWindow is how far back the rollup reads.
const resourcesWindow = time.Hour

// resourcesStaleAfter is how long the newest sampler file may go unwritten before
// the writer counts as stopped. The sampler appends about every thirty seconds, so
// fifteen minutes of silence is a stopped writer, and a live one never crosses it
// at an hour or midnight boundary.
const resourcesStaleAfter = 15 * time.Minute

func (resourcesSource) Group() string { return "resources" }

// Collect returns an error, leaving the group null, when neither day has a
// sampler file, when the newest file has been silent for longer than
// resourcesStaleAfter, and when a file exists but its write time cannot be
// read: a snapshot that cannot tell whether the writer stopped must not report
// it healthy. A file that exists and cannot be read is counted instead.
func (resourcesSource) Collect(_ context.Context, env Env) (map[string]any, error) {
	now := env.Now().UTC()
	dir := filepath.Join(env.Town, ".runtime", "metrics", "resources")
	roll := &resourcesRollup{
		from:   now.Add(-resourcesWindow),
		to:     now,
		groups: map[string]*resourceComponent{},
	}
	found := false
	var unknown error
	newest, newestAt := "", time.Time{}
	for _, day := range []time.Time{now.AddDate(0, 0, -1), now} {
		data, path, err := readResourcesDay(env, dir, day.Format("2006-01-02"))
		if errors.Is(err, errResourcesDayMissing) {
			continue
		}
		found = true
		at, statErr := env.ModTime(path)
		if statErr != nil && unknown == nil {
			unknown = fmt.Errorf("resource sampler file %s: last write time unknown: %w", path, statErr)
		}
		if statErr == nil && (newest == "" || at.After(newestAt)) {
			newest, newestAt = path, at
		}
		if err != nil {
			roll.unreadable++
			continue
		}
		roll.addFile(data)
	}
	if !found {
		return nil, fmt.Errorf("no resource sampler file for today or yesterday under %s", dir)
	}
	if unknown != nil {
		return nil, unknown
	}
	if newest != "" && now.Sub(newestAt) > resourcesStaleAfter {
		return nil, fmt.Errorf("resource sampler file %s is stale: last written %s, more than %s before the snapshot at %s",
			newest, newestAt.UTC().Format(time.RFC3339), resourcesStaleAfter, now.UTC().Format(time.RFC3339))
	}
	return roll.columns(), nil
}

// errResourcesDayMissing is returned for a day with neither its plain file nor
// its .gz sibling: the sampler wrote nothing that day, which is not a failure.
var errResourcesDayMissing = errors.New("no resource sampler file for the day")

// readResourcesDay returns one day's rows and the file they came from: the
// plain file, or its .gz sibling when the plain file is missing. It returns
// errResourcesDayMissing when neither exists, and another error when a file
// exists but its rows cannot be read. path names the file in both cases.
func readResourcesDay(env Env, dir, date string) (data []byte, path string, err error) {
	plain := filepath.Join(dir, date+".jsonl")
	data, err = env.ReadFile(plain)
	if err == nil {
		return data, plain, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, plain, err
	}
	path = plain + ".gz"
	data, err = env.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", errResourcesDayMissing
	}
	if err != nil {
		return nil, path, err
	}
	data, err = gunzip(data)
	return data, path, err
}

// resourceStat is one measure's running mean and max. A stat with no value
// reads as nil, so a column the hour did not measure is not a zero.
type resourceStat struct {
	sum, max float64
	n        int
}

func (s *resourceStat) add(v float64) {
	if s.n == 0 || v > s.max {
		s.max = v
	}
	s.sum += v
	s.n++
}

func (s resourceStat) mean() any {
	if s.n == 0 {
		return nil
	}
	return s.sum / float64(s.n)
}

func (s resourceStat) peak() any {
	if s.n == 0 {
		return nil
	}
	return s.max
}

// resourceComponent is the CPU and memory stats of one class and component.
type resourceComponent struct {
	cpu, mem resourceStat
}

// resourcesRollup folds the hour's rows into one component per column prefix.
type resourcesRollup struct {
	from, to   time.Time
	groups     map[string]*resourceComponent
	rows       int
	skipped    int
	unmeasured int
	unreadable int
}

// addFile folds one day's file in. The last segment is dropped: the sampler
// ends every row with a newline, so a segment without one is still being
// written.
func (r *resourcesRollup) addFile(data []byte) {
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines[:len(lines)-1] {
		r.addRow(line)
	}
}

func (r *resourcesRollup) addRow(line []byte) {
	var row map[string]any
	if err := json.Unmarshal(line, &row); err != nil {
		r.skipped++
		return
	}
	if v, _ := row["v"].(float64); v != resourcesSchemaVersion {
		r.skipped++
		return
	}
	key, at, ok := resourceRowKey(row)
	if !ok {
		r.skipped++
		return
	}
	if at.Before(r.from) || at.After(r.to) {
		return
	}
	r.rows++
	comp := r.groups[key]
	if comp == nil {
		comp = &resourceComponent{}
		r.groups[key] = comp
	}
	if cpu, ok := row["cpu_pct"].(float64); ok {
		comp.cpu.add(cpu)
	} else {
		r.unmeasured++
	}
	if mem, ok := row["mem_bytes"].(float64); ok {
		comp.mem.add(mem)
	} else {
		r.unmeasured++
	}
}

// resourceRowKey returns the column prefix a row rolls up under and the time
// it was sampled. ok is false for a row with no placeable timestamp, class or
// component, or with a cluster that is not a non-empty string.
func resourceRowKey(row map[string]any) (key string, at time.Time, ok bool) {
	ts, _ := row["ts"].(string)
	at, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "", time.Time{}, false
	}
	class, _ := row["class"].(string)
	component, _ := row["component"].(string)
	if class == "" || component == "" {
		return "", time.Time{}, false
	}
	switch cluster := row["cluster"].(type) {
	case nil:
		key = "res_" + class + "_" + component
	case string:
		if cluster == "" {
			return "", time.Time{}, false
		}
		key = "res_" + cluster + "_" + class + "_" + component
	default:
		return "", time.Time{}, false
	}
	return key, at, true
}

func (r *resourcesRollup) columns() map[string]any {
	cols := map[string]any{
		"res_rows_1h":           r.rows,
		"res_skipped_rows":      r.skipped,
		"res_unmeasured_values": r.unmeasured,
		"res_unreadable_files":  r.unreadable,
	}
	for key, comp := range r.groups {
		cols[key+"_cpu_mean"] = comp.cpu.mean()
		cols[key+"_cpu_max"] = comp.cpu.peak()
		cols[key+"_mem_mean"] = comp.mem.mean()
		cols[key+"_mem_max"] = comp.mem.peak()
	}
	return cols
}
