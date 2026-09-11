package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// daemonLivenessBaseline is the runtime baseline for the daemon-liveness
// check, persisted at <town>/.runtime/daemon-liveness.json. It records the
// HeartbeatCount this check last observed and when it observed it, so a
// later run can tell "fresh but not yet given a fair chance to advance"
// apart from "fresh but genuinely stuck".
type daemonLivenessBaseline struct {
	HeartbeatCount int64     `json:"heartbeat_count"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// DaemonLivenessCheck verifies the daemon's own heartbeat (daemon/state.json
// LastHeartbeat/HeartbeatCount) is both fresh and advancing. A clock
// heartbeat proves only that the clock still ticks, not that the daemon's
// work loop is making progress — until this check, nothing read
// LastHeartbeat/HeartbeatCount and acted on it; DaemonCheck only confirms
// the process is alive via lock/PID.
type DaemonLivenessCheck struct {
	BaseCheck
}

// NewDaemonLivenessCheck creates a new daemon-liveness check.
func NewDaemonLivenessCheck() *DaemonLivenessCheck {
	return &DaemonLivenessCheck{
		BaseCheck: BaseCheck{
			CheckName:        "daemon-liveness",
			CheckDescription: "Verify the daemon heartbeat is fresh and its count is advancing",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

func daemonLivenessStatePath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "daemon-liveness.json")
}

// Run reads daemon/state.json and compares it against the baseline this
// check recorded on a previous run. Liveness requires both a fresh
// timestamp and, once the baseline is old enough to have given the daemon a
// fair chance to tick again, an advancing count.
func (c *DaemonLivenessCheck) Run(ctx *CheckContext) *CheckResult {
	state, err := daemon.LoadState(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not read daemon/state.json",
			Details: []string{err.Error()},
		}
	}
	if state.LastHeartbeat.IsZero() {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: daemon/state.json is missing or has no heartbeat recorded yet",
		}
	}
	if !state.Running {
		// A cleanly stopped daemon leaves LastHeartbeat/HeartbeatCount
		// untouched (daemon.go's stop path only flips Running to false), so
		// they go stale by design and prove nothing. Liveness is only
		// meaningful while the daemon claims to be running; match
		// DaemonCheck's StatusWarning so "not running" doesn't also fail
		// doctor's exit code as an error.
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Daemon is not running",
		}
	}

	recoveryTick := config.LoadOperationalConfig(ctx.TownRoot).GetDaemonConfig().RecoveryHeartbeatIntervalD()
	staleThreshold := 2 * recoveryTick
	age := time.Since(state.LastHeartbeat)

	statePath := daemonLivenessStatePath(ctx.TownRoot)
	baseline, hadBaseline := readDaemonLivenessBaseline(statePath)
	baselineAge := time.Since(baseline.RecordedAt)
	countAdvanced := hadBaseline && state.HeartbeatCount != baseline.HeartbeatCount

	var result *CheckResult
	switch {
	case age > staleThreshold:
		result = &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("daemon heartbeat is stale: last heartbeat %s ago (threshold %s)", age.Round(time.Second), staleThreshold),
		}
	case !hadBaseline:
		result = &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: no baseline yet, recorded the current heartbeat count as the starting point",
		}
	case baselineAge >= staleThreshold && !countAdvanced:
		result = &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("daemon heartbeat count not advancing: still %d after %s", state.HeartbeatCount, baselineAge.Round(time.Second)),
		}
	default:
		result = &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("daemon heartbeat is fresh (%s ago), count %d", age.Round(time.Second), state.HeartbeatCount),
		}
	}

	// Only refresh the baseline when the count has advanced, or when there
	// was none yet, or when the existing one is already old enough to have
	// given the daemon a fair chance to tick (which also means a stuck count
	// flagged this run is not re-flagged forever once the daemon recovers).
	// Refreshing unconditionally on every run — the previous behavior —
	// reset baselineAge to ~0 each time, so a doctor cadence shorter than
	// staleThreshold could never observe baselineAge >= staleThreshold and
	// the "not advancing" branch above could never fire.
	if !hadBaseline || countAdvanced || baselineAge >= staleThreshold {
		if writeErr := writeDaemonLivenessBaseline(statePath, daemonLivenessBaseline{
			HeartbeatCount: state.HeartbeatCount,
			RecordedAt:     time.Now(),
		}); writeErr != nil {
			return &CheckResult{
				Name:    c.Name(),
				Status:  StatusSkipped,
				Message: "unknown: could not persist baseline",
				Details: []string{writeErr.Error()},
			}
		}
	}

	return result
}

// readDaemonLivenessBaseline reads the baseline state file. The second
// return value is false when no usable baseline exists yet (missing or
// unparseable file, or a zero RecordedAt) — distinct from a zero-value
// baseline the check must never invent.
func readDaemonLivenessBaseline(path string) (daemonLivenessBaseline, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return daemonLivenessBaseline{}, false
	}
	var baseline daemonLivenessBaseline
	if err := json.Unmarshal(data, &baseline); err != nil || baseline.RecordedAt.IsZero() {
		return daemonLivenessBaseline{}, false
	}
	return baseline, true
}

// writeDaemonLivenessBaseline persists the baseline state file atomically,
// creating its .runtime parent directory if needed. A torn write (crash
// mid-write) must not be read back as "no baseline" — that would silently
// reset the detection window for another staleThreshold.
func writeDaemonLivenessBaseline(path string, baseline daemonLivenessBaseline) error {
	return atomicfile.EnsureDirAndWriteJSON(path, baseline)
}
