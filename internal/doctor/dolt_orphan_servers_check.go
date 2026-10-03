package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/util"
)

// DoltOrphanServersCheck detects orphaned test 'dolt sql-server' processes —
// left behind when an embedded-dolt test suite is killed at its timeout and
// its shared/per-test server is reparented to init without teardown running
// (gt-twil) — plus the stale beads-bd-tests-* temp dirs those dead servers
// leave in $TMPDIR.
type DoltOrphanServersCheck struct {
	FixableCheck
	orphans   []util.DoltOrphanServer
	staleDirs []string

	// findOrphans lists the dolt sql-server processes that are not the
	// town's own; nil is util.FindOrphanDoltServers (reads the process table).
	findOrphans func(townRoot string) ([]util.DoltOrphanServer, error)
	// findStaleDirs lists the stale beads-bd-tests-* temp dirs; nil is
	// util.FindStaleBeadsTestTempDirs.
	findStaleDirs func() ([]string, error)
}

// NewDoltOrphanServersCheck creates a new dolt-orphan-servers check.
func NewDoltOrphanServersCheck() *DoltOrphanServersCheck {
	return &DoltOrphanServersCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "dolt-orphan-servers",
				CheckDescription: "Detect orphaned test 'dolt sql-server' processes and their stale temp dirs",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// Run scans for dolt sql-server processes other than the town's own server
// and for stale beads-bd-tests-* temp dirs whose server has already exited.
func (c *DoltOrphanServersCheck) Run(ctx *CheckContext) *CheckResult {
	c.orphans = nil
	c.staleDirs = nil

	findOrphans, findStaleDirs := c.findOrphans, c.findStaleDirs
	if findOrphans == nil {
		findOrphans = util.FindOrphanDoltServers
	}
	if findStaleDirs == nil {
		findStaleDirs = util.FindStaleBeadsTestTempDirs
	}

	orphans, err := findOrphans(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not list processes",
			Details: []string{err.Error()},
		}
	}
	c.orphans = orphans

	// Best-effort — a temp-dir read failure shouldn't block reporting orphan
	// processes, which is the higher-value half of this check.
	if staleDirs, err := findStaleDirs(); err == nil {
		c.staleDirs = staleDirs
	}

	if len(c.orphans) == 0 && len(c.staleDirs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No orphaned dolt sql-server processes or stale test temp dirs found",
		}
	}

	var details []string
	reapable := 0
	// Name the remedy per pid: the town's port and data-dir tell an imposter
	// (kill-imposters) from a leak that command cannot reach (`kill <pid>`).
	townCfg := doltserver.DefaultConfig(ctx.TownRoot)
	for _, o := range c.orphans {
		if o.Reason == "orphan" {
			reapable++
		}
		remedy := util.StrayDoltRemedy(util.StrayDoltProcess{PID: o.PID, Port: o.Port, DataDir: o.DataDir}, townCfg.Port, townCfg.DataDir)
		details = append(details, fmt.Sprintf(
			"PID %d (ppid %d, %s, %ds old, port %d, data-dir %s): %s — remedy: %s",
			o.PID, o.PPID, o.Reason, o.Age, o.Port, dataDirOrNone(o.DataDir), o.ConfigPath, remedy))
	}
	for _, d := range c.staleDirs {
		details = append(details, "Stale test temp dir (server gone): "+d)
	}

	return &CheckResult{
		Name:   c.Name(),
		Status: StatusWarning,
		Message: fmt.Sprintf("%d orphaned dolt sql-server process(es) (%d auto-fixable), %d stale test temp dir(s)",
			len(c.orphans), reapable, len(c.staleDirs)),
		Details: details,
		FixHint: "Run 'gt doctor fix dolt-orphan-servers' to SIGTERM orphaned servers and remove stale test temp dirs",
	}
}

// dataDirOrNone renders a blank data-dir as "(none)" so the detail line stays
// readable when a stray server's argv named neither --data-dir nor --config.
func dataDirOrNone(dir string) string {
	if dir == "" {
		return "(none)"
	}
	return dir
}

// DestructiveFix marks this repair as destructive (gt-638go.3): it SIGTERMs orphaned Dolt servers.
func (c *DoltOrphanServersCheck) DestructiveFix() bool { return true }

// Fix sends SIGTERM to every orphan tagged "orphan" (never "unexpected" —
// those may be a developer's own manual server) and removes stale test temp
// dirs found during Run.
func (c *DoltOrphanServersCheck) Fix(ctx *CheckContext) error {
	if len(c.orphans) > 0 {
		util.ReapOrphanDoltServers(c.orphans)
	}
	if len(c.staleDirs) > 0 {
		if _, err := util.RemoveStaleBeadsTestTempDirs(c.staleDirs); err != nil {
			return err
		}
	}
	return nil
}
