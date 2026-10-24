package doctor

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/util"
)

// DoltStrayServersCheck detects the Dolt servers and port files a beads
// workspace started for itself, outside the town's own server (gt-4js4f).
//
// The orphan scan reports such a server too, as "unexpected" after 60s; what it
// cannot say is which workspace owns the server, or that the port file beside
// it sends that workspace's bd calls to it.
//
// Read-only: it reports, and leaves the port file in place. Remediation is
// `kill <pid>` for a stray server — a pid, never a pattern, which could match
// another session's server (gt-gyw5w).
type DoltStrayServersCheck struct {
	BaseCheck

	// findServers lists the running dolt sql-server processes; nil is
	// util.FindDoltServers.
	findServers func() ([]util.DoltServerProcess, error)

	// dial reports whether a TCP connection to addr succeeds; nil dials.
	dial func(addr string) error
}

// NewDoltStrayServersCheck creates a new stray-workspace-server check.
func NewDoltStrayServersCheck() *DoltStrayServersCheck {
	return &DoltStrayServersCheck{
		BaseCheck: BaseCheck{
			CheckName:        "dolt-stray-servers",
			CheckDescription: "Detect Dolt servers a beads workspace started for itself, and port files pointing at them",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run reports a running `dolt sql-server` whose --config lies under a .beads
// directory and serves a port other than the town's, and a workspace port file
// whose port nobody serves or whose server lacks the database that workspace's
// bd commands use.
func (c *DoltStrayServersCheck) Run(ctx *CheckContext) *CheckResult {
	findServers := c.findServers
	if findServers == nil {
		findServers = util.FindDoltServers
	}

	servers, err := findServers()
	if err != nil {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusSkipped,
			Message:  "unknown: could not list processes",
			Details:  []string{err.Error()},
			Category: c.CheckCategory,
		}
	}

	townPort := doltserver.DefaultConfig(ctx.TownRoot).Port
	var details []string

	strays := 0
	for _, s := range servers {
		_, workspace, ok := util.BeadsDirFromConfigPath(s.ConfigPath)
		if !ok {
			continue // the town's own server runs from .dolt-data, not a .beads dir
		}
		// A workspace pointed at the town's port is what the town wants; an
		// unreadable port (0) is not the town's, so it is reported.
		if townPort > 0 && s.Port == townPort {
			continue
		}
		strays++
		details = append(details, fmt.Sprintf(
			"Stray Dolt server for workspace %s: PID %d on port %s (config %s) — the town's server is on port %s; stop it with 'kill %d'",
			workspace, s.PID, displayPort(s.Port), s.ConfigPath, displayPort(townPort), s.PID))
	}

	unusablePortFiles := 0
	for _, beadsDir := range c.workspaceBeadsDirs(ctx, servers) {
		detail, ok := c.stalePortFile(beadsDir, servers)
		if !ok {
			continue
		}
		unusablePortFiles++
		details = append(details, detail)
	}

	if strays == 0 && unusablePortFiles == 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  "No stray workspace Dolt servers or unusable workspace port files found",
			Category: c.CheckCategory,
		}
	}

	return &CheckResult{
		Name:   c.Name(),
		Status: StatusWarning,
		Message: fmt.Sprintf("%d stray workspace Dolt server(s), %d workspace port file(s) with no usable server",
			strays, unusablePortFiles),
		Details:  details,
		Category: c.CheckCategory,
	}
}

// workspaceBeadsDirs lists the beads directories whose port files this check
// reads: the town's, every registered rig's, and every workspace a running
// server named through its --config — the last is how a workspace outside the
// town (a plain ~/.claude) enters the scan at all.
func (c *DoltStrayServersCheck) workspaceBeadsDirs(ctx *CheckContext, servers []util.DoltServerProcess) []string {
	seen := make(map[string]bool)
	var dirs []string
	add := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}

	add(filepath.Join(ctx.TownRoot, ".beads"))
	for rigName := range loadRigNames(filepath.Join(ctx.TownRoot, "mayor", "rigs.json")) {
		add(filepath.Join(ctx.TownRoot, rigName, "mayor", "rig", ".beads"))
		add(filepath.Join(ctx.TownRoot, rigName, ".beads"))
	}
	// Top-level directories catch a rig that rigs.json does not name yet.
	if entries, err := os.ReadDir(ctx.TownRoot); err == nil {
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				add(filepath.Join(ctx.TownRoot, e.Name(), ".beads"))
			}
		}
	}
	for _, s := range servers {
		if beadsDir, _, ok := util.BeadsDirFromConfigPath(s.ConfigPath); ok {
			add(beadsDir)
		}
	}
	return dirs
}

// stalePortFile judges beadsDir/dolt-server.port: it reports a port nobody
// accepts connections on, and a port whose server does not hold the database
// this workspace's bd commands use. ok is false when there is no port file to
// judge, or when a reachable port cannot be attributed to a Dolt server whose
// databases we can name — a guess there would accuse a healthy workspace.
func (c *DoltStrayServersCheck) stalePortFile(beadsDir string, servers []util.DoltServerProcess) (string, bool) {
	portPath := filepath.Join(beadsDir, "dolt-server.port")
	data, err := os.ReadFile(portPath) //nolint:gosec // G304: port file under a workspace beads dir
	if err != nil {
		return "", false
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port <= 0 {
		return "", false // unparsable: nothing this check can say about it
	}

	if !c.portOpen(port) {
		return fmt.Sprintf(
			"Stale port file %s: port %d accepts no connections — a bd call in that workspace trusts it and can start another stray server",
			portPath, port), true
	}

	database := workspaceDatabase(beadsDir)
	listening := serversOnPort(servers, port)
	if database == "" || len(listening) == 0 {
		return "", false // reachable, but not attributable: report nothing
	}
	for _, s := range listening {
		if dataDirHasDatabase(s.DataDir, database) {
			return "", false
		}
	}
	served := listening[0]
	return fmt.Sprintf(
		"Port file %s: port %d is served by PID %d from %s, which holds no %q database — bd in that workspace fails with \"database not found\"",
		portPath, port, served.PID, displayDataDir(served.DataDir), database), true
}

// portOpen reports whether something accepts TCP connections on the local
// machine's port.
func (c *DoltStrayServersCheck) portOpen(port int) bool {
	addr := doltServerAddr("127.0.0.1", port)
	if c.dial != nil {
		return c.dial(addr) == nil
	}
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// serversOnPort narrows a server listing to those serving port.
func serversOnPort(servers []util.DoltServerProcess, port int) []util.DoltServerProcess {
	var matched []util.DoltServerProcess
	for _, s := range servers {
		if s.Port == port {
			matched = append(matched, s)
		}
	}
	return matched
}

// workspaceDatabase names the database a workspace's bd commands select, from
// its bd metadata. "" means the workspace is not a server-mode Dolt store, so
// there is no database to hold it to.
func workspaceDatabase(beadsDir string) string {
	cfg, ok := doltserver.ReadBeadsRuntimeConfig(beadsDir)
	if !ok {
		return ""
	}
	return cfg.Database
}

// dataDirHasDatabase reports whether a Dolt server's data directory holds db,
// by the rule doltserver lists databases with: a directory of the same name
// holding .dolt/noms/manifest, so a phantom directory does not count. A server
// with no discoverable data directory is never accused of missing one.
func dataDirHasDatabase(dataDir, db string) bool {
	if dataDir == "" || db == "" {
		return true
	}
	return doltserver.DataDirHasDatabase(dataDir, db)
}

// displayPort keeps a 0 port (the town names no endpoint) readable in a detail
// line that would otherwise say "port 0".
func displayPort(port int) string {
	if port <= 0 {
		return "(none)"
	}
	return strconv.Itoa(port)
}

// displayDataDir keeps a server with no discoverable data directory readable.
func displayDataDir(dir string) string {
	if dir == "" {
		return "(unknown data dir)"
	}
	return dir
}
