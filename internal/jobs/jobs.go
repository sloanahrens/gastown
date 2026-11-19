// Package jobs lists the town's maintenance jobs: every daemon step, and the
// jobs the daemon runs inside one (the Dolt gc), with the last completion, the
// cadence and whether it is overdue. The list is read-only. It reads the daemon's
// patrol config, last-run record and maintenance state and writes nothing, calls
// no daemon and runs no bd.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// State is where a job stands against its cadence.
type State string

const (
	// StateNeverRun: the job has a cadence but no last-run entry.
	StateNeverRun State = "never_run"
	// StateOverdue: the time since the last run is past the cadence plus its grace.
	StateOverdue State = "overdue"
	// StateOK: the last run is within the cadence plus its grace.
	StateOK State = "ok"
	// StateNoCadence: the job has no interval, so nothing says when it is due.
	// It is shown amber, never green.
	StateNoCadence State = "no_cadence"
	// StateUnknown: the job's inputs cannot be read, the last-run record is
	// unreadable or corrupt, or its interval is not a Go duration. It is an
	// error row, never ok.
	StateUnknown State = "unknown"
)

// Outcome is how a job's last run ended. The empty outcome means no run is
// recorded, so nothing says how it ended.
type Outcome string

const (
	// OutcomeOK: the run finished its work.
	OutcomeOK Outcome = "ok"
	// OutcomeFailed: the run stopped with an error, or its source cannot be read.
	OutcomeFailed Outcome = "failed"
	// OutcomeRefused: the run declined to act, as its guard requires.
	OutcomeRefused Outcome = "refused"
	// OutcomeDeferred: the run found its window busy and left the work for later.
	OutcomeDeferred Outcome = "deferred"
)

// minGrace is the least slack an overdue check allows past the cadence.
const minGrace = 10 * time.Minute

// Job is one maintenance job as the list shows it.
type Job struct {
	Name string
	// Kind is the daemon step kind the job belongs to: a job that runs inside a
	// patrol, like the Dolt gc, carries that patrol's kind.
	Kind config.DaemonStepKind
	// LastRun is the job's last completion, nil when none is recorded or the
	// last-run record cannot be read.
	LastRun *time.Time
	// Cadence is the interval from the daemon's patrol config, zero when the
	// job has none.
	Cadence time.Duration
	State   State
	// Outcome is how the job's last run ended; empty when none is recorded.
	Outcome Outcome
	// Stats are the job's headline numbers, by short name.
	Stats map[string]int64
	// Source names the config the cadence was read from, empty when the town
	// has no patrol config.
	Source string
}

// List returns one Job per daemon step in config.DaemonSteps, judged at now.
// An unreadable patrol config is an error, because no cadence can be trusted.
// An unreadable last-run record is not: its rows come back as StateUnknown.
func List(townRoot string, now time.Time) ([]Job, error) {
	return list(townRoot, config.DaemonSteps(), now)
}

// list is List over the given steps, so a test can name its own.
func list(townRoot string, steps []config.DaemonStep, now time.Time) ([]Job, error) {
	cadences, source, err := readCadences(townRoot)
	if err != nil {
		return nil, err
	}
	runs, runErr := readLastRuns(townRoot)

	jobs := make([]Job, 0, len(steps))
	for _, s := range steps {
		j := Job{Name: s.Name, Kind: s.Kind, Source: source}
		if at, ok := runs[s.Name]; runErr == nil && ok && !at.IsZero() {
			j.LastRun = &at
		}
		cadence, cadErr := cadenceOf(cadences, s.Name)
		j.Cadence = cadence
		j.State = stateOf(j, runErr != nil, cadErr, now)
		jobs = append(jobs, j)
	}
	return append(jobs, doltGCRows(townRoot, now)...), nil
}

// doltGCName is the job list's row for the Dolt gc, which runs inside the
// scheduled_maintenance patrol (internal/daemon/maintenance_gc.go).
const doltGCName = "dolt_gc"

// doltGCRows returns the dolt_gc row, or none when the daemon has written no
// maintenance state: with no state file no gc is recorded. A state file that
// exists but cannot be read is a failed row, never a clean one. The row's last
// run is the newest gc across databases and its stat the total reclaimed.
func doltGCRows(townRoot string, now time.Time) []Job {
	lastGC, reclaimed, err := daemon.ReadMaintenanceGC(townRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	j := Job{
		Name:    doltGCName,
		Kind:    config.DaemonStepPatrol,
		Cadence: daemon.MaintenanceGCCadence,
	}
	if err != nil {
		j.Outcome = OutcomeFailed
		j.State = StateUnknown
		return []Job{j}
	}
	if !lastGC.IsZero() {
		j.LastRun = &lastGC
		j.Outcome = OutcomeOK
	}
	j.Stats = map[string]int64{"reclaimed_bytes": reclaimed}
	j.State = stateOf(j, false, false, now)
	return []Job{j}
}

// stateOf judges one job. An unreadable last-run record or an unreadable
// interval makes the row unknown before anything else is tried, so a broken
// input never shows as a clean state.
func stateOf(j Job, runsUnreadable, cadenceUnreadable bool, now time.Time) State {
	switch {
	case runsUnreadable, cadenceUnreadable:
		return StateUnknown
	case j.Cadence == 0:
		return StateNoCadence
	case j.LastRun == nil:
		return StateNeverRun
	}
	grace := max(j.Cadence/4, minGrace)
	if now.Sub(*j.LastRun) > j.Cadence+grace {
		return StateOverdue
	}
	return StateOK
}

// cadenceEntry is one patrol's entry in the patrol config: its interval as
// the file wrote it, or bad when the entry is not an object.
type cadenceEntry struct {
	interval *string
	bad      bool
}

// cadenceOf returns a name's cadence. An entry with no interval, or with an
// interval of zero, has cadence zero. An interval that is not a Go duration,
// or is negative, reports unreadable.
func cadenceOf(cadences map[string]cadenceEntry, name string) (time.Duration, bool) {
	e, ok := cadences[name]
	switch {
	case !ok:
		return 0, false
	case e.bad:
		return 0, true
	case e.interval == nil || *e.interval == "":
		return 0, false
	}
	d, err := time.ParseDuration(*e.interval)
	if err != nil || d < 0 {
		return 0, true
	}
	return d, false
}

// readCadences reads the daemon's patrol table, each patrol's raw entry by
// name, through config.CheckJSONFileParses so the two-file section rule
// applies. source names the file it came from, empty when the town has no
// patrol config. A patrol config that does not parse is an error.
func readCadences(townRoot string) (cadences map[string]cadenceEntry, source string, err error) {
	path := config.DaemonPatrolConfigPath(townRoot)
	// A map target declares every key, so the read takes the patrol table
	// without caring which keys a patrol's typed config has.
	var doc map[string]json.RawMessage
	if err := config.CheckJSONFileParses(path, &doc); err != nil {
		return nil, "", fmt.Errorf("read daemon patrol config: %w", err)
	}
	if doc == nil {
		return nil, "", nil
	}
	source = config.SourcePathRel(townRoot, path)

	var patrols map[string]json.RawMessage
	if raw, ok := doc["patrols"]; ok {
		if err := json.Unmarshal(raw, &patrols); err != nil {
			return nil, source, fmt.Errorf("read daemon patrol config %s: patrols is not an object: %w", source, err)
		}
	}
	cadences = make(map[string]cadenceEntry, len(patrols))
	for name, raw := range patrols {
		var entry struct {
			Interval *string `json:"interval"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			cadences[name] = cadenceEntry{bad: true}
			continue
		}
		cadences[name] = cadenceEntry{interval: entry.Interval}
	}
	return cadences, source, nil
}

// readLastRuns reads the daemon's last-run record. A missing record is no
// record, as the daemon reads it, so every cadenced job is never_run. An
// unreadable or corrupt record is returned as an error for the caller to mark.
func readLastRuns(townRoot string) (map[string]time.Time, error) {
	runs, err := daemon.ReadPatrolLastRuns(townRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return runs, err
}
