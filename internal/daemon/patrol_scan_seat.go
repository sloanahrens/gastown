package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/patrolscan"
)

// patrolScanSeat scans one seat of one rig and logs its report: the seat's
// answer, not the rig's. It builds the Scanner the way runPatrolScanOn does, on
// the same host, ledger and options, and runs TickSeat, so the seat meets the
// rules the tick applies to every seat.
//
// It does not take patrolScanRunning, so it cannot wait on a running tick. The
// caller owns the exclusion: the patrol_scan tick holds the flag around its
// scans, and any other caller must hold it the same way before it scans a seat.
func (d *Daemon) patrolScanSeat(h *patrolScanHost, rig, name string) {
	if !d.isPatrolActive("patrol_scan") || d.config == nil {
		return
	}
	ledger := patrolscan.NewFileLedger(patrolscan.LedgerPath(d.config.TownRoot), 7*24*time.Hour)
	scanner := patrolscan.New(h, ledger, d.patrolScanRunOptions(h))
	report := scanner.TickSeat(rig, name)
	for _, line := range report.Lines() {
		d.logger.Printf("patrol_scan: %s", line)
	}
}
