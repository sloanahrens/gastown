package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/util"
)

// Operational constants — timeouts needed to perform checks.
const (
	defaultDoctorDogInterval = 5 * time.Minute
)

// Default advisory thresholds — used for recommendations in the report.
// These are defaults; override via DoctorDogConfig fields.
const (
	defaultDoctorDogLatencyAlertMs     = 5000.0
	defaultDoctorDogOrphanAlertCount   = 20
	defaultDoctorDogBackupStaleSeconds = 3600.0
)

// DoctorDogConfig holds configuration for the doctor_dog patrol.
type DoctorDogConfig struct {
	// Enabled controls whether the doctor dog runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "5m").
	IntervalStr string `json:"interval,omitempty"`

	// Databases lists the expected production databases.
	// If empty, uses the default set.
	Databases []string `json:"databases,omitempty"`

	// Advisory thresholds — when exceeded, recommendations are added to the report.
	// Agents (Mayor/Deacon) read the report and decide what actions to take.
	// Zero values mean "use default".

	// LatencyAlertMs: latency threshold in ms. Default: 5000 (5s).
	LatencyAlertMs float64 `json:"latency_alert_ms,omitempty"`

	// OrphanAlertCount: database count threshold. Default: 20.
	OrphanAlertCount int `json:"orphan_alert_count,omitempty"`

	// BackupStaleSeconds: backup age threshold in seconds. Default: 3600 (1hr).
	BackupStaleSeconds float64 `json:"backup_stale_seconds,omitempty"`
}

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

// doctorProbes are the measurements behind every mol-dog-doctor step. They
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
	// backupAge is the age of db's backup directory; false when it has none.
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

// doctorReport is one precheck: findings need an agent, notes are logged.
type doctorReport struct {
	findings []string
	notes    []string
	latency  time.Duration
	conns    int
	connMax  int
	orphans  int
}

// doctorConnAlertPct is the share of max connections the formula warns at.
const doctorConnAlertPct = 80

// doctorDogFindings runs the mol-dog-doctor checks deterministically. Every
// step in that formula is a threshold comparison or a sanctioned reap; an
// agent session added nothing to the all-clear case except a ~24k-token
// uncached first turn every five minutes (claude-l5w).
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

	// Only served databases: .dolt-backup keeps directories for databases
	// long since dropped, and judging those would trip on every run.
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
// docker that cannot be listed is a note, since no dog session can start it.
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
			info, err := os.Stat(filepath.Join(townRoot, ".dolt-backup", db))
			if err != nil || !info.IsDir() {
				return 0, false
			}
			return time.Since(info.ModTime()), true
		},
		reap: func() (slot.ReapReport, error) { return slot.Reap(townRoot, slot.ReapOptions{}) },
	}
}

// runDoctorDog runs the doctor checks in the daemon and pours a mol-dog-doctor
// molecule for an agent only when one of them finds something. The daemon
// still only measures and reaps what the formula already sanctioned; the
// judgment (escalate, recommend cleanup) stays with the agent, which now gets
// the findings instead of re-deriving them.
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
		d.logger.Printf("doctor_dog: all clear (latency %v, connections %d/%d, orphans %d); no molecule",
			r.latency.Round(time.Millisecond), r.conns, r.connMax, r.orphans)
		return
	}

	findings := strings.Join(r.findings, "; ")
	d.logger.Printf("doctor_dog: %d finding(s), pouring molecule for agent execution: %s", len(r.findings), findings)

	mol := d.pourDogMolecule(constants.MolDogDoctor, map[string]string{
		"port":              strconv.Itoa(d.doltServerPort()),
		"latency_threshold": strconv.FormatFloat(latencyMs, 'f', 0, 64) + "ms",
		"orphan_threshold":  strconv.Itoa(orphanCount),
		"backup_threshold":  strconv.FormatFloat(backupStaleSec, 'f', 0, 64) + "s",
		"findings":          findings,
		"latency":           r.latency.Round(time.Millisecond).String(),
		"conn_count":        strconv.Itoa(r.conns),
		"conn_max":          strconv.Itoa(r.connMax),
		"orphan_count":      strconv.Itoa(r.orphans),
	})
	defer mol.close()

	if mol.rootID == "" {
		d.logger.Printf("doctor_dog: molecule pour failed (non-fatal), skipping cycle")
		return
	}

	d.logger.Printf("doctor_dog: poured %s → %s", constants.MolDogDoctor, mol.rootID)
}

// runDeaconSelfProbe runs one evaluate-then-send cycle of the deacon
// self-probe on the doctor-dog cadence (glossary "Self-probe"): judge
// whether the previous probe was acked within budget, advance the
// consecutive-error counter and escalate to the mayor after
// deaconSelfProbeErrorThreshold in a row, then send the next probe into the
// deacon's real inbox. Code-driven, like cleanupOrphanedDoltServers — the
// judgment itself needs no agent, only the escalation on repeated failure
// does, and that's handled by `gt escalate` under the hood.
//
// Gated on deacon.IsPaused first (fail closed on an unreadable pause state):
// a paused deacon legitimately will not ack, and evaluating anyway would
// raise a false alarm on the first operator pause.
func (d *Daemon) runDeaconSelfProbe() {
	townRoot := d.config.TownRoot
	mailbox := mail.NewMailboxFromAddress(constants.RoleDeacon, townRoot)
	sender := mail.NewRouterWithTownRoot(townRoot, townRoot)

	if err := runDeaconSelfProbeCycle(townRoot, mailbox, sender, d.escalateAlert, d.clearAlerts); err != nil {
		d.logger.Printf("doctor_dog: deacon self-probe send failed (non-fatal): %v", err)
	}
}

// cleanupOrphanedDoltServers reaps orphaned test 'dolt sql-server' processes:
// leftovers from an embedded-dolt test suite killed at its timeout, whose
// shared/per-test server never got torn down and was reparented to
// init/launchd. An 18h-old orphan survived beads' own test-side reaper, so
// the town needs its own guard on the doctor_dog cadence (gt-twil).
//
// Code-driven, not molecule-based: unlike the rest of doctor_dog, detection
// and SIGTERM here don't need agent judgment, and running on a 5-minute
// ticker (vs. an agent-dispatched molecule) catches the leak before it can
// survive to contaminate a gate run.
func (d *Daemon) cleanupOrphanedDoltServers() {
	orphans, err := util.FindOrphanDoltServers(d.config.TownRoot)
	if err != nil {
		d.logger.Printf("Warning: dolt orphan server scan failed: %v", err)
		return
	}

	for _, o := range orphans {
		if o.Reason != "orphan" {
			d.logger.Printf("dolt orphan server scan: unexpected dolt sql-server PID %d ppid=%d (%s) — not auto-reaped",
				o.PID, o.PPID, o.ConfigPath)
		}
	}

	if results := util.ReapOrphanDoltServers(orphans); len(results) > 0 {
		d.logger.Printf("dolt orphan server cleanup: reaped %d process(es)", len(results))
		for _, r := range results {
			if r.Error != nil {
				d.logger.Printf("  WARNING: SIGTERM PID %d (%s) failed: %v", r.Process.PID, r.Process.ConfigPath, r.Error)
			} else {
				d.logger.Printf("  Sent SIGTERM to PID %d ppid=%d: %s", r.Process.PID, r.Process.PPID, r.Process.ConfigPath)
			}
		}
	}

	stale, err := util.FindStaleBeadsTestTempDirs()
	if err != nil {
		d.logger.Printf("Warning: stale beads test temp dir scan failed: %v", err)
		return
	}
	if len(stale) == 0 {
		return
	}
	removed, err := util.RemoveStaleBeadsTestTempDirs(stale)
	if len(removed) > 0 {
		d.logger.Printf("dolt orphan server cleanup: removed %d stale beads-bd-tests-* temp dir(s)", len(removed))
	}
	if err != nil {
		d.logger.Printf("Warning: failed to remove some stale test temp dirs: %v", err)
	}
}
