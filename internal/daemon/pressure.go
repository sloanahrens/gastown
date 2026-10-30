package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/hoststat"
)

// PressureResult holds the outcome of a pressure check.
type PressureResult struct {
	// OK is true if spawning should proceed.
	OK bool

	// Reason describes why spawning was blocked (empty if OK).
	Reason string

	// CPUIdle is the percent of all cores idle at check time. It is set only
	// when the CPU tier ran and top could be read; zero otherwise.
	CPUIdle float64

	// CPUReadErr is why the CPU tier could not read the host, nil when it
	// read it or did not run. An unreadable host does not hold the spawn.
	CPUReadErr error

	// MemAvailableGB is approximate available memory in GB.
	MemAvailableGB float64

	// ActiveSessions is the count of active Claude agent sessions.
	ActiveSessions int
}

// checkPressure evaluates system load and session concurrency to decide
// whether spawning a new agent session is safe. It checks:
//
//  1. CPU pressure: share of the host's cores busy (100% minus CPU idle)
//     vs threshold. Not the load average: macOS counts runnable threads in
//     it, so a 24-core host with half its CPU idle reads load 30 (gt-kc8v1).
//  2. Memory pressure: available memory vs minimum threshold.
//  3. Session concurrency: active tmux sessions vs maximum cap.
//
// Infrastructure agents (deacon, witness) should NOT be gated by
// pressure—they are the monitoring/recovery layer. Only gate:
//   - Polecats (dispatchQueuedWork, crash restarts)
//   - Refineries
//   - Dogs
func (d *Daemon) checkPressure(_ string) PressureResult {
	cfg := d.loadOperationalConfig().GetDaemonConfig()

	cpuThreshold := cfg.PressureCPUThresholdV()
	memThreshold := cfg.PressureMemThresholdGBV()
	maxSessions := cfg.PressureMaxSessionsV()

	// All checks disabled (default) — skip entirely, no subprocess calls.
	if cpuThreshold <= 0 && memThreshold <= 0 && maxSessions <= 0 {
		return PressureResult{OK: true}
	}

	var result PressureResult
	result.OK = true

	// Tier 1: CPU pressure (busy share of the host's cores). The threshold
	// keeps its per-core scale: 1.0 is every core busy. A top that cannot be
	// read is no evidence of a busy host, so it does not hold the spawn; the
	// daemon log says why it could not be read, so a gate that has gone blind
	// does not look like a host that is fine.
	if cpuThreshold > 0 {
		d.warnPressureScale(cpuThreshold)
		idle, err := d.hostCPUIdle()
		if err != nil {
			result.CPUReadErr = err
			d.logger.Printf("cpu pressure: not holding the spawn, the CPU tier could not read the host: %v", err)
		} else {
			result.CPUIdle = idle
			busyPerCore := (100 - idle) / 100
			if busyPerCore > cpuThreshold {
				result.OK = false
				result.Reason = fmt.Sprintf("cpu pressure: %.1f%% CPU idle, busy/core %.2f exceeds threshold %.2f", idle, busyPerCore, cpuThreshold)
				return result
			}
		}
	}

	// Tier 1: Memory pressure
	if memThreshold > 0 {
		result.MemAvailableGB = availableMemoryGB()
		if result.MemAvailableGB > 0 && result.MemAvailableGB < memThreshold {
			result.OK = false
			result.Reason = fmt.Sprintf("memory pressure: %.1fGB available, minimum %.1fGB", result.MemAvailableGB, memThreshold)
			return result
		}
	}

	// Tier 2: Session concurrency cap
	if maxSessions > 0 {
		active, err := d.countAgentSessions()
		if err != nil {
			// An unlistable tmux is no evidence of an empty town: counting it as
			// zero sessions would spawn past the cap, so the cap holds the spawn.
			result.OK = false
			result.Reason = fmt.Sprintf("session cap: could not count sessions against max %d: %v", maxSessions, err)
			d.logger.Printf("session pressure: holding the spawn: %v", err)
			return result
		}
		result.ActiveSessions = active
		if result.ActiveSessions >= maxSessions {
			result.OK = false
			result.Reason = fmt.Sprintf("session cap: %d active sessions, max %d", result.ActiveSessions, maxSessions)
			return result
		}
	}

	return result
}

// warnPressureScale says once that a CPU threshold past 1.0 can never trip: it
// is the busy share of the host's cores, no longer a load average per core, so
// a value tuned for the old scale leaves the tier dead. Run calls it at start
// so the warning does not wait for the first spawn check (gt-kc8v1).
func (d *Daemon) warnPressureScale(cpuThreshold float64) {
	if cpuThreshold <= 1 {
		return
	}
	d.pressureScaleWarn.Do(func() {
		d.logger.Printf("pressure_cpu_threshold %.2f can never trip: it is the busy share of the host's cores (at most 1.0), no longer a load average per core; a value tuned for the load average needs re-setting (gt-kc8v1)", cpuThreshold)
	})
}

// hostCPUIdle is the host's CPU idle percent, or the reason it could not be
// read. Tests answer it through d.cpuIdleFn, or through d.hostTop when the
// sample and parse of top's output is what they exercise.
func (d *Daemon) hostCPUIdle() (float64, error) {
	if d.cpuIdleFn != nil {
		return d.cpuIdleFn()
	}
	if d.hostTop != nil {
		return hoststat.SampleCPUIdleTop(context.Background(), d.hostTop)
	}
	return hoststat.SampleCPUIdle(context.Background())
}

// countAgentSessions counts active tmux sessions that belong to Gas Town agents.
// Uses the town's tmux socket so it only counts sessions for this town. A list
// error is returned, not counted as zero: "no server" already reads as an empty
// list, so an error is a real tmux failure.
func (d *Daemon) countAgentSessions() (int, error) {
	sessions, err := d.tmux.ListSessions()
	if err != nil {
		return 0, fmt.Errorf("listing tmux sessions: %w", err)
	}

	count := 0
	for _, name := range sessions {
		if isAgentSession(name) {
			count++
		}
	}
	return count, nil
}

// isAgentSession returns true if the tmux session name looks like a Gas Town agent.
// Agent sessions use prefixed names (e.g., "rig-crew-max", "rig-polecat-foo").
func isAgentSession(name string) bool {
	// Agent sessions contain role markers
	for _, marker := range []string{
		constants.RolePolecat,
		constants.RoleCrew,
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// loadAverage1 returns the 1-minute load average.
// Falls back to 0 if unavailable (effectively disabling the check).
func loadAverage1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		// macOS: use sysctl
		return loadAverage1Sysctl()
	}
	var load1 float64
	_, _ = fmt.Sscanf(string(data), "%f", &load1)
	return load1
}

// EstimateLoad1 gives callers outside this package the raw 1-minute load
// average, one instant sysctl/proc read. It is not normalized by core count:
// macOS loadavg counts processes in uninterruptible wait (disk/IO), not just
// CPU-runnable ones, so on a many-core host with bursty non-CPU load (e.g. a
// Dolt server) load1/NumCPU reports "busy" long before the CPU actually is.
// Gates that need to match the operator's own "don't retry above load1 60"
// rule of thumb read this directly instead (gt-e6xh).
func EstimateLoad1() float64 {
	return loadAverage1()
}
