package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The town's intervention record: one line per time a person or the overseer
// stepped into the loop, so the weekly report can count how often the loop needs
// a person, and for what, beside its rework figures. gt intervention record
// appends the lines and CountInterventions reads them. The record is
// append-only and shares .runtime/metrics with the regime events.

// interventionsJSONLName is the record's file name under regimeEventsRuntimeDir.
const interventionsJSONLName = "interventions.jsonl"

// InterventionKinds are the kinds an intervention may be recorded as: what a
// person did that the loop could not. The writer refuses any other, and the
// reader counts a line carrying one as unparseable rather than as an
// intervention.
var InterventionKinds = []string{
	"waiver", "takeover", "handfix", "hold", "restart", "rescue_land", "other",
}

// interventionKindValid reports whether kind is one of InterventionKinds.
func interventionKindValid(kind string) bool {
	for _, known := range InterventionKinds {
		if kind == known {
			return true
		}
	}
	return false
}

// interventionBeadOptional reports whether kind may be recorded without a bead:
// a hold and a restart can be about a rig or the town rather than one bead.
func interventionBeadOptional(kind string) bool {
	return kind == "hold" || kind == "restart"
}

// InterventionsPath is the town's intervention record.
func InterventionsPath(town string) string {
	return filepath.Join(town, regimeEventsRuntimeDir, interventionsJSONLName)
}

// InterventionRecord is one line of the record: when the intervention happened,
// what kind it was, the bead and rig it concerned, why it was needed, and who
// made it. Bead is empty for a hold or restart recorded without one, and Rig is
// empty for a line that names no rig: a beadless hold or restart, or a bead that
// routes to the town rather than a rig.
type InterventionRecord struct {
	TS     time.Time `json:"ts"`
	Kind   string    `json:"kind"`
	Bead   string    `json:"bead"`
	Rig    string    `json:"rig"`
	Reason string    `json:"reason"`
	Actor  string    `json:"actor"`
}

// Validate reports what the record lacks: a known kind, an instant, a reason
// and an actor are always needed, and a bead for every kind but hold and
// restart. The writer, the reader and a command that resolves a bead's rig
// before writing share it, so a line the reader counts as unparseable is never
// one the writer produced.
func (r InterventionRecord) Validate() error {
	kind := strings.TrimSpace(r.Kind)
	switch {
	case r.TS.IsZero():
		return errors.New("no timestamp")
	case !interventionKindValid(kind):
		return fmt.Errorf("unknown kind %q, want one of %s", kind, strings.Join(InterventionKinds, ", "))
	case strings.TrimSpace(r.Reason) == "":
		return errors.New("empty reason")
	case strings.TrimSpace(r.Actor) == "":
		return errors.New("empty actor")
	case strings.TrimSpace(r.Bead) == "" && !interventionBeadOptional(kind):
		return fmt.Errorf("kind %q needs a bead", kind)
	}
	return nil
}

// interventionFile is what the append needs of the file it writes: one write
// and a close.
type interventionFile interface {
	Write([]byte) (int, error)
	Close() error
}

// interventionAppend is the record's way to the file: it encodes the line and
// opens the file. The production values are json.Marshal and os.OpenFile. A
// record's line is strings and a timestamp, so neither fails on a healthy host;
// the struct exists so a test drives the encoding, write, short-write and close
// failures a host cannot be made to produce.
type interventionAppend struct {
	encode func(any) ([]byte, error)
	open   func(name string, flag int, perm os.FileMode) (interventionFile, error)
}

// hostInterventionAppend is the process's own encoder and filesystem.
func hostInterventionAppend() interventionAppend {
	return interventionAppend{encode: json.Marshal, open: openInterventionFile}
}

// openInterventionFile opens the record file for appending.
func openInterventionFile(name string, flag int, perm os.FileMode) (interventionFile, error) {
	return os.OpenFile(name, flag, perm)
}

// RecordIntervention appends ev to the intervention record at path as one line,
// creating the file 0600 and its directory when they are missing. The append is
// one O_APPEND write of one line, so a concurrent record cannot interleave with
// it. An invalid record is refused and nothing is written.
func RecordIntervention(path string, ev InterventionRecord) error {
	return appendIntervention(path, ev, hostInterventionAppend())
}

// appendIntervention is RecordIntervention through seams. A failed write still
// closes the file, and a close failure is an error so a record the host did not
// keep is not reported as written.
func appendIntervention(path string, ev InterventionRecord, seams interventionAppend) error {
	ev.TS = ev.TS.UTC()
	ev.Kind = strings.TrimSpace(ev.Kind)
	ev.Bead = strings.TrimSpace(ev.Bead)
	ev.Rig = strings.TrimSpace(ev.Rig)
	ev.Reason = strings.TrimSpace(ev.Reason)
	ev.Actor = strings.TrimSpace(ev.Actor)
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("intervention: %w", err)
	}
	line, err := seams.encode(ev)
	if err != nil {
		return fmt.Errorf("intervention: encoding: %w", err)
	}
	line = append(line, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("intervention: creating %s: %w", dir, err)
	}
	f, err := seams.open(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("intervention: opening %s: %w", path, err)
	}
	n, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("intervention: appending to %s: %w", path, werr)
	}
	if n != len(line) {
		return fmt.Errorf("intervention: appending to %s: %w", path, io.ErrShortWrite)
	}
	if cerr != nil {
		return fmt.Errorf("intervention: closing %s: %w", path, cerr)
	}
	return nil
}

// InterventionCount is how many interventions of one kind, in one rig, fell in
// one week. Week is the Monday the week began on, as YYYY-MM-DD in UTC. Rig is
// empty for a line that names no rig: a hold or restart recorded without a bead.
type InterventionCount struct {
	Week  string
	Rig   string
	Kind  string
	Count int
}

// InterventionReport is CountInterventions's reading of the record: one count
// per week, rig and kind, and how many lines it could not read.
type InterventionReport struct {
	Counts      []InterventionCount
	Unparseable int
}

// CountInterventions counts the record's lines per week by kind and rig. A line
// that is not JSON, lacks a field a record needs, or names an unknown kind is
// counted in Unparseable rather than as an intervention, so a bad line never
// becomes a zero count. A record dated after now is not counted: it has not
// happened yet at the instant asked about. Weeks begin on Monday, in UTC.
func CountInterventions(lines []string, now time.Time) InterventionReport {
	var report InterventionReport
	type key struct{ week, rig, kind string }
	counts := make(map[key]int, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec InterventionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			report.Unparseable++
			continue
		}
		if err := rec.Validate(); err != nil {
			report.Unparseable++
			continue
		}
		if rec.TS.After(now) {
			continue
		}
		counts[key{weekOf(rec.TS.UTC()), strings.TrimSpace(rec.Rig), strings.TrimSpace(rec.Kind)}]++
	}
	report.Counts = make([]InterventionCount, 0, len(counts))
	for k, n := range counts {
		report.Counts = append(report.Counts, InterventionCount{Week: k.week, Rig: k.rig, Kind: k.kind, Count: n})
	}
	sort.Slice(report.Counts, func(i, j int) bool {
		a, b := report.Counts[i], report.Counts[j]
		if a.Week != b.Week {
			return a.Week < b.Week
		}
		if a.Rig != b.Rig {
			return a.Rig < b.Rig
		}
		return a.Kind < b.Kind
	})
	return report
}
