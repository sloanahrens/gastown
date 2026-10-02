package doctor

import (
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// DoltServerPatrolCheck detects the silent Dolt-outage gap: the Dolt SQL
// server is not reachable AND the daemon's dolt_server patrol (which owns
// Dolt lifecycle detection and restart) is not enabled in mayor/daemon.json.
//
// This is detection only — it never starts, stops, or restarts Dolt. When the
// condition is present it surfaces a config gap that would otherwise be silent:
// the daemon heartbeats fine, agents keep running, but nothing notices or
// recovers a Dolt death until an agent manually runs `gt dolt start`.
//
// Enabling the dolt_server patrol (auto-restart) is an operator decision that
// requires sign-off; this check only makes the gap visible so that decision can
// be made with evidence.
type DoltServerPatrolCheck struct {
	BaseCheck

	// dial reports whether a TCP connection to addr succeeds; nil dials.
	dial func(addr string) error
}

// NewDoltServerPatrolCheck creates a check that surfaces the Dolt-down +
// dolt_server-patrol-disabled condition.
func NewDoltServerPatrolCheck() *DoltServerPatrolCheck {
	return &DoltServerPatrolCheck{
		BaseCheck: BaseCheck{
			CheckName:        "dolt-server-patrol",
			CheckDescription: "Detect Dolt server down while the daemon dolt_server patrol is disabled",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run reports the gap when Dolt is unreachable and no daemon patrol is watching
// it. It is a no-op (OK) when Dolt is reachable, and does not alarm when the
// dolt_server patrol is already enabled (the daemon will detect and recover).
func (c *DoltServerPatrolCheck) Run(ctx *CheckContext) *CheckResult {
	// If Dolt is reachable, there is nothing to surface.
	if c.isDoltReachable(ctx.TownRoot) {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  "Dolt server reachable",
			Category: c.CheckCategory,
		}
	}

	// Dolt is down. Determine whether the daemon is watching it.
	patrolEnabled, cfgPath, cfgErr := c.doltServerPatrolEnabled(ctx.TownRoot)
	if cfgErr != nil {
		// We cannot read the patrol config, so we cannot confirm the gap.
		// Report the outage but avoid asserting a config gap we could not verify.
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Dolt server unreachable (could not verify dolt_server patrol state)",
			Details: []string{cfgErr.Error()},
			FixHint: "Run 'gt dolt start' to restore the Dolt server, then re-run this check",
		}
	}

	if patrolEnabled {
		// The daemon owns Dolt lifecycle and will detect/recover. Nothing to
		// surface here — the outage is being handled.
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  "Dolt server unreachable, but dolt_server patrol is enabled (daemon will detect and recover)",
			Category: c.CheckCategory,
		}
	}

	// The gap: Dolt is down and nothing is watching it.
	relPath, _ := filepath.Rel(ctx.TownRoot, cfgPath)
	return &CheckResult{
		Name:   c.Name(),
		Status: StatusWarning,
		Message: "Dolt server is down and the daemon dolt_server patrol is NOT enabled — " +
			"no automatic detection or recovery will run",
		Details: []string{
			"bd commands will fail or create isolated local databases (split-brain risk)",
			"Recovery currently depends on an agent manually running 'gt dolt start'",
			"The dolt_server patrol key is absent or disabled in " + relPath,
		},
		FixHint: "To enable daemon-managed Dolt detection/restart (requires operator sign-off), " +
			"add a dolt_server patrol to " + relPath + " and restart the daemon. " +
			"Until then, restore the server manually with 'gt dolt start'",
		Category: c.CheckCategory,
	}
}

// isDoltReachable reports whether the local Dolt SQL server is accepting
// connections on its configured port.
func (c *DoltServerPatrolCheck) isDoltReachable(townRoot string) bool {
	cfg := doltserver.DefaultConfig(townRoot)
	addr := doltServerAddr(cfg.Host, cfg.Port)
	if c.dial != nil {
		return c.dial(addr) == nil
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// doltServerAddr is the host:port the Dolt server listens on.
func doltServerAddr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// doltServerPatrolEnabled reports whether the dolt_server patrol is enabled in
// mayor/daemon.json, along with the config file path. The third return value is
// an error when the config could not be read or parsed.
func (c *DoltServerPatrolCheck) doltServerPatrolEnabled(townRoot string) (bool, string, error) {
	cfgPath := config.DaemonPatrolConfigPath(townRoot)
	cfg, err := config.LoadDaemonPatrolConfig(cfgPath)
	if errors.Is(err, config.ErrNotFound) {
		// No daemon config at all: the dolt_server patrol is not enabled.
		return false, cfgPath, nil
	}
	if err != nil {
		return false, cfgPath, err
	}
	if cfg.Patrols == nil || cfg.Patrols.DoltServer == nil {
		return false, cfgPath, nil
	}
	return cfg.Patrols.DoltServer.Enabled, cfgPath, nil
}
