// Package jobs lists the town's maintenance jobs: every daemon step, the jobs
// the daemon runs inside one (the Dolt gc), the nightly Dolt backup and each
// node's disk reading, with the last completion, the outcome, the cadence, the
// due time and whether it is overdue. The list is read-only. It reads the
// daemon's patrol config, last-run record, maintenance state, update history,
// backup root, node disk record, disk maintenance ledger and run ledger, and
// writes nothing, calls no daemon and runs no bd.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/jobledger"
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
	Name string `json:"name"`
	// Kind is the daemon step kind the job belongs to: a job that runs inside a
	// patrol, like the Dolt gc, carries that patrol's kind.
	Kind config.DaemonStepKind `json:"kind,omitempty"`
	// LastRun is the job's last completion, nil when none is recorded or the
	// job's own reader cannot read it.
	LastRun *time.Time `json:"last_run,omitempty"`
	// Cadence is the interval from the daemon's patrol config, zero when the
	// job has none. JSON omits it: it is a Go duration, and no reader of the
	// array reads it.
	Cadence time.Duration `json:"-"`
	// Due is when the job is next due: the newest run's own next-due time when
	// the ledger recorded one, else LastRun plus the cadence and its grace. Nil
	// when nothing says when the job is due.
	Due   *time.Time `json:"due,omitempty"`
	State State      `json:"state"`
	// Outcome is how the last run ended, empty when no run is recorded or the
	// row's reader has no outcome for it.
	Outcome Outcome `json:"outcome,omitempty"`
	// Stats are the row's headline numbers, keyed by the name the dashboard
	// reads (duration_ms, databases, reclaimed_bytes): the row's own reader's,
	// with the newest ledger run's laid over them. Nil when neither has any.
	Stats map[string]int64 `json:"stats,omitempty"`
	// Runs are the job's newest runs from the run ledger, oldest first, at most
	// ledgerRunCap. Nil when the ledger holds none.
	Runs []jobledger.Run `json:"runs,omitempty"`
	// Source names the config the cadence was read from, empty when the
	// cadence is not read from a config file.
	Source string `json:"source,omitempty"`
}

// Options are the list's inputs that do not come from the town root.
type Options struct {
	// BackupRoot is the nightly Dolt backup root: the value
	// doltbackup.DefaultRoot returns, which is the path the daemon's
	// scheduled_maintenance passes to doltbackup.Commit. Empty means the
	// caller did not resolve it, and the dolt_backup row is unknown.
	BackupRoot string
	// diskmaintFS is the filesystem the disk maintenance ledger is read
	// through, rooted at the town root. A nil field reads the town root itself;
	// a test hands it an fstest.MapFS, which cannot be written.
	diskmaintFS fs.FS
}

// diskmaintLedgerFS returns the filesystem the disk maintenance ledger is read
// through: the caller's when it named one, else the town root's.
func (o Options) diskmaintLedgerFS(townRoot string) fs.FS {
	if o.diskmaintFS != nil {
		return o.diskmaintFS
	}
	return os.DirFS(townRoot)
}

// List returns one Job per daemon step in config.DaemonSteps, one for the
// nightly Dolt backup, the Dolt gc row when the daemon has recorded one, one
// per node in the node disk record and one per target in the disk maintenance
// ledger, judged at now. A row named in the run ledger
// takes its newest run's outcome, stats and time and its recent runs from there.
// An unreadable patrol config is an error, because no cadence can be trusted, as
// is an empty town root. An unreadable last-run record, update history, backup
// root, maintenance state, node disk record, disk maintenance ledger or run
// ledger is not: its rows come back as StateUnknown.
func List(townRoot string, opts Options, now time.Time) ([]Job, error) {
	return list(townRoot, opts, config.DaemonSteps(), now)
}

// list is List over the given steps, so a test can name its own.
func list(townRoot string, opts Options, steps []config.DaemonStep, now time.Time) ([]Job, error) {
	cadences, source, err := readCadences(townRoot)
	if err != nil {
		return nil, err
	}
	runs, runErr := readLastRuns(townRoot)
	history, historyErr := updateHistory(townRoot)
	backup, backupNight, backupErr := newestBackupNight(opts.BackupRoot)

	jobs := make([]Job, 0, len(steps)+2)
	for _, s := range steps {
		j := Job{Name: s.Name, Kind: s.Kind, Source: source}
		cadence, cadErr := cadenceOf(cadences, s.Name)
		j.Cadence = cadence
		if s.Name == updateJobName {
			// The update patrol records its runs in history.jsonl, so that
			// file, not the daemon's last-run record, says when it last ran.
			j.Cadence = nightlyCadence
			j.Source = ""
			j.State = applyUpdateRun(&j, history, historyErr, now)
		} else {
			if at, ok := runs[s.Name]; runErr == nil && ok && !at.IsZero() {
				j.LastRun = &at
			}
			j.State = stateOf(j, runErr != nil, cadErr, now)
		}
		jobs = append(jobs, j)
	}
	jobs = append(jobs, doltBackupRow(backup, backupNight, backupErr, now))
	jobs = append(jobs, doltGCRows(townRoot, now)...)
	jobs = append(jobs, nodeDiskRows(townRoot, now)...)
	jobs = append(jobs, diskmaintRows(opts.diskmaintLedgerFS(townRoot), now)...)
	return withLedger(townRoot, jobs, now)
}

// ledgerRunCap is how many of a job's runs the list carries.
const ledgerRunCap = 5

// withLedger fills the rows from the town's run ledger. A row the ledger names
// takes its newest run's outcome, stats and time, the run's own next-due time
// when it recorded one, and its newest runs. The ledger is read once per row, so
// a ledger file that cannot be read marks the row whose read failed unknown.
func withLedger(townRoot string, rows []Job, now time.Time) ([]Job, error) {
	ledger, err := jobledger.NewLedger(townRoot)
	if err != nil {
		// The only town root the ledger refuses is the empty one, which no
		// town supplies: a caller error, not a row no reader can vouch for.
		return nil, err
	}
	for i := range rows {
		rows[i] = ledgerRow(rows[i], ledger, now)
	}
	return rows, nil
}

// ledgerRow applies the ledger to one row. A read error marks the row unknown.
// A job with no run keeps the state its own reader gave it and gains a computed
// due time. A run's own next-due time becomes the row's due time and judges the
// job in place of its cadence, except a row its own reader left unknown: a run
// cannot repair that. A failed outcome from either source stays failed, and the
// run's stats join the reader's rather than replace them.
func ledgerRow(j Job, ledger *jobledger.Ledger, now time.Time) Job {
	runs, err := ledger.Tail(j.Name, ledgerRunCap)
	if err != nil {
		j.State = StateUnknown
		return j
	}
	if len(runs) == 0 {
		j.Due = dueOf(j)
		return j
	}
	// Tail returns the runs oldest first, so the newest is the last.
	newest := runs[len(runs)-1]
	j.Runs = runs
	if at := runTime(newest); at != nil {
		j.LastRun = at
	}
	if j.Outcome != OutcomeFailed {
		// A row whose own reader reported a failure keeps it: a clean run in
		// the ledger does not repair an input the reader could not read.
		j.Outcome = ledgerOutcome(newest.Outcome)
	}
	j.Stats = mergeStats(j.Stats, newest.Stats)
	if newest.NextDue != nil {
		due := *newest.NextDue
		j.Due = &due
		if j.State != StateUnknown {
			j.State = stateAtDue(due, now)
		}
		return j
	}
	j.Due = dueOf(j)
	if j.Cadence > 0 && j.State != StateUnknown {
		// The run replaced the time the row was judged on, so judge it again;
		// a cadence that could not be read left the row unknown, and a run with
		// no next-due time cannot repair that.
		j.State = stateOf(j, false, false, now)
	}
	return j
}

// mergeStats returns the row's stats with the run's figures laid over them: a
// key the run recorded wins, and a key only the row's reader has stays.
func mergeStats(row, run map[string]int64) map[string]int64 {
	if len(run) == 0 {
		return row
	}
	out := maps.Clone(row)
	if out == nil {
		out = make(map[string]int64, len(run))
	}
	maps.Copy(out, run)
	return out
}

// runTime is when a run finished, else when it started: nil when it recorded
// neither.
func runTime(r jobledger.Run) *time.Time {
	at := r.EndedAt
	if at.IsZero() {
		at = r.StartedAt
	}
	if at.IsZero() {
		return nil
	}
	return &at
}

// ledgerOutcome maps a run's outcome to the row's. The empty string is a run
// that recorded no outcome; any other string this list does not know is
// failed, because a run whose outcome cannot be read did not succeed.
func ledgerOutcome(s string) Outcome {
	switch Outcome(s) {
	case OutcomeOK, OutcomeFailed, OutcomeRefused, OutcomeDeferred:
		return Outcome(s)
	case "":
		return ""
	}
	return OutcomeFailed
}

// graceOf is the slack an overdue check allows past the cadence.
func graceOf(cadence time.Duration) time.Duration {
	return max(cadence/4, minGrace)
}

// dueOf is when a row is due by its cadence: its last run plus the cadence and
// its grace. Nil when the row has no last run or no cadence.
func dueOf(j Job) *time.Time {
	if j.LastRun == nil || j.Cadence <= 0 {
		return nil
	}
	due := j.LastRun.Add(j.Cadence + graceOf(j.Cadence))
	return &due
}

// stateAtDue judges a job by a due time it recorded itself: ok until that time
// passes, overdue after it.
func stateAtDue(due, now time.Time) State {
	if now.After(due) {
		return StateOverdue
	}
	return StateOK
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
	grace := graceOf(j.Cadence)
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
