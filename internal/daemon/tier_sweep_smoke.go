package daemon

import (
	"errors"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// sweepSmokeRecord is what the daemon's startup smoke launch proved about
// launching the sweeper: the outcome, the first line of the error when it
// failed, when the launch was classified, and the daemon start it belongs to,
// so the verify probe can tell a stale record from this daemon's (gt-ohgh0v).
type sweepSmokeRecord struct {
	Time        time.Time `json:"time"`
	DaemonStart time.Time `json:"daemon_start"`
	Outcome     string    `json:"outcome"`
	Detail      string    `json:"detail"`
}

// classifySweepSmoke names the outcome a sweep launch reported: busy while a
// sweep Job of the rig holds the claim guard, declined when the capacity
// preflight refused it, failed with the error's first line as Detail, and
// started otherwise. It fills only Outcome and Detail; the caller stamps Time
// and DaemonStart (gt-ohgh0v).
func classifySweepSmoke(l tierSweepLaunch) sweepSmokeRecord {
	var busy *sweeprunner.ActiveJob
	if errors.As(l.err, &busy) {
		return sweepSmokeRecord{Outcome: "busy"}
	}
	if l.declined {
		return sweepSmokeRecord{Outcome: "declined"}
	}
	if l.err != nil {
		detail, _, _ := strings.Cut(l.err.Error(), "\n")
		return sweepSmokeRecord{Outcome: "failed", Detail: detail}
	}
	return sweepSmokeRecord{Outcome: "started"}
}
