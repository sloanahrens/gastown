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
	"github.com/steveyegge/gastown/internal/townhealth"
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

// HealthView reads the health file the daemon writes every tick
// (gt-s3rec.2) and renders it at now: the one line, then one line per
// field. A file that is missing, unreadable or stale is UNKNOWN; a broken
// operational.health block falls back to the default stale age (the
// report's config field already says so).
func HealthView(townRoot string, now time.Time) (lines []string, v townhealth.Verdict, rep *townhealth.Report) {
	_, stale, err := config.LoadOperationalConfig(townRoot).GetHealthSettings().Resolve()
	if err != nil {
		stale = townhealth.DefaultStaleAfter
	}
	r, err := townhealth.Read(townRoot)
	if err != nil {
		return []string{fmt.Sprintf("UNKNOWN no health report: %v", err)}, townhealth.VerdictUnknown, nil
	}
	return townhealth.Lines(r, now, stale), townhealth.Effective(r, now, stale), &r
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
