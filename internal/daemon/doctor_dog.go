package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/slot"
)

// Operational constants — timeouts needed to perform checks.
const (
	defaultDoctorDogInterval = 5 * time.Minute
)

// Default advisory thresholds — used for recommendations in the report.
// These are defaults; override via DoctorDogConfig fields.
const (
	defaultDoctorDogLatencyAlertMs   = 5000.0
	defaultDoctorDogOrphanAlertCount = 20
	// The backup is nightly (scheduled_maintenance, gt-8z769.5).
	defaultDoctorDogBackupStaleSeconds = float64(doltbackup.StaleAfter / time.Second)
)

// doctorDogThresholds returns the effective thresholds, using config overrides or defaults.
func doctorDogThresholds(config *DaemonPatrolConfig) (latencyMs float64, orphanCount int, backupStaleSec float64) {
	latencyMs = defaultDoctorDogLatencyAlertMs
	orphanCount = defaultDoctorDogOrphanAlertCount
	backupStaleSec = defaultDoctorDogBackupStaleSeconds

	if config != nil && config.Patrols != nil && config.Patrols.DoctorDog != nil {
		cfg := config.Patrols.DoctorDog
		if cfg.LatencyAlertMs > 0 {
			latencyMs = cfg.LatencyAlertMs
		}
		if cfg.OrphanAlertCount > 0 {
			orphanCount = cfg.OrphanAlertCount
		}
		if cfg.BackupStaleSeconds > 0 {
			backupStaleSec = cfg.BackupStaleSeconds
		}
	}
	return
}

// doctorDogInterval returns the configured interval, or the default (5m).
func doctorDogInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.DoctorDog != nil {
		if config.Patrols.DoctorDog.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.DoctorDog.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultDoctorDogInterval
}

// doctorDogDatabases returns the list of production databases for health checks.
func doctorDogDatabases(config *DaemonPatrolConfig) []string {
	if config != nil && config.Patrols != nil && config.Patrols.DoctorDog != nil {
		if len(config.Patrols.DoctorDog.Databases) > 0 {
			return config.Patrols.DoctorDog.Databases
		}
	}
	return []string{"hq", "gt", "mo"}
}

// doctorProbes are the measurements behind every doctor_dog check. They
// are functions so tests can drive each branch without a Dolt server or a
// docker daemon.
type doctorProbes struct {
	// latency times SELECT active_branch(); an error means unreachable.
	latency func() (time.Duration, error)
	// conns returns the active connection count and the configured maximum.
	conns func() (int, int, error)
	// databases lists the databases the server serves.
	databases func() ([]string, error)
	// orphans counts databases no rig references (gt dolt cleanup's set).
	orphans func() (int, error)
	// backupAge is the age of db's newest nightly backup; false when it has none.
	backupAge func(db string) (time.Duration, bool)
	// reap removes gate-container debris (gt slot reap).
	reap func() (slot.ReapReport, error)
}

// doctorLimits are the thresholds past which a measurement becomes a finding.
type doctorLimits struct {
	latency     time.Duration
	orphans     int
	backupStale time.Duration
}

// doctorReport is one cycle: findings fail the cycle, notes are logged.
type doctorReport struct {
	findings []string
	notes    []string
	latency  time.Duration
	conns    int
	connMax  int
	orphans  int
}

// doctorConnAlertPct is the share of max connections doctor_dog warns at.
const doctorConnAlertPct = 80

// doctorDogFindings runs the doctor checks deterministically. Every check is
// a threshold comparison or a sanctioned reap; the agent session that once ran
// them as the mol-dog-doctor formula added nothing to the all-clear case except
// a ~24k-token uncached first turn every five minutes (claude-l5w).
func doctorDogFindings(p doctorProbes, lim doctorLimits) doctorReport {
	var r doctorReport

	latency, err := p.latency()
	if err != nil {
		r.findings = append(r.findings, fmt.Sprintf("Dolt server unreachable: %v", err))
		// Every other server check would fail for the same reason.
		r.notes = append(r.notes, reapNotes(p, &r)...)
		return r
	}
	r.latency = latency
	if latency > lim.latency {
		r.findings = append(r.findings, fmt.Sprintf("query latency %v exceeds %v", latency.Round(time.Millisecond), lim.latency))
	}

	// A probe that errors on a reachable server is a finding, not a note: a
	// measurement that cannot be taken must not read as a healthy one. The
	// worst case is a pour every run, which is where this started.
	if n, limit, err := p.conns(); err != nil {
		r.findings = append(r.findings, fmt.Sprintf("connection count unavailable: %v", err))
	} else {
		r.conns, r.connMax = n, limit
		if limit > 0 && n*100 >= limit*doctorConnAlertPct {
			r.findings = append(r.findings, fmt.Sprintf("connections %d of %d (>= %d%%)", n, limit, doctorConnAlertPct))
		}
	}

	if n, err := p.orphans(); err != nil {
		r.findings = append(r.findings, fmt.Sprintf("orphan scan unavailable: %v", err))
	} else {
		r.orphans = n
		if n > lim.orphans {
			r.findings = append(r.findings, fmt.Sprintf("%d orphan databases exceed %d; recommend gt dolt cleanup", n, lim.orphans))
		}
	}

	// Only served databases: old nights hold databases long since dropped,
	// and judging those would trip on every run.
	if dbs, err := p.databases(); err != nil {
		r.findings = append(r.findings, fmt.Sprintf("database list unavailable: %v", err))
	} else {
		for _, db := range dbs {
			if age, ok := p.backupAge(db); ok && age > lim.backupStale {
				r.findings = append(r.findings, fmt.Sprintf("backup %s is %v old (threshold %v)", db, age.Round(time.Minute), lim.backupStale))
			}
		}
	}

	r.notes = append(r.notes, reapNotes(p, &r)...)
	return r
}

// reapNotes runs the debris reap. A removal that failed is a finding; a
// docker that cannot be listed is a note, since nothing here can start it.
func reapNotes(p doctorProbes, r *doctorReport) []string {
	var notes []string
	rep, err := p.reap()
	if err != nil {
		notes = append(notes, fmt.Sprintf("slot reap skipped: %v", err))
	}
	if len(rep.Removed) > 0 {
		notes = append(notes, "slot reap removed: "+strings.Join(rep.Removed, ", "))
	}
	for _, f := range rep.Failed {
		r.findings = append(r.findings, "slot reap failed: "+f)
	}
	return notes
}

// doctorDogProbes wires doctorProbes to the live town.
func doctorDogProbes(townRoot string) doctorProbes {
	return doctorProbes{
		latency: func() (time.Duration, error) { return doltserver.MeasureQueryLatency(townRoot) },
		conns: func() (int, int, error) {
			n, err := doltserver.GetActiveConnectionCount(townRoot)
			limit := doltserver.DefaultConfig(townRoot).MaxConnections
			if limit <= 0 {
				limit = 1000 // Dolt default, as GetHealthMetrics assumes
			}
			return n, limit, err
		},
		databases: func() ([]string, error) { return doltserver.ListDatabases(townRoot) },
		orphans: func() (int, error) {
			o, err := doltserver.FindOrphanedDatabases(townRoot)
			return len(o), err
		},
		backupAge: func(db string) (time.Duration, bool) {
			root, err := doltbackup.DefaultRoot()
			if err != nil {
				return 0, false
			}
			b, ok, err := doltbackup.LastFor(root, db)
			if err != nil || !ok {
				return 0, false
			}
			return b.Age(time.Now()), true
		},
		reap: func() (slot.ReapReport, error) { return slot.Reap(townRoot, slot.ReapOptions{}) },
	}
}

// runDoctorDog runs the doctor checks in the daemon. A cycle with findings is
// reported as failed (a log line and a feed event); a clean cycle logs only.
func (d *Daemon) runDoctorDog() {
	if !d.isPatrolActive("doctor_dog") {
		return
	}
	// Its probes query Dolt (latency, connections, databases); skip the tick
	// while a scheduled_maintenance gc holds the write side.
	release, ok := d.tryDoltTask("doctor_dog")
	if !ok {
		return
	}
	defer release()

	latencyMs, orphanCount, backupStaleSec := doctorDogThresholds(d.patrolConfig)
	r := doctorDogFindings(doctorDogProbes(d.config.TownRoot), doctorLimits{
		latency:     time.Duration(latencyMs) * time.Millisecond,
		orphans:     orphanCount,
		backupStale: time.Duration(backupStaleSec) * time.Second,
	})
	for _, n := range r.notes {
		d.logger.Printf("doctor_dog: %s", n)
	}
	if len(r.findings) == 0 {
		d.logger.Printf("doctor_dog: all clear (latency %v, connections %d/%d, orphans %d)",
			r.latency.Round(time.Millisecond), r.conns, r.connMax, r.orphans)
		return
	}

	cycle := d.startDogCycle("doctor_dog")
	defer cycle.close()
	cycle.failStep("inspect", strings.Join(r.findings, "; "))
}
