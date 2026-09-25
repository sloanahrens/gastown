//go:build !windows

package util

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/tmux"
)

// minOrphanAge is the minimum age (in seconds) a process must be before
// we consider it orphaned. This prevents race conditions with newly spawned
// processes and avoids killing legitimate short-lived subagents.
const minOrphanAge = 60

// buildChildMap builds a parent→children map from a single ps call.
// This replaces per-PID pgrep calls, reducing O(N) process spawns to O(1).
func buildChildMap() map[int][]int {
	children := make(map[int][]int)
	out, err := exec.Command("ps", "-eo", "pid,ppid").Output()
	if err != nil {
		return children
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		if pid > 0 {
			children[ppid] = append(children[ppid], pid)
		}
	}
	return children
}

// addDescendants adds all descendant PIDs of a process to the set using
// a pre-built child map (no additional process spawns).
func addDescendants(parentPID int, childMap map[int][]int, pids map[int]bool) {
	for _, pid := range childMap[parentPID] {
		if !pids[pid] {
			pids[pid] = true
			addDescendants(pid, childMap, pids)
		}
	}
}

// getTmuxSessionPIDs returns a set of PIDs belonging to ANY tmux session
// across ALL tmux sockets on this machine.
//
// This prevents killing Claude processes that are running in tmux sessions,
// even if they temporarily show TTY "?" during startup or session transitions.
//
// CRITICAL: We protect ALL tmux sessions on ALL sockets. When multiple Gas Town
// instances run on the same machine, each uses its own tmux socket. A single-socket
// query would miss processes in other towns' sessions, causing cross-town kills.
func getTmuxSessionPIDs() map[int]bool {
	pids := make(map[int]bool)

	// Build process tree once, shared across all socket scans
	childMap := buildChildMap()

	// Scan all tmux sockets in the socket directory.
	// Each Gas Town instance (and any personal tmux servers) gets its own socket.
	socketDir := tmux.SocketDir()
	entries, err := os.ReadDir(socketDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			socketPath := filepath.Join(socketDir, entry.Name())
			collectPanePIDs(socketPath, childMap, pids)
		}
	}

	// Also query the current town's socket via BuildCommand as a fallback.
	// This handles non-standard socket locations (e.g. GT_TMUX_SOCKET override).
	out, err := tmux.BuildCommand("list-panes", "-a", "-F", "#{pane_pid}").Output()
	if err == nil {
		for _, pidStr := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if pid, err := strconv.Atoi(pidStr); err == nil && pid > 0 {
				pids[pid] = true
				addDescendants(pid, childMap, pids)
			}
		}
	}

	return pids
}

// collectPanePIDs queries a single tmux socket for all pane PIDs and adds them
// (plus their descendant processes) to the protection set.
func collectPanePIDs(socketPath string, childMap map[int][]int, pids map[int]bool) {
	out, err := exec.Command("tmux", "-S", socketPath, "list-panes", "-a", "-F", "#{pane_pid}").Output()
	if err != nil {
		return
	}
	for _, pidStr := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, err := strconv.Atoi(pidStr); err == nil && pid > 0 {
			pids[pid] = true
			addDescendants(pid, childMap, pids)
		}
	}
}

// getACPSessionPIDs returns a set of PIDs belonging to active ACP (Agent Client Protocol) sessions.
// ACP sessions run outside of tmux and would otherwise be killed by the zombie-scan.
// We protect the ACP proxy process and all its children (including the opencode agent).
func getACPSessionPIDs() map[int]bool {
	pids := make(map[int]bool)

	// Find all town roots by looking for mayor-acp.pid files
	// Common locations: ~/gt, ~/town-*, etc.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return pids
	}

	// Build process tree once
	childMap := buildChildMap()

	// Check the primary town root (~/gt)
	pidPath := filepath.Join(homeDir, "gt", "mayor", "mayor-acp.pid")
	if data, err := os.ReadFile(pidPath); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			// Check if process is still alive
			if processExists(pid) {
				pids[pid] = true
				// Add all child processes (including the opencode agent)
				addDescendants(pid, childMap, pids)
			}
		}
	}

	return pids
}

// sigkillGracePeriod is how long (in seconds) we wait after sending SIGTERM
// before escalating to SIGKILL. If a process was sent SIGTERM and is still
// around after this period, we use SIGKILL on the next cleanup cycle.
const sigkillGracePeriod = 60

// signalState tracks what signal was last sent to a PID and when.
type signalState struct {
	Signal    string    // "SIGTERM" or "SIGKILL"
	Timestamp time.Time // When the signal was sent
}

// stateFileDir returns the directory for state files.
func stateFileDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/tmp"
	}
	return dir
}

// loadSignalState reads a state file and returns the current signal state
// for each tracked PID. Automatically cleans up entries for dead processes.
// Uses file locking to prevent concurrent access.
func loadSignalState(filename string) map[int]signalState {
	state := make(map[int]signalState)

	path := filepath.Join(stateFileDir(), filename)

	// Acquire coordination lock (serializes with saveSignalState)
	unlock, err := lock.FlockAcquire(path + ".flock")
	if err != nil {
		return state
	}
	defer unlock()

	f, err := os.Open(path)
	if err != nil {
		return state // File doesn't exist yet, that's fine
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 3 {
			continue
		}
		pid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		sig := parts[1]
		ts, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			continue
		}

		// Only keep if process still exists
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			state[pid] = signalState{Signal: sig, Timestamp: time.Unix(ts, 0)}
		}
	}

	return state
}

// saveSignalState writes the current signal state to a state file.
// Uses file locking to prevent concurrent access.
func saveSignalState(filename string, state map[int]signalState) error {
	path := filepath.Join(stateFileDir(), filename)

	// Acquire coordination lock (serializes with loadSignalState)
	unlock, err := lock.FlockAcquire(path + ".flock")
	if err != nil {
		return fmt.Errorf("acquiring lock: %w", err)
	}
	defer unlock()

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	for pid, s := range state {
		fmt.Fprintf(f, "%d %s %d\n", pid, s.Signal, s.Timestamp.Unix())
	}
	return nil
}

// orphanStateFile is the filename for orphan process tracking state.
const orphanStateFile = "gastown-orphan-state"

// loadOrphanState reads the orphan state file.
func loadOrphanState() map[int]signalState {
	return loadSignalState(orphanStateFile)
}

// saveOrphanState writes the orphan state file.
func saveOrphanState(state map[int]signalState) error {
	return saveSignalState(orphanStateFile, state)
}

// processExists checks if a process is still running.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// getProcessCwd returns the current working directory of a process.
// On Linux, reads /proc/<pid>/cwd. On macOS and other Unix, uses lsof.
// Returns empty string if the cwd cannot be determined.
//
// On hardened Linux kernels (Ubuntu default: kernel.yama.ptrace_scope=1),
// readlink(/proc/<pid>/cwd) fails with EACCES for non-descendant same-user
// processes. The lsof fallback handles this when lsof is installed (it may
// be setuid or hold CAP_SYS_PTRACE). If neither method works, "" is returned
// and the caller fails safe by not killing the process.
func getProcessCwd(pid int) string {
	pidStr := strconv.Itoa(pid)

	// Try /proc/<pid>/cwd first (Linux).
	// Fails on hardened kernels (ptrace_scope>=1) for non-descendant processes.
	if target, err := os.Readlink(filepath.Join("/proc", pidStr, "cwd")); err == nil {
		// Linux appends " (deleted)" when the directory has been removed.
		// Strip it so the walk-up in isInGasTownWorkspace can still match
		// the workspace root (the process is definitely orphaned if its
		// workspace was nuked).
		return strings.TrimSuffix(target, " (deleted)")
	}

	// Fallback: lsof (macOS, and Linux when /proc is restricted by ptrace_scope).
	// -a is required to AND the -p and -d conditions; without it lsof ORs them.
	// lsof may be setuid or have CAP_SYS_PTRACE, letting it succeed where
	// readlink failed. Not installed by default on Alpine or minimal Ubuntu images.
	out, err := exec.Command("lsof", "-a", "-p", pidStr, "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	// lsof -Fn output: lines starting with 'p' (pid) and 'n' (name/path)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:]
		}
	}
	return ""
}

// resolveTownRoot returns the Gas Town workspace root for a process, identified
// by walking up from its CWD looking for the mayor/town.json marker.
// Returns the workspace root path, or "" if the process is not in any workspace
// or its CWD cannot be determined.
func resolveTownRoot(pid int) string {
	cwd := getProcessCwd(pid)
	if cwd == "" {
		return ""
	}
	return resolveTownRootFromDir(cwd)
}

// resolveTownRootFromDir walks up from dir looking for mayor/town.json.
// Returns the workspace root path, or "" if not found.
func resolveTownRootFromDir(dir string) string {
	current := dir
	for {
		if _, err := os.Stat(filepath.Join(current, "mayor", "town.json")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

// isInGasTownWorkspace checks whether a process's working directory is inside
// a Gas Town workspace (identified by the mayor/town.json marker).
func isInGasTownWorkspace(pid int) bool {
	return resolveTownRoot(pid) != ""
}

// isIDEClaudeProcess checks if a Claude process was spawned by an IDE extension
// (VS Code, Cursor, etc.). IDE-launched Claude processes run with TTY "?" but
// are legitimate — they're controlled by the IDE, not orphaned from dead sessions.
func isIDEClaudeProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return false
	}
	args := string(out)
	// Check for IDE-specific paths in the executable
	if strings.Contains(args, "vscode-server") ||
		strings.Contains(args, "vscode/extensions") ||
		strings.Contains(args, ".cursor-server") ||
		strings.Contains(args, ".cursor/extensions") {
		return true
	}
	// Generic IDE detection: stream-json I/O is specific to IDE extensions
	if strings.Contains(args, "--output-format stream-json") &&
		strings.Contains(args, "--input-format stream-json") {
		return true
	}
	return false
}

// parseEtime parses ps etime format into seconds.
// Format: [[DD-]HH:]MM:SS
// Examples: "01:23" (83s), "01:02:03" (3723s), "2-01:02:03" (176523s)
func parseEtime(etime string) (int, error) {
	var days, hours, minutes, seconds int

	// Check for days component (DD-HH:MM:SS)
	if idx := strings.Index(etime, "-"); idx != -1 {
		d, err := strconv.Atoi(etime[:idx])
		if err != nil {
			return 0, fmt.Errorf("parsing days: %w", err)
		}
		days = d
		etime = etime[idx+1:]
	}

	// Split remaining by colons
	parts := strings.Split(etime, ":")
	switch len(parts) {
	case 2: // MM:SS
		m, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, fmt.Errorf("parsing minutes: %w", err)
		}
		s, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, fmt.Errorf("parsing seconds: %w", err)
		}
		minutes, seconds = m, s
	case 3: // HH:MM:SS
		h, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, fmt.Errorf("parsing hours: %w", err)
		}
		m, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, fmt.Errorf("parsing minutes: %w", err)
		}
		s, err := strconv.Atoi(parts[2])
		if err != nil {
			return 0, fmt.Errorf("parsing seconds: %w", err)
		}
		hours, minutes, seconds = h, m, s
	default:
		return 0, fmt.Errorf("unexpected etime format: %s", etime)
	}

	return days*86400 + hours*3600 + minutes*60 + seconds, nil
}

// isAgentOrphanCommName returns true if the ps "comm" field names a runtime we track for
// TTY-less orphan / zombie cleanup (matches internal/config agent presets).
func isAgentOrphanCommName(cmdLower string) bool {
	switch cmdLower {
	case "claude", "claude-code", "codex", "opencode", "cursor-agent", "agent", "copilot":
		return true
	default:
		return false
	}
}

// processEntry is one row of the process table returned by psSnapshot.
type processEntry struct {
	PID   int
	PPID  int
	TTY   string
	Comm  string
	Etime string
}

// psSnapshot reads the process table with the parent pid included.
//
// ppid is what separates an orphan from a live tool's child (gt-h1tq): a
// TTY-less claude whose parent is still running is that parent's
// responsibility, never the daemon's. The parent test MUST use this same
// snapshot — a parent that dies between two ps calls would otherwise flip the
// child's classification.
func psSnapshot() ([]processEntry, error) {
	out, err := exec.Command("ps", "-eo", "pid,ppid,tty,comm,etime").Output()
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	return parseProcessTable(string(out)), nil
}

// parseProcessTable parses `ps -eo pid,ppid,tty,comm,etime` output, skipping
// the header and any row that does not parse.
func parseProcessTable(out string) []processEntry {
	var entries []processEntry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
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
		entries = append(entries, processEntry{
			PID:   pid,
			PPID:  ppid,
			TTY:   fields[2],
			Comm:  fields[3],
			Etime: fields[4],
		})
	}
	return entries
}

// hasLiveParent reports whether the parent is still around to own this
// process. ppid 1 means reparented to launchd/init and ppid 0 means the
// kernel — neither is an owner. The tmux server is not an owner either: a
// pane's process forks directly under the server, so once its session is
// killed a survivor that ignored the resulting SIGHUP keeps the
// (still-running, unrelated-sessions-hosting) server as its ppid forever —
// "live parent" would hide that stranded process from zombie cleanup
// indefinitely. Any other ppid missing from the table has exited, which is
// exactly the orphan case the daemon exists to clean up.
func (e processEntry) hasLiveParent(live map[int]string) bool {
	comm, ok := live[e.PPID]
	return e.PPID > 1 && ok && comm != "tmux"
}

// unownedCandidate is a process table row that passed the cheap filters, with
// its age already parsed.
type unownedCandidate struct {
	processEntry
	Age int // Age in seconds
}

// unownedCandidates applies the cheap, pure filters shared by both scan paths:
// TTY-less, a tracked agent comm, older than minOrphanAge, not protected by a
// tmux/ACP session, and owned by nobody — parent reparented to launchd/init or
// gone from the table.
//
// A headless child of a live parent is deliberately NOT a candidate: the
// parent spawns, supervises and reaps it. That is the gt-h1tq fix — the
// daemon's orphan sweep used to SIGTERM `claude -p` children of a running
// `om review`, because the scan had no parent column to look at.
//
// A process whose parent is itself a candidate is also not a candidate: the
// parent is signaled this cycle, and the child reparents to launchd and is
// collected on the next one.
//
// The expensive per-PID probes (cwd → town root, IDE detection) stay with the
// callers — they spawn processes and must not run for rows already ruled out.
func unownedCandidates(entries []processEntry, protected map[int]bool) []unownedCandidate {
	live := make(map[int]string, len(entries))
	for _, e := range entries {
		live[e.PID] = e.Comm
	}

	var candidates []unownedCandidate
	for _, e := range entries {
		// Only look for claude/codex processes without a TTY.
		// Linux shows "?" for no TTY, macOS shows "??"
		if e.TTY != "?" && e.TTY != "??" {
			continue
		}
		if !isAgentOrphanCommName(strings.ToLower(e.Comm)) {
			continue
		}
		// Processes in valid Gas Town tmux sessions (and ACP sessions) are
		// never candidates, even if they show TTY "?" during startup.
		if protected[e.PID] {
			continue
		}
		// Skip processes younger than minOrphanAge seconds
		// This prevents killing newly spawned subagents and reduces false positives
		age, err := parseEtime(e.Etime)
		if err != nil {
			continue
		}
		if age < minOrphanAge {
			continue
		}
		if e.hasLiveParent(live) {
			continue
		}
		candidates = append(candidates, unownedCandidate{processEntry: e, Age: age})
	}
	return candidates
}

// OrphanedProcess represents a claude process running without a controlling terminal.
type OrphanedProcess struct {
	PID      int
	PPID     int // Parent pid at scan time; 0 or 1 means reparented to launchd/init
	Cmd      string
	Age      int    // Age in seconds
	TownRoot string // Gas Town workspace root, or "" if not in any workspace
}

// FindOrphanedClaudeProcesses finds Gas Town agent processes (claude/codex/opencode/cursor-agent/copilot, etc.)
// without a controlling terminal that nobody owns any more.
// These are typically subagent processes spawned by Claude Code's Task tool that didn't
// clean up properly after completion.
//
// Detection is based on TTY column: processes with TTY "?" have no controlling terminal.
// This is safer than process tree walking because:
// - Legitimate terminal sessions always have a TTY (pts/*)
// - Orphaned subagents have no TTY (?)
// - Won't accidentally kill user's personal claude instances in terminals
//
// Ownership is checked too: the pid is only orphaned when its parent is gone
// (reparented to launchd/init, or absent from the same process table). A
// TTY-less child of a live parent — a headless `claude -p` under a running
// `om review`, say — belongs to that parent and is never signaled (gt-h1tq).
//
// Additionally, processes must be older than minOrphanAge seconds to be considered
// orphaned. This prevents race conditions with newly spawned processes.
func FindOrphanedClaudeProcesses() ([]OrphanedProcess, error) {
	// Get PIDs belonging to valid Gas Town tmux sessions.
	// These should not be killed even if they show TTY "?" during startup.
	protectedPIDs := getTmuxSessionPIDs()

	// Also protect ACP sessions (opencode agents running outside tmux)
	// ACP sessions have their own lifecycle management and should not be killed
	acpPIDs := getACPSessionPIDs()
	for pid := range acpPIDs {
		protectedPIDs[pid] = true
	}

	entries, err := psSnapshot()
	if err != nil {
		return nil, err
	}

	var orphans []OrphanedProcess
	for _, c := range unownedCandidates(entries, protectedPIDs) {
		// Skip IDE extension processes (VS Code, Cursor, etc.).
		// These have TTY "?" but are legitimate — controlled by the IDE.
		if isIDEClaudeProcess(c.PID) {
			continue
		}

		// Skip processes NOT in a Gas Town workspace.
		// Only kill orphaned Claude processes whose cwd is under a Gas Town
		// workspace root. This prevents killing user's Claude Code instances
		// running in repos outside ~/gt/ (or wherever the workspace is).
		townRoot := resolveTownRoot(c.PID)
		if townRoot == "" {
			continue
		}

		orphans = append(orphans, OrphanedProcess{
			PID:      c.PID,
			PPID:     c.PPID,
			Cmd:      c.Comm,
			Age:      c.Age,
			TownRoot: townRoot,
		})
	}

	return orphans, nil
}

// CleanupResult describes what happened to an orphaned process.
type CleanupResult struct {
	Process OrphanedProcess
	Signal  string // "SIGTERM", "SIGKILL", or "UNKILLABLE"
	Error   error
}

// ZombieProcess represents a claude process not in any active tmux session.
type ZombieProcess struct {
	PID      int
	PPID     int // Parent pid at scan time; 0 or 1 means reparented to launchd/init
	Cmd      string
	Age      int    // Age in seconds
	TTY      string // TTY column from ps (may be "?" or a session like "s024")
	TownRoot string // Gas Town workspace root, or "" if not in any workspace
}

// FindZombieClaudeProcesses finds Claude processes with no TTY that are NOT in
// any active tmux session. This catches "zombie" processes whose tmux session
// has died. Processes with a real TTY (e.g. pts/*) are skipped because those
// are interactive terminal sessions, not zombies.
//
// Like FindOrphanedClaudeProcesses, this requires that nobody owns the
// process: a TTY-less child of a live parent belongs to that parent, not to
// us (gt-h1tq — deacon patrol runs this scan automatically, so without the
// ownership test it signaled the same headless `om review` backends).
func FindZombieClaudeProcesses() ([]ZombieProcess, error) {
	// Get ALL valid PIDs (panes + their children) from active tmux sessions
	validPIDs := getTmuxSessionPIDs()

	// Also protect ACP sessions (opencode agents running outside tmux)
	// ACP sessions have their own lifecycle management and should not be killed
	acpPIDs := getACPSessionPIDs()
	for pid := range acpPIDs {
		validPIDs[pid] = true
	}

	// SAFETY CHECK: If no valid PIDs found, tmux might be down or no sessions exist.
	// Returning empty is safer than marking all Claude processes as zombies.
	if len(validPIDs) == 0 {
		// Check if tmux is even running
		if err := tmux.BuildCommand("list-sessions").Run(); err != nil {
			return nil, fmt.Errorf("tmux not available: %w", err)
		}
		// tmux is running but no gt-*/hq-* sessions - that's a valid state,
		// but we can't safely determine zombies without reference sessions.
		// Return empty rather than marking everything as zombie.
		return nil, nil
	}

	entries, err := psSnapshot()
	if err != nil {
		return nil, err
	}

	var zombies []ZombieProcess
	for _, c := range unownedCandidates(entries, validPIDs) {
		// Skip IDE extension processes (VS Code, Cursor, etc.).
		// These have TTY "?" but are legitimate — controlled by the IDE.
		if isIDEClaudeProcess(c.PID) {
			continue
		}

		// Skip processes NOT in a Gas Town workspace.
		// Only kill zombie Claude processes whose cwd is under a Gas Town
		// workspace root. This prevents killing user's Claude Code instances
		// running in repos outside ~/gt/.
		townRoot := resolveTownRoot(c.PID)
		if townRoot == "" {
			continue
		}

		// Nobody owns it and it's not in any active tmux session - it's a zombie
		zombies = append(zombies, ZombieProcess{
			PID:      c.PID,
			PPID:     c.PPID,
			Cmd:      c.Comm,
			Age:      c.Age,
			TTY:      c.TTY,
			TownRoot: townRoot,
		})
	}

	return zombies, nil
}

// zombieStateFile is the filename for zombie process tracking state.
const zombieStateFile = "gastown-zombie-state"

// loadZombieState reads the zombie state file.
func loadZombieState() map[int]signalState {
	return loadSignalState(zombieStateFile)
}

// saveZombieState writes the zombie state file.
func saveZombieState(state map[int]signalState) error {
	return saveSignalState(zombieStateFile, state)
}

// ZombieCleanupResult describes what happened to a zombie process.
type ZombieCleanupResult struct {
	Process ZombieProcess
	Signal  string // "SIGTERM", "SIGKILL", or "UNKILLABLE"
	Error   error
}

// CleanupZombieClaudeProcesses finds and kills zombie Claude processes.
// Uses tmux verification to ensure we never kill processes in active sessions.
//
// Uses the same graceful escalation as orphan cleanup:
//  1. First encounter → SIGTERM, record in state file
//  2. Next cycle, still alive after grace period → SIGKILL
//  3. Next cycle, still alive after SIGKILL → log as unkillable
func CleanupZombieClaudeProcesses() ([]ZombieCleanupResult, error) {
	zombies, err := FindZombieClaudeProcesses()
	if err != nil {
		return nil, err
	}

	state := loadZombieState()
	now := time.Now()

	var results []ZombieCleanupResult
	var lastErr error

	activeZombies := make(map[int]bool)
	zombieByPID := make(map[int]ZombieProcess, len(zombies))
	for _, z := range zombies {
		activeZombies[z.PID] = true
		zombieByPID[z.PID] = z
	}

	// Check state for PIDs that died or need escalation
	for pid, s := range state {
		if !activeZombies[pid] {
			delete(state, pid)
			continue
		}

		elapsed := now.Sub(s.Timestamp).Seconds()

		if s.Signal == "SIGKILL" {
			results = append(results, ZombieCleanupResult{
				Process: zombieRecord(pid, zombieByPID),
				Signal:  "UNKILLABLE",
				Error:   fmt.Errorf("process %d survived SIGKILL", pid),
			})
			delete(state, pid)
			delete(activeZombies, pid)
			continue
		}

		if s.Signal == "SIGTERM" && elapsed >= float64(sigkillGracePeriod) {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				if err != syscall.ESRCH {
					lastErr = fmt.Errorf("SIGKILL PID %d: %w", pid, err)
				}
				delete(state, pid)
				delete(activeZombies, pid)
				continue
			}
			state[pid] = signalState{Signal: "SIGKILL", Timestamp: now}
			results = append(results, ZombieCleanupResult{
				Process: zombieRecord(pid, zombieByPID),
				Signal:  "SIGKILL",
			})
			delete(activeZombies, pid)
		}
	}

	// Send SIGTERM to new zombies
	for _, zombie := range zombies {
		if !activeZombies[zombie.PID] {
			continue
		}
		if _, exists := state[zombie.PID]; exists {
			continue
		}

		// TOCTOU guard: re-verify this process is still a zombie before signaling.
		// Between FindZombieClaudeProcesses() and now, the process may have
		// joined a tmux session or been adopted by an active session.
		if !isProcessStillOrphaned(zombie.PID) {
			continue
		}

		if err := syscall.Kill(zombie.PID, syscall.SIGTERM); err != nil {
			if err != syscall.ESRCH {
				lastErr = fmt.Errorf("SIGTERM PID %d: %w", zombie.PID, err)
			}
			continue
		}
		state[zombie.PID] = signalState{Signal: "SIGTERM", Timestamp: now}
		results = append(results, ZombieCleanupResult{
			Process: zombie,
			Signal:  "SIGTERM",
		})
	}

	if err := saveZombieState(state); err != nil {
		if lastErr == nil {
			lastErr = fmt.Errorf("saving zombie state: %w", err)
		}
	}

	return results, lastErr
}

// zombieRecord returns the scan's record for pid. Escalation results are built
// from the state file's pids, so without this they would log a bare "claude"
// placeholder with no parent — exactly the attribution gt-h1tq asks for.
// A pid the current scan no longer lists (it died between cycles) falls back
// to the placeholder.
func zombieRecord(pid int, byPID map[int]ZombieProcess) ZombieProcess {
	if z, ok := byPID[pid]; ok {
		return z
	}
	return ZombieProcess{PID: pid, Cmd: "claude"}
}

// orphanRecord is zombieRecord for the orphan scan: escalation results built
// from state-file pids keep the comm and ppid their SIGTERM was sent with,
// falling back to a placeholder for a pid the current scan no longer lists.
func orphanRecord(pid int, byPID map[int]OrphanedProcess) OrphanedProcess {
	if o, ok := byPID[pid]; ok {
		return o
	}
	return OrphanedProcess{PID: pid, Cmd: "claude"}
}

// CleanupOrphanedClaudeProcesses finds and kills orphaned claude/codex processes.
//
// Uses a state machine to escalate signals:
//  1. First encounter → SIGTERM, record in state file
//  2. Next cycle, still alive after grace period → SIGKILL, update state
//  3. Next cycle, still alive after SIGKILL → log as unkillable, remove from state
//
// Returns the list of cleanup results and any error encountered.
func CleanupOrphanedClaudeProcesses() ([]CleanupResult, error) {
	orphans, err := FindOrphanedClaudeProcesses()
	if err != nil {
		return nil, err
	}

	// Load previous state
	state := loadOrphanState()
	now := time.Now()

	var results []CleanupResult
	var lastErr error

	// Track which PIDs we're still working on
	activeOrphans := make(map[int]bool)
	orphanByPID := make(map[int]OrphanedProcess, len(orphans))
	for _, o := range orphans {
		activeOrphans[o.PID] = true
		orphanByPID[o.PID] = o
	}

	// First pass: check state for PIDs that died (cleanup) or need escalation
	for pid, s := range state {
		if !activeOrphans[pid] {
			// Process died, remove from state
			delete(state, pid)
			continue
		}

		// Process still alive - check if we need to escalate
		elapsed := now.Sub(s.Timestamp).Seconds()

		if s.Signal == "SIGKILL" {
			// Already sent SIGKILL and it's still alive - unkillable
			results = append(results, CleanupResult{
				Process: orphanRecord(pid, orphanByPID),
				Signal:  "UNKILLABLE",
				Error:   fmt.Errorf("process %d survived SIGKILL", pid),
			})
			delete(state, pid) // Remove from tracking, nothing more we can do
			delete(activeOrphans, pid)
			continue
		}

		if s.Signal == "SIGTERM" && elapsed >= float64(sigkillGracePeriod) {
			// Sent SIGTERM but still alive after grace period - escalate to SIGKILL
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				if err != syscall.ESRCH {
					lastErr = fmt.Errorf("SIGKILL PID %d: %w", pid, err)
				}
				delete(state, pid)
				delete(activeOrphans, pid)
				continue
			}
			state[pid] = signalState{Signal: "SIGKILL", Timestamp: now}
			results = append(results, CleanupResult{
				Process: orphanRecord(pid, orphanByPID),
				Signal:  "SIGKILL",
			})
			delete(activeOrphans, pid)
		}
		// If SIGTERM was recent, leave it alone - check again next cycle
	}

	// Second pass: send SIGTERM to new orphans not yet in state
	for _, orphan := range orphans {
		if !activeOrphans[orphan.PID] {
			continue // Already handled above
		}
		if _, exists := state[orphan.PID]; exists {
			continue // Already in state, waiting for grace period
		}

		// TOCTOU guard: re-verify this process is still orphaned before signaling.
		// Between FindOrphanedClaudeProcesses() and now, the process may have
		// joined a tmux session or acquired a TTY.
		if !isProcessStillOrphaned(orphan.PID) {
			continue
		}

		// New orphan - send SIGTERM
		if err := syscall.Kill(orphan.PID, syscall.SIGTERM); err != nil {
			if err != syscall.ESRCH {
				lastErr = fmt.Errorf("SIGTERM PID %d: %w", orphan.PID, err)
			}
			continue
		}
		state[orphan.PID] = signalState{Signal: "SIGTERM", Timestamp: now}
		results = append(results, CleanupResult{
			Process: orphan,
			Signal:  "SIGTERM",
		})
	}

	// Save updated state
	if err := saveOrphanState(state); err != nil {
		if lastErr == nil {
			lastErr = fmt.Errorf("saving orphan state: %w", err)
		}
	}

	return results, lastErr
}

// isProcessStillOrphaned re-checks whether a process is still orphaned/zombie.
// Used for TOCTOU re-verification immediately before sending signals.
// Returns true if the process still has no controlling terminal and is not
// in any active tmux session (i.e., still safe to signal).
func isProcessStillOrphaned(pid int) bool {
	// Re-check the process TTY via ps
	out, err := exec.Command("ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // Process may have exited - not orphaned anymore
	}

	tty := strings.TrimSpace(string(out))
	if tty == "" {
		return false // Process gone
	}

	// If it now has a real TTY, it's been adopted
	if tty != "?" && tty != "??" {
		return false
	}

	// Re-check against current tmux session PIDs
	protectedPIDs := getTmuxSessionPIDs()
	return !protectedPIDs[pid]
}
