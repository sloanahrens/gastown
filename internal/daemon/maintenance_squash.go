package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltsquash"
)

// The rolling Dolt squash's daemon step (D9a of
// docs/plans/2026-11-15-dolt-squash-plan.md; the design's "Trigger").
//
// Once a day the check reads four numbers per database the squash block
// enables — the last nightly backup's size and duration, the round-trip p95 of
// the reads bd issues, and main's commit count — and doltsquash.Decide turns
// them into a decision the check logs. Nothing is squashed here: the run, its
// marker, maintenance_state.json, the ledger line and the attention items are
// D9b (gt-0g4yoa).
//
// The check rides the scheduled_maintenance tick but not its window and not
// its quiet-window guard: it only reads, and a reading taken only in a quiet
// instant would almost never be taken (gt-y6ovz). It runs off the tick's
// goroutine because its Dolt reads can each wait out a bound and the loop
// carries the heartbeat (gt-uvxy, gt-23zbmi).

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
)

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

// runSquashCheck reads the four numbers for each enabled database and logs
// doltsquash.Decide's answer with the cut. A check that decided for every database
// records its completion, so it is not due again for a day. A check that could
// not decide for some database is NOT recorded as completed: it is due again
// after squashCheckRetryAfter, so a day of unreadable inputs does not set the
// daily gate (overseer decision, gt-g4iwk0).
//
// A number that cannot be read is a WARNING naming it, and no verdict is
// logged for that database: a quiet-day line over an unreadable input would
// read a failed read as a clean day. The run's own last-run argument is the
// zero time: nothing has run yet, and the record of a run belongs to D9b, which
// also owns the marker and the backend the run executes against.
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
		if dec.Due {
			reason += ", cut " + dec.Cut.Format(time.RFC3339)
		}
		d.logger.Printf("squash: %s: %s (%s)", db, reason, readings.describe())
	}
}

// squashReadings is what the daily check read for one database: the three
// trigger inputs, the commit count the design logs as the size trigger's proxy,
// and the last backup's age, which the run's first step requires and the check
// only reports (D9b enforces it).
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
