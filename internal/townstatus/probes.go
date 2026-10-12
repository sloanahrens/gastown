package townstatus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/util"
)

// readDoltCommitMeter fills the commits-per-day meter into info. A meter
// that cannot be read leaves it empty: the status line then shows no marker,
// and gt doctor's dolt-commit-rate check reports the failure.
func readDoltCommitMeter(info *DoltInfo, townRoot string, measure func(context.Context, string) ([]doltserver.DBCommits, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	counts, err := measure(ctx, townRoot)
	if err != nil {
		return
	}
	info.CommitsLastDay = counts
	info.CommitsPerDayWarn = config.LoadOperationalConfig(townRoot).GetDoltConfig().CommitsPerDayWarnV()
}

// strayDoltInfos turns the orphan scan into the status model's stray list,
// pairing each process with the command that clears it: kill-imposters on the
// town's port or data-dir, `kill <pid>` for a foreign-port leak (gt-gyw5w).
func strayDoltInfos(orphans []util.DoltOrphanServer, townPort int, townDataDir string) []StrayDoltInfo {
	if len(orphans) == 0 {
		return nil
	}
	out := make([]StrayDoltInfo, 0, len(orphans))
	for _, o := range orphans {
		out = append(out, StrayDoltInfo{
			PID:     o.PID,
			Port:    o.Port,
			DataDir: o.DataDir,
			Remedy:  util.StrayDoltRemedy(util.StrayDoltProcess{PID: o.PID, Port: o.Port, DataDir: o.DataDir}, townPort, townDataDir),
		})
	}
	return out
}

// supervisorProbe reads the host's supervisor for a job it is failing to
// spawn. It is the one live probe in this otherwise file-backed view: in that
// state the daemon is not running, so the health file is stale or absent and
// nothing on disk names the cause (gt-92qx3). A test passes its own.
type supervisorProbe func(townRoot string) (templates.SupervisorState, bool)

// hostSupervisorRespawn asks launchctl what it is doing with the daemon's job.
func hostSupervisorRespawn(townRoot string) (templates.SupervisorState, bool) {
	return templates.SupervisorRespawn(townRoot, templates.SupervisorJobState)
}

// HealthView reads the health file the daemon writes every tick
// (gt-s3rec.2) and renders it at now: the one line, then one line per
// field. A file that is missing, unreadable or stale is UNKNOWN; a broken
// operational.health block falls back to the default stale age (the
// report's config field already says so). A supervisor that cannot spawn the
// daemon is the one state the file cannot carry, so it is read live and
// renders the daemon field RED (gt-92qx3).
func HealthView(townRoot string, now time.Time) (lines []string, v townhealth.Verdict, rep *townhealth.Report) {
	return healthView(townRoot, now, hostSupervisorRespawn)
}

// healthView is HealthView over an injected supervisor probe.
func healthView(townRoot string, now time.Time, supervisor supervisorProbe) (lines []string, v townhealth.Verdict, rep *townhealth.Report) {
	_, stale, err := config.LoadOperationalConfig(townRoot).GetHealthSettings().Resolve()
	if err != nil {
		stale = townhealth.DefaultStaleAfter
	}
	r, readErr := townhealth.Read(townRoot)
	if st, looping := supervisor(townRoot); looping {
		out := daemonRespawnReport(r, readErr, st, now, stale)
		return townhealth.Lines(out, now, stale), townhealth.Effective(out, now, stale), &out
	}
	if readErr != nil {
		return []string{fmt.Sprintf("UNKNOWN no health report: %v", readErr)}, townhealth.VerdictUnknown, nil
	}
	return townhealth.Lines(r, now, stale), townhealth.Effective(r, now, stale), &r
}

// daemonRespawnReport is the report for a town whose supervisor is failing
// every spawn of the daemon: read as it was found, with its daemon field
// replaced by the live supervisor failure. A read that is absent or stale
// cannot date the line — the daemon that writes it is the one not running — so
// the report is dated now and carries the live field alone.
func daemonRespawnReport(read townhealth.Report, readErr error, st templates.SupervisorState, now time.Time, staleAfter time.Duration) townhealth.Report {
	f := daemonRespawnField(st, readErr)
	if readErr != nil || now.Sub(read.At) >= staleAfter {
		return townhealth.Report{At: now, Verdict: townhealth.Red, Fields: []townhealth.Field{f}}
	}
	return withDaemonField(read, f)
}

// daemonRespawnField is the daemon field for a supervisor that never gets the
// program started, and the repair an operator has to run by hand.
func daemonRespawnField(st templates.SupervisorState, readErr error) townhealth.Field {
	detail := fmt.Sprintf("%s has no process for the job after %d spawns, last exit code %d; repair: %s",
		st.Kind, st.Runs, st.LastExit, templates.SupervisorRepair(st.Kind))
	if readErr != nil {
		detail = fmt.Sprintf("%s; no health report (%v)", detail, readErr)
	}
	return townhealth.Field{
		Name:    townhealth.FieldDaemon,
		Tag:     townhealth.Live,
		Verdict: townhealth.Red,
		Value:   fmt.Sprintf("%s respawn loop, no pid, runs %d, last exit code %d", st.Kind, st.Runs, st.LastExit),
		Detail:  detail,
	}
}

// withDaemonField returns r with its daemon field replaced by f, appended when
// it has none, and its verdict recomputed.
func withDaemonField(r townhealth.Report, f townhealth.Field) townhealth.Report {
	fields := make([]townhealth.Field, 0, len(r.Fields)+1)
	replaced := false
	for _, g := range r.Fields {
		if g.Name == townhealth.FieldDaemon {
			g, replaced = f, true
		}
		fields = append(fields, g)
	}
	if !replaced {
		fields = append(fields, f)
	}
	r.Fields = fields
	r.Verdict = townhealth.Green
	for _, g := range fields {
		r.Verdict = r.Verdict.Worse(g.Verdict)
	}
	return r
}

// gatePool resolves the town's container-gate pool from
// operational.container_gate (see internal/slot).
func gatePool(townRoot string) slot.Pool {
	cg := config.LoadOperationalConfig(townRoot).GetContainerGateConfig()
	return slot.PoolFromConfig(cg)
}

// gateSlotHolder reads the container-gate pool's current holder for the
// 'gt status' slot line, or nil when no slot is held or the read failed.
//
// Flock-only (StatusPoolLocksOnly): its caller runs on every refresh and this
// result carries nothing but the holder, so the full StatusPool's `docker ps`
// cross-check would be a subprocess whose output is never read (gt-a8kx).
func gateSlotHolder(townRoot string) *SlotInfo {
	rep, err := slot.StatusPoolLocksOnly(townRoot, gatePool(townRoot))
	if err != nil || !rep.Held || rep.Owner == nil {
		return nil
	}
	return &SlotInfo{Role: rep.Owner.Role, PID: rep.Owner.PID, AcquiredAt: rep.Owner.AcquiredAt}
}

// tryDetailLock serializes the Dolt-heavy half of a gather: a second status
// run that cannot take the lock renders runtime-only (Fast) status instead of
// adding its queries to the first one's. The returned bool is false when
// another run holds it.
func tryDetailLock(townRoot string) (func(), bool) {
	if townRoot == "" {
		return func() {}, true
	}

	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return func() {}, false
	}

	fileLock := flock.New(filepath.Join(dir, "status-detail.lock"))
	locked, err := fileLock.TryLock()
	if err != nil || !locked {
		return func() {}, false
	}

	return func() { _ = fileLock.Unlock() }, true
}
