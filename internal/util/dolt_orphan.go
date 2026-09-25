//go:build !windows

package util

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/workspace"
)

// doltOrphanMinAge is the minimum age (in seconds) a dolt sql-server process
// or beads-bd-tests-* temp dir must be before it is treated as orphaned.
// Mirrors minOrphanAge's purpose: give a server that is still starting up a
// chance to write its own state before we act on it.
const doltOrphanMinAge = 60

// beadsTestTempDirPrefix is the directory name prefix embedded-dolt test
// suites use for their scratch config/data dirs, both the per-test shape
// (beads-bd-tests-N/.../dolt-server-config.yaml) and the shared-server shape
// (beads-bd-tests-N/.../shared-server/dolt-server-config.yaml).
const beadsTestTempDirPrefix = "beads-bd-tests-"

// DoltOrphanServer is a `dolt sql-server` process that is not the town's own
// server (daemon/dolt.pid) and matches the shape a killed or timed-out
// embedded-dolt test suite leaves behind: reparented to init/launchd (PPID
// <= 1) or running from a beads-bd-tests-* scratch config. Either shape means
// teardown never ran — the suite was killed (e.g. its own timeout) before it
// could stop its server (gt-twil).
type DoltOrphanServer struct {
	PID        int
	PPID       int    // Parent pid at scan time; <= 1 means reparented to launchd/init
	ConfigPath string // --config value from argv, or "" if not present
	Age        int    // seconds, from ps etime
	Reason     string // "orphan" (auto-fixable) or "unexpected" (reported only)
}

// DoltOrphanReapResult describes what happened when a DoltOrphanServer was signaled.
type DoltOrphanReapResult struct {
	Process DoltOrphanServer
	Signal  string // "SIGTERM" or "SKIPPED"
	Error   error
}

// doltProcEntry is one process-table row relevant to dolt sql-server detection.
type doltProcEntry struct {
	PID   int
	PPID  int
	Etime string
	Args  string
}

// doltPSSnapshot reads the process table with full argv, needed to identify
// `dolt sql-server` invocations and their --config path.
func doltPSSnapshot() ([]doltProcEntry, error) {
	out, err := exec.Command("ps", "-eo", "pid,ppid,etime,args").Output()
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	return parseDoltProcessTable(string(out)), nil
}

// parseDoltProcessTable parses `ps -eo pid,ppid,etime,args` output. args may
// itself contain whitespace, so only the first three fields are fixed; the
// remainder is rejoined as args.
func parseDoltProcessTable(out string) []doltProcEntry {
	var entries []doltProcEntry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue // Header line or invalid PID
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		entries = append(entries, doltProcEntry{
			PID:   pid,
			PPID:  ppid,
			Etime: fields[2],
			Args:  strings.Join(fields[3:], " "),
		})
	}
	return entries
}

// isDoltSQLServerArgs reports whether argv is a `dolt sql-server` invocation:
// the first token's basename is "dolt" and the second is "sql-server".
func isDoltSQLServerArgs(args string) bool {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return false
	}
	return filepath.Base(fields[0]) == "dolt" && fields[1] == "sql-server"
}

// doltSQLServerConfigPath extracts the --config flag's value from a dolt
// sql-server argv string, or "" if not present.
func doltSQLServerConfigPath(args string) string {
	fields := strings.Fields(args)
	for i, f := range fields {
		if f == "--config" && i+1 < len(fields) {
			return fields[i+1]
		}
		if strings.HasPrefix(f, "--config=") {
			return strings.TrimPrefix(f, "--config=")
		}
	}
	return ""
}

// isBeadsTestConfigPath reports whether a config path is under a
// beads-bd-tests-* scratch dir (either the per-test or shared-server shape).
func isBeadsTestConfigPath(configPath string) bool {
	return strings.Contains(configPath, beadsTestTempDirPrefix)
}

// townDoltServerPID reads the town's own dolt sql-server PID from
// daemon/dolt.pid (see internal/doltserver.DefaultConfig). Returns 0 if the
// file is missing or unreadable, in which case callers cannot confirm any
// dolt sql-server as "the" town server and every one found is reported.
func townDoltServerPID(townRoot string) int {
	data, err := os.ReadFile(filepath.Join(townRoot, "daemon", "dolt.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// classifyDoltOrphan builds a DoltOrphanServer from a process-table entry
// already known to be a non-town `dolt sql-server`, or returns ok=false if
// it doesn't meet the minimum-age floor.
func classifyDoltOrphan(e doltProcEntry) (DoltOrphanServer, bool) {
	age, err := parseEtime(e.Etime)
	if err != nil || age < doltOrphanMinAge {
		return DoltOrphanServer{}, false
	}

	cfg := doltSQLServerConfigPath(e.Args)
	// PPID <= 1 (reparented to launchd/init) is only orphan evidence when
	// there's no config path contradicting it: a daemonized production
	// server is *also* reparented to PPID 1 once its launching shell exits,
	// but it runs from a real, persistent --config, not a test scratch dir
	// or no --config at all. Without this, PPID<=1 alone would tag any
	// daemonized server "orphan" — the shape Fix() SIGTERMs (gt-l7za1).
	reason := "unexpected"
	switch {
	case isBeadsTestConfigPath(cfg):
		reason = "orphan"
	case cfg == "" && e.PPID <= 1:
		reason = "orphan"
	}

	return DoltOrphanServer{
		PID:        e.PID,
		PPID:       e.PPID,
		ConfigPath: cfg,
		Age:        age,
		Reason:     reason,
	}, true
}

// townServerPIDs returns the set of PIDs considered "the town's own dolt
// sql-server": townRoot's daemon/dolt.pid, plus — when the hermetic test
// harness has recorded one (workspace.ForbiddenTownRoot) — the live town
// root's daemon/dolt.pid too. A hermetic/sandbox townRoot has no
// daemon/dolt.pid of its own (townDoltServerPID returns 0), so without this
// second source the real production server, which the process table scan
// still sees since it isn't sandboxed, has nothing to match against and is
// misclassified as an orphan (gt-l7za1).
func townServerPIDs(townRoot string) map[int]bool {
	pids := make(map[int]bool, 2)
	if pid := townDoltServerPID(townRoot); pid != 0 {
		pids[pid] = true
	}
	if forbidden := workspace.ForbiddenTownRoot(); forbidden != "" && forbidden != townRoot {
		if pid := townDoltServerPID(forbidden); pid != 0 {
			pids[pid] = true
		}
	}
	return pids
}

// FindOrphanDoltServers scans the process table for `dolt sql-server`
// processes other than the town's own server(s) (see townServerPIDs) whose
// shape matches an orphaned embedded-dolt test server: reparented to
// init/launchd (PPID <= 1) or running from a beads-bd-tests-* scratch config
// (either the per-test or shared-server layout). Any other non-town dolt
// sql-server is also returned, tagged Reason "unexpected", so it is surfaced
// without being auto-signaled — it may be a developer's own manual server.
func FindOrphanDoltServers(townRoot string) ([]DoltOrphanServer, error) {
	expected := townServerPIDs(townRoot)

	entries, err := doltPSSnapshot()
	if err != nil {
		return nil, err
	}

	var orphans []DoltOrphanServer
	for _, e := range entries {
		if !isDoltSQLServerArgs(e.Args) {
			continue
		}
		if expected[e.PID] {
			continue // the town's own server
		}
		if o, ok := classifyDoltOrphan(e); ok {
			orphans = append(orphans, o)
		}
	}
	return orphans, nil
}

// isStillDoltSQLServer re-checks that pid is still a live dolt sql-server
// process, used as a TOCTOU guard immediately before signaling.
func isStillDoltSQLServer(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return false
	}
	return isDoltSQLServerArgs(strings.TrimSpace(string(out)))
}

// ReapOrphanDoltServers sends SIGTERM to every orphan whose Reason is
// "orphan" (the auto-fixable shape). "unexpected" entries are left alone —
// they are reported by FindOrphanDoltServers but not signaled here, since an
// unrecognized second server might be a developer's own instance.
//
// SIGTERM only, never SIGKILL escalation and never SIGQUIT: dolt sql-server
// treats SIGQUIT as a request to dump and exit, which is destructive noise
// for a process we merely want gone, and a stuck server here is not on the
// same hot path as the Claude-orphan reaper that escalates.
func ReapOrphanDoltServers(orphans []DoltOrphanServer) []DoltOrphanReapResult {
	var results []DoltOrphanReapResult
	for _, o := range orphans {
		if o.Reason != "orphan" {
			continue
		}
		// TOCTOU guard: re-verify immediately before signaling — the process
		// may have exited or been replaced between scan and now.
		if !isStillDoltSQLServer(o.PID) {
			continue
		}
		err := syscall.Kill(o.PID, syscall.SIGTERM)
		if err != nil && err == syscall.ESRCH {
			continue // already gone
		}
		results = append(results, DoltOrphanReapResult{
			Process: o,
			Signal:  "SIGTERM",
			Error:   err,
		})
	}
	return results
}

// liveDoltConfigDirs returns the set of beads-bd-tests-* directories
// currently referenced by a live dolt sql-server's --config path (town
// server included — it is never itself under a test temp dir, but excluding
// it isn't necessary since the prefix match already scopes this).
func liveDoltConfigDirs() (map[string]bool, error) {
	entries, err := doltPSSnapshot()
	if err != nil {
		return nil, err
	}

	dirs := make(map[string]bool)
	for _, e := range entries {
		if !isDoltSQLServerArgs(e.Args) {
			continue
		}
		cfg := doltSQLServerConfigPath(e.Args)
		if cfg == "" {
			continue
		}
		dir := filepath.Dir(cfg)
		for dir != "/" && dir != "." {
			if strings.HasPrefix(filepath.Base(dir), beadsTestTempDirPrefix) {
				dirs[dir] = true
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return dirs, nil
}

// FindStaleBeadsTestTempDirs finds beads-bd-tests-* directories under the OS
// temp dir whose dolt sql-server has already exited — the leftover from a
// reaped or crashed embedded-dolt test suite. A directory referenced by any
// live dolt sql-server's --config path, or modified more recently than
// doltOrphanMinAge, is never returned. Read-only: callers decide whether to
// remove what's returned.
func FindStaleBeadsTestTempDirs() ([]string, error) {
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("reading temp dir: %w", err)
	}

	liveDirs, err := liveDoltConfigDirs()
	if err != nil {
		return nil, err
	}

	var stale []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), beadsTestTempDirPrefix) {
			continue
		}
		path := filepath.Join(tmpDir, e.Name())
		if liveDirs[path] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) < time.Duration(doltOrphanMinAge)*time.Second {
			continue
		}
		stale = append(stale, path)
	}
	return stale, nil
}

// RemoveStaleBeadsTestTempDirs removes each of the given directories,
// returning those actually removed. Intended for paths returned by
// FindStaleBeadsTestTempDirs — callers must not pass arbitrary paths, since
// this performs os.RemoveAll.
func RemoveStaleBeadsTestTempDirs(dirs []string) (removed []string, err error) {
	var lastErr error
	for _, d := range dirs {
		if !strings.HasPrefix(filepath.Base(d), beadsTestTempDirPrefix) {
			continue // safety: never remove a path that isn't the expected shape
		}
		if rmErr := os.RemoveAll(d); rmErr != nil {
			lastErr = rmErr
			continue
		}
		removed = append(removed, d)
	}
	return removed, lastErr
}
