package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltsquash"
	"github.com/steveyegge/gastown/internal/lock"
)

// The rolling Dolt squash's daemon step (D9a and D9b-1 of
// docs/plans/2026-11-15-dolt-squash-plan.md; the design's "Trigger" and "The
// run").
//
// Once a day the check reads four numbers per database the squash block
// enables — the last nightly backup's size and duration, the round-trip p95 of
// the reads bd issues, and main's commit count — and doltsquash.Decide turns
// them into a decision the check logs. A database Decide finds due is run by
// doltsquash.Run, which holds the town's run marker for its whole length so
// two daemons cannot run it at once. The run's state record, ledger line and
// attention item are D9b-2 and D9b-3 (gt-5v1fye, gt-0g4yoa).
//
// The check rides the scheduled_maintenance tick but not its window and not
// its quiet-window guard: its readings are taken on every tick regardless, and
// a reading taken only in a quiet instant would almost never be taken
// (gt-y6ovz). It runs off the tick's goroutine because its Dolt reads and a
// run's steps can each wait out a bound and the loop carries the heartbeat
// (gt-uvxy, gt-23zbmi).

const (
	// squashCheckPatrol is the daily check's key in daemon/patrol_last_run.json.
	squashCheckPatrol = "squash_check"

	// squashCheckInterval is how often the check is due: the 24-hour gate a
	// run itself must pass (doltsquash.RunInterval), so the four numbers are
	// read once a day.
	squashCheckInterval = doltsquash.RunInterval

	// squashCheckRetryAfter is how soon a check that could not decide is due
	// again: re-reading every tick would repeat the same failure, and waiting
	// the whole day would leave it unseen for 24 hours.
	squashCheckRetryAfter = time.Hour

	// squashP95Rounds is how many round trips the query p95 is taken over, one
	// read per round, cycling the mix. Twenty is the floor gt-dc81 set for a
	// p95 to mean anything: a percentile over fewer samples is noise.
	squashP95Rounds = 20

	// squashP95Percent is the percentile the trigger compares.
	squashP95Percent = 95

	// squashP95Budget bounds the whole p95 series. A read that runs past it,
	// or fails, leaves the p95 unreadable rather than a smaller number.
	squashP95Budget = 20 * time.Second

	// squashDoltReadTimeout bounds one statement the check reads, as the
	// connection's read timeout.
	squashDoltReadTimeout = 10 * time.Second

	// squashLatencyProbeID is the id the mix's lookup asks for. No issue
	// carries it, so the lookup returns no row and still makes the full round
	// trip bd makes for an id it does not find.
	squashLatencyProbeID = "gt-squash-latency-probe"

	// squashRunMarkerFile is the run marker's name under <town>/daemon. One
	// run holds the flock on it for its whole length, so a second daemon's run
	// is refused before it sends a statement.
	squashRunMarkerFile = "squash_run.lock"

	// squashRunBackupFreshness is how recent the last nightly backup must be
	// for a run to start: the 24 hours the design's step 1 allows, which is
	// also the age the restore path can use.
	squashRunBackupFreshness = 24 * time.Hour
)

// squashMarkerPath is the run marker's file: <town>/daemon/squash_run.lock.
func squashMarkerPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", squashRunMarkerFile)
}

// errSquashMarkerUnavailable reports that the run marker could not be taken for
// a reason other than another holder: the flock could not be opened or locked,
// so the daemon cannot tell whether a run holds it. It is kept apart from
// doltsquash.ErrRunMarkerHeld because a lock failure is not evidence another
// run is in progress, and must not be read as one.
var errSquashMarkerUnavailable = errors.New("squash: the run marker cannot be taken")

// squashMarker is doltsquash.Marker over one flock file: Hold creates the file
// and takes the exclusive lock without waiting, so a run that finds it held is
// refused rather than queued, and Release gives it back. doltsquash.Run calls
// both, so the marker is held for the run's whole length and released on every
// way out.
type squashMarker struct {
	path    string
	release func()
}

// Hold takes the marker, or returns doltsquash.ErrRunMarkerHeld while another
// run holds it. A marker that could not be locked at all returns
// errSquashMarkerUnavailable, because that is not the same answer.
func (m *squashMarker) Hold() error {
	if m.release != nil {
		return fmt.Errorf("%w: this marker already holds it", errSquashMarkerUnavailable)
	}
	// The lock call creates the file but not its directory, and a fresh town
	// may not hold <town>/daemon yet.
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return fmt.Errorf("%w: %v", errSquashMarkerUnavailable, err)
	}
	release, ok, err := lock.FlockTryAcquire(m.path)
	if err != nil {
		return fmt.Errorf("%w: %v", errSquashMarkerUnavailable, err)
	}
	if !ok {
		return doltsquash.ErrRunMarkerHeld
	}
	m.release = release
	return nil
}

// Release gives the marker back. A marker that never held it releases nothing,
// so the deferred Release that follows a refused Hold is harmless.
func (m *squashMarker) Release() error {
	if m.release == nil {
		return nil
	}
	m.release()
	m.release = nil
	return nil
}

// squashLatencyMix is the read mix the check times: the three reads bd issues,
// as `gt dolt latency` runs them (internal/cmd/dolt_latency.go). Each statement
// passes beadsql's read-only guard before it is sent.
var squashLatencyMix = []beadsql.Query{
	beadsql.InProgressByAge(),
	beadsql.LabelPresent(squashLatencyProbeID, "latency"),
	beadsql.IssueCount(),
}

// squashStep is the daily check's built step: the settings for the databases
// the squash block enables. d.squashStep builds it, and only when the block is
// on; a nil step means the block is off, so there is no zero value to use.
type squashStep struct {
	settings *agentconfig.SquashSettings
}

// squashStep builds the daily check's step from the patrols config's squash
// block. It returns nil, with no error, when the block is absent or off: the
// daemon builds no squash step and its tick does nothing for it
// (config.ParseSquash). A block that is on but holds a value out of bounds is
// an error naming its daemon.json key, and no step is built, so the check never
// runs with a bound nobody wrote (gt-f1or4z).
func (d *Daemon) squashStep() (*squashStep, error) {
	var sm *agentconfig.ScheduledMaintenanceConfig
	if pc := d.patrol(); pc != nil && pc.Patrols != nil {
		sm = pc.Patrols.ScheduledMaintenance
	}
	settings, err := sm.ParseSquash()
	if err != nil {
		return nil, err
	}
	if settings == nil {
		return nil, nil
	}
	return &squashStep{settings: settings}, nil
}

// triggerSquashCheck runs the daily check when it is due, on its own
// goroutine. It is called on every scheduled_maintenance tick, so it decides
// due-ness itself rather than being called once a day: a daemon restart resets
// an in-process ticker but not the persisted last run (gt-ima2), and the
// check's own record is what keeps it to once a day.
//
// A step built from no block (the block absent or off) ends the tick here with
// nothing read and nothing logged. One check runs at a time: a tick that finds
// one in flight skips, so two checks cannot read the same numbers and record a
// second completion.
func (d *Daemon) triggerSquashCheck() {
	step, err := d.squashStep()
	if err != nil {
		d.logger.Printf("squash: WARNING: %v — the check did not run", err)
		return
	}
	if step == nil {
		return
	}
	now := d.maintenance().now()
	dec := evaluatePatrolDue(d.config.TownRoot, squashCheckPatrol, d.squashCheckRun.last(), now, squashCheckInterval)
	if !dec.due {
		// Silent: the tick reaches here every few minutes, and the due day's
		// readings line is the record of the check.
		return
	}
	if !d.squashCheckRunning.CompareAndSwap(false, true) {
		d.logger.Printf("squash: previous check still running — skipping this tick")
		return
	}
	if dec.warn != "" {
		d.logger.Printf("squash: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("squash: due — %s", dec.note)
	}
	d.maintenance().dispatch(func() {
		defer d.squashCheckRunning.Store(false)
		d.runSquashCheck(step, now)
	})
}

// runSquashCheck reads the four numbers for each enabled database, logs
// doltsquash.Decide's answer with the cut, and runs a due database through
// doltsquash.Run. A check that finished every database records its completion,
// so it is not due again for a day. A check that could not decide, could not
// run, or found the run marker held is NOT recorded as completed: it is due
// again after squashCheckRetryAfter, so a day of unreadable inputs or a run a
// second daemon holds does not set the daily gate (overseer decision,
// gt-g4iwk0).
//
// A number that cannot be read is a WARNING naming it, and no verdict is
// logged for that database: a quiet-day line over an unreadable input would
// read a failed read as a clean day. The run's own last-run argument is the
// zero time: the daily gate above is the only one until D9b-2 records runs in
// maintenance_state.json.
func (d *Daemon) runSquashCheck(step *squashStep, now time.Time) {
	undecided := false
	defer func() {
		at := d.maintenance().now()
		if undecided {
			// Backdated so the patrol's own interval makes it due again after
			// squashCheckRetryAfter rather than after the whole interval.
			at = at.Add(squashCheckRetryAfter - squashCheckInterval)
		}
		d.recordPatrolRun(&d.squashCheckRun, squashCheckPatrol, at)
	}()

	settings := doltsquash.Settings{
		Thresholds: doltsquash.Thresholds{
			BackupBytes:    step.settings.TriggerBackupBytes,
			BackupDuration: step.settings.TriggerBackupDuration,
			QueryP95:       step.settings.TriggerQueryP95,
		},
		KeepDays: step.settings.KeepDays,
	}
	// Each read carries its own bound, so the check needs no clock of its own
	// to cancel: a daemon shutdown does not have to cut a read short.
	ctx := context.Background()
	for _, db := range step.settings.Databases {
		readings := d.readSquashInputs(ctx, db, now)
		if names := readings.unreadable(); len(names) > 0 {
			undecided = true
			d.logger.Printf("squash: WARNING: %s: the check did not decide, unreadable: %s (%s)",
				db, strings.Join(names, ", "), readings.describe())
			continue
		}
		dec, err := doltsquash.Decide(now, time.Time{}, readings.inputs, settings)
		if err != nil {
			undecided = true
			d.logger.Printf("squash: WARNING: %s: %v — the check did not decide", db, err)
			continue
		}
		reason := dec.Reason
		if !dec.Due {
			d.logger.Printf("squash: %s: %s (%s)", db, reason, readings.describe())
			continue
		}
		reason += ", cut " + dec.Cut.Format(time.RFC3339)
		d.logger.Printf("squash: %s: %s (%s)", db, reason, readings.describe())
		// A run starts only on a backup the restore path can use (design step
		// 1): an older one holds the run rather than squashing history the
		// backup does not cover.
		if readings.backupAge > squashRunBackupFreshness {
			undecided = true
			d.logger.Printf("squash: %s: holding: the last backup finished %v ago, and a run needs one within %v",
				db, readings.backupAge.Round(time.Minute), squashRunBackupFreshness)
			continue
		}
		res, err := d.runSquashDatabase(ctx, db, dec.Cut, step.settings)
		switch {
		case errors.Is(err, doltsquash.ErrRunMarkerHeld):
			undecided = true
			d.logger.Printf("squash: %s: another daemon holds the run marker — skipping this check", db)
		case errors.Is(err, errSquashMarkerUnavailable):
			undecided = true
			d.logger.Printf("squash: WARNING: %s: %v — the run did not start", db, err)
		case err != nil:
			undecided = true
			d.logger.Printf("squash: WARNING: %s: the run failed: %v", db, err)
		case res.Held:
			d.logger.Printf("squash: %s: the run held: %s", db, res.Recovered.Reason)
		default:
			d.logger.Printf("squash: %s: the run finished", db)
		}
	}
}

// runSquashDatabase runs the engine over one due database. It opens one
// session, which MaxRuntime bounds both as the run's context and as the
// connection's read timeout, and doltsquash.Run holds the town's run marker for
// the run's whole length. A run refused by the marker has sent no statement:
// its error matches doltsquash.ErrRunMarkerHeld, and a marker that could not be
// taken matches errSquashMarkerUnavailable.
func (d *Daemon) runSquashDatabase(ctx context.Context, db string, cut time.Time, settings *agentconfig.SquashSettings) (doltsquash.RunResult, error) {
	conn, err := d.maintenance().squashOpenDB(d.maintenanceDSN(db, settings.MaxRuntime))
	if err != nil {
		return doltsquash.RunResult{}, fmt.Errorf("squash: opening the connection to %s: %w", db, err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	runCtx, cancel := context.WithTimeout(ctx, settings.MaxRuntime)
	defer cancel()
	return doltsquash.Run(runCtx, doltsquash.RunInput{
		Exec:    squashDoltExec{conn: conn},
		Marker:  &squashMarker{path: squashMarkerPath(d.config.TownRoot)},
		Clock:   d.clk(),
		Sleeper: d.clk(),
		Cut:     cut,
	})
}

// squashReadings is what the daily check read for one database: the three
// trigger inputs, the commit count the design logs as the size trigger's proxy,
// and the last backup's age, which the run's first step requires before it
// starts.
type squashReadings struct {
	inputs       doltsquash.Inputs
	commits      int64
	commitsErr   error
	backupAge    time.Duration
	backupAgeErr error
}

// readSquashInputs reads one database's numbers. A number that cannot be read
// is a Failed reading or a recorded error, never a zero: doltsquash.Decide
// names an unreadable input and compares nothing, so an unreadable trigger
// cannot trip a run. db is a name config.ParseSquash accepted, so it holds only
// letters, digits, underscore and hyphen before it reaches the DSN.
func (d *Daemon) readSquashInputs(ctx context.Context, db string, now time.Time) squashReadings {
	var r squashReadings
	r.inputs.BackupBytes, r.inputs.BackupDuration, r.backupAge, r.backupAgeErr = d.readSquashBackup(db, now)

	conn, err := d.maintenance().squashOpenDB(d.maintenanceDSN(db, squashDoltReadTimeout))
	if err != nil {
		err = fmt.Errorf("squash: opening the connection to %s: %w", db, err)
		r.inputs.QueryP95 = doltsquash.Failed[time.Duration](err)
		r.commitsErr = err
		return r
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	ex := squashDoltExec{conn: conn}

	r.inputs.QueryP95 = squashQueryP95(ctx, ex, d.maintenance().now)

	countCtx, cancel := context.WithTimeout(ctx, squashDoltReadTimeout)
	defer cancel()
	r.commits, r.commitsErr = squashMainCommits(countCtx, ex)
	return r
}

// readSquashBackup reads the newest complete backup that holds db: its size,
// the night's duration, and its age. The manifest records one duration per
// night, not per database, so every database backed up that night reports the
// same duration.
func (d *Daemon) readSquashBackup(db string, now time.Time) (size doltsquash.Reading[int64], duration doltsquash.Reading[time.Duration], age time.Duration, ageErr error) {
	root, err := d.maintenance().backupRoot()
	if err != nil {
		err = fmt.Errorf("squash: the backup root: %w", err)
		return doltsquash.Failed[int64](err), doltsquash.Failed[time.Duration](err), 0, err
	}
	b, ok, err := doltbackup.LastFor(root, db)
	if err != nil {
		err = fmt.Errorf("squash: reading the backups under %s: %w", root, err)
		return doltsquash.Failed[int64](err), doltsquash.Failed[time.Duration](err), 0, err
	}
	if !ok {
		err = fmt.Errorf("squash: no complete backup holds %s", db)
		return doltsquash.Failed[int64](err), doltsquash.Failed[time.Duration](err), 0, err
	}
	// dirSize is the size the gc reasons about: the database's regular files
	// with Dolt's journal left out.
	bytes, err := dirSize(filepath.Join(b.Path, db))
	if err != nil {
		size = doltsquash.Failed[int64](fmt.Errorf("squash: measuring the backup of %s: %w", db, err))
	} else {
		size = doltsquash.Read(bytes)
	}
	started, finished := b.Manifest.Started, b.Manifest.Finished
	if started.IsZero() || finished.IsZero() || finished.Before(started) {
		bad := fmt.Errorf("squash: the backup of %s records started %v and finished %v, so its duration cannot be read",
			db, started, finished)
		return size, doltsquash.Failed[time.Duration](bad), 0, bad
	}
	duration = doltsquash.Read(finished.Sub(started))
	return size, duration, now.Sub(finished), nil
}

// unreadable names the numbers the check could not read, in the order the log
// line gives them. An empty result means every number was read.
func (r squashReadings) unreadable() []string {
	var names []string
	if r.inputs.BackupBytes.Err != nil {
		names = append(names, "backup bytes")
	}
	if r.inputs.BackupDuration.Err != nil {
		names = append(names, "backup duration")
	}
	if r.inputs.QueryP95.Err != nil {
		names = append(names, "query p95")
	}
	if r.commitsErr != nil {
		names = append(names, "main commits")
	}
	return names
}

// describe renders the readings for the check's log line.
func (r squashReadings) describe() string {
	var backup string
	switch {
	case r.inputs.BackupBytes.Err != nil:
		backup = "backup unreadable: " + r.inputs.BackupBytes.Err.Error()
	case r.inputs.BackupDuration.Err != nil:
		backup = fmt.Sprintf("backup %s, duration unreadable: %v", formatBytes(r.inputs.BackupBytes.Value), r.inputs.BackupDuration.Err)
	default:
		backup = fmt.Sprintf("backup %s in %v", formatBytes(r.inputs.BackupBytes.Value), r.inputs.BackupDuration.Value.Round(time.Millisecond))
		if r.backupAgeErr == nil {
			backup += fmt.Sprintf(", %v old", r.backupAge.Round(time.Minute))
		}
	}
	p95 := "query p95 unreadable"
	if r.inputs.QueryP95.Err != nil {
		p95 += ": " + r.inputs.QueryP95.Err.Error()
	} else {
		p95 = fmt.Sprintf("query p95 %v", r.inputs.QueryP95.Value.Round(time.Microsecond))
	}
	commits := "main commits unreadable"
	if r.commitsErr != nil {
		commits += ": " + r.commitsErr.Error()
	} else {
		commits = fmt.Sprintf("main commits %d", r.commits)
	}
	return backup + "; " + p95 + "; " + commits
}

// squashMainCommitsSQL is main's commit count, the fourth number the design's
// check reads. It is the statement the engine reads the count with after a
// swap (doltsquash.Run's mainCommits).
const squashMainCommitsSQL = "SELECT COUNT(*) FROM dolt_log('main')"

// squashMainCommits reads main's commit count through ex.
func squashMainCommits(ctx context.Context, ex doltsquash.Exec) (int64, error) {
	rows, err := ex.Exec(ctx, squashMainCommitsSQL)
	if err != nil {
		return 0, err
	}
	if len(rows.Data) != 1 || len(rows.Data[0]) != 1 {
		return 0, fmt.Errorf("squash: the commit count of main returned %d records holding %d values in the first, want one value",
			len(rows.Data), firstRecordValues(rows.Data))
	}
	n, err := strconv.ParseInt(rows.Data[0][0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("squash: the commit count of main read %q, want a number", rows.Data[0][0])
	}
	return n, nil
}

// firstRecordValues is how many values the first record holds, 0 when there is
// no record.
func firstRecordValues(data [][]string) int {
	if len(data) == 0 {
		return 0
	}
	return len(data[0])
}

// squashQueryP95 times the reads bd issues against ex and returns their
// nearest-rank 95th percentile. One read runs per round, cycling the mix; the
// budget bounds the whole series. A read that fails, or a budget that runs out,
// leaves the p95 unreadable rather than a number taken from fewer, faster
// reads.
func squashQueryP95(ctx context.Context, ex doltsquash.Exec, now func() time.Time) doltsquash.Reading[time.Duration] {
	budget, cancel := context.WithTimeout(ctx, squashP95Budget)
	defer cancel()

	statements := make([]string, len(squashLatencyMix))
	for i, q := range squashLatencyMix {
		statement, err := q.Inline()
		if err != nil {
			return doltsquash.Failed[time.Duration](fmt.Errorf("squash: the latency mix's read %d: %w", i, err))
		}
		statements[i] = statement
	}
	samples := make([]time.Duration, 0, squashP95Rounds)
	for i := 0; i < squashP95Rounds; i++ {
		start := now()
		if _, err := ex.Exec(budget, statements[i%len(statements)]); err != nil {
			return doltsquash.Failed[time.Duration](fmt.Errorf(
				"squash: latency read %d of %d: %w", i+1, squashP95Rounds, err))
		}
		samples = append(samples, now().Sub(start))
	}
	return doltsquash.Read(squashP95(samples))
}

// squashP95 returns the nearest-rank squashP95Percent percentile of samples:
// the sample at rank ceil(squashP95Percent*N/100), counting from one. The rank
// is integer arithmetic, so float rounding never moves it by one sample.
// samples must not be empty; squashQueryP95 records one per round and only asks
// once it holds them all.
func squashP95(samples []time.Duration) time.Duration {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	rank := (squashP95Percent*len(sorted) + 99) / 100
	return sorted[rank-1]
}

// squashDoltExec is doltsquash's Exec over one database's daemon Dolt
// connection: one statement per call, and every row returned as strings in the
// engine's shape. A NULL value reads as the empty string; the statements this
// slice sends are a COUNT(*), which is never NULL, and reads whose round trip
// is only timed, so a later reader selecting a nullable column must treat an
// empty value as absent.
type squashDoltExec struct {
	conn *sql.DB
}

// Exec runs one statement and returns the rows it produced.
func (e squashDoltExec) Exec(ctx context.Context, statement string) (doltsquash.Rows, error) {
	rows, err := e.conn.QueryContext(ctx, statement)
	if err != nil {
		return doltsquash.Rows{}, err
	}
	defer rows.Close()
	return squashRows(rows)
}

// squashRows reads an open result set into the engine's shape. It takes the
// result set rather than the statement so a result set that is already closed,
// which is the one way the column read fails, is reachable from a test.
func squashRows(rows *sql.Rows) (doltsquash.Rows, error) {
	columns, err := rows.Columns()
	if err != nil {
		return doltsquash.Rows{}, err
	}
	values := make([]sql.NullString, len(columns))
	scan := make([]any, len(columns))
	for i := range values {
		scan[i] = &values[i]
	}
	data := make([][]string, 0)
	for rows.Next() {
		if err := rows.Scan(scan...); err != nil {
			return doltsquash.Rows{}, err
		}
		record := make([]string, len(columns))
		for i, v := range values {
			record[i] = v.String
		}
		data = append(data, record)
	}
	if err := rows.Err(); err != nil {
		return doltsquash.Rows{}, err
	}
	return doltsquash.Rows{Columns: columns, Data: data}, nil
}
