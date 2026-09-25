//go:build !windows

package util

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tmux"
)

func TestParseEtime(t *testing.T) {
	tests := []struct {
		input    string
		expected int
		wantErr  bool
	}{
		// MM:SS format
		{"00:30", 30, false},
		{"01:00", 60, false},
		{"01:23", 83, false},
		{"59:59", 3599, false},

		// HH:MM:SS format
		{"00:01:00", 60, false},
		{"01:00:00", 3600, false},
		{"01:02:03", 3723, false},
		{"23:59:59", 86399, false},

		// DD-HH:MM:SS format
		{"1-00:00:00", 86400, false},
		{"2-01:02:03", 176523, false},
		{"7-12:30:45", 649845, false},

		// Edge cases
		{"00:00", 0, false},
		{"0-00:00:00", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseEtime(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseEtime(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.expected {
				t.Errorf("parseEtime(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

func TestParseProcessTable(t *testing.T) {
	out := `  PID  PPID TTY      COMM     ELAPSED
    1     0 ??       launchd  10-00:00:00
  420    1  ??       claude   01:02:03
  500   420 ttys001  claude   00:05:00

  not-a-pid     1 ?? claude 00:01:00
  600     1 ??       claude
`
	entries := parseProcessTable(out)
	if len(entries) != 3 {
		t.Fatalf("parseProcessTable() returned %d entries, want 3: %+v", len(entries), entries)
	}
	want := []processEntry{
		{PID: 1, PPID: 0, TTY: "??", Comm: "launchd", Etime: "10-00:00:00"},
		{PID: 420, PPID: 1, TTY: "??", Comm: "claude", Etime: "01:02:03"},
		{PID: 500, PPID: 420, TTY: "ttys001", Comm: "claude", Etime: "00:05:00"},
	}
	for i, w := range want {
		if entries[i] != w {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], w)
		}
	}
}

// psTable builds the raw text a ps -eo pid,ppid,tty,comm,etime call would
// print, so the ownership rules can be exercised without real processes.
func psTable(rows ...string) string {
	return "  PID  PPID TTY      COMM     ELAPSED\n" + strings.Join(rows, "\n") + "\n"
}

// candidatePIDs reduces candidate rows to the pid set the assertions care about.
func candidatePIDs(candidates []unownedCandidate) map[int]int {
	got := make(map[int]int, len(candidates))
	for _, c := range candidates {
		got[c.PID] = c.PPID
	}
	return got
}

// TestUnownedCandidates_Ownership is the gt-h1tq regression: the orphan scan
// used to signal any TTY-less claude with a cwd under the town, including a
// headless `claude -p` child of a live `om review`. Only processes nobody owns
// may be signaled.
func TestUnownedCandidates_Ownership(t *testing.T) {
	entries := parseProcessTable(psTable(
		// (a) reparented to launchd — a genuine orphan
		"  100     1 ??       claude   01:00:00",
		// (b) live parent (pid 150, listed below) — the `om review` backend case
		"  200   150 ??       claude   01:00:00",
		// the live parent: itself a claude, owned by the shell at 900
		"  150   900 ??       claude   02:00:00",
		"  900     1 ttys002  zsh      03:00:00",
		// (c) parent pid absent from the table — parent already exited
		"  300   250 ??       claude   01:00:00",
		// (d) ppid 0 (kernel) is not an owner either
		"  400     0 ??       claude   01:00:00",
	))

	got := candidatePIDs(unownedCandidates(entries, nil))

	for _, pid := range []int{100, 300, 400} {
		if _, ok := got[pid]; !ok {
			t.Errorf("PID %d: not a candidate, want candidate (nobody owns it)", pid)
		}
	}
	for _, pid := range []int{200, 150} {
		if _, ok := got[pid]; ok {
			t.Errorf("PID %d: candidate, want not a candidate (live parent in the table)", pid)
		}
	}
}

// TestUnownedCandidates_StrandedTmuxSurvivor is the gt-tbtx regression: a
// pane process forks directly under the tmux server, so once its session is
// killed a survivor that ignored the resulting SIGHUP keeps the
// still-running server as its ppid. The server hosts unrelated sessions and
// says nothing about whether THIS process is still owned, so it must not
// count as a live parent the way the gt-h1tq `om review` case does.
func TestUnownedCandidates_StrandedTmuxSurvivor(t *testing.T) {
	entries := parseProcessTable(psTable(
		// tmux server: alive, hosting other (unrelated) sessions
		"   50     1 ??       tmux     05:00:00",
		// stranded survivor of a session the server already killed
		"  210    50 ??       claude   01:00:00",
	))

	got := candidatePIDs(unownedCandidates(entries, nil))

	if _, ok := got[210]; !ok {
		t.Error("PID 210 (survivor parented to the tmux server): want candidate")
	}
}

// TestUnownedCandidates_LiveParentChain documents the cascade: a claude whose
// parent is itself a candidate is not signaled while the parent is; the
// parent is signaled this cycle and the child reparents to launchd, so the
// next cycle collects it.
func TestUnownedCandidates_LiveParentChain(t *testing.T) {
	entries := parseProcessTable(psTable(
		"  100     1 ??       claude   01:00:00", // orphaned parent
		"  200   100 ??       claude   00:30:00", // its child, parent still alive
	))

	got := candidatePIDs(unownedCandidates(entries, nil))

	if _, ok := got[100]; !ok {
		t.Error("PID 100 (reparented to launchd): want candidate")
	}
	if _, ok := got[200]; ok {
		t.Error("PID 200 (child of a live parent): want not a candidate this cycle")
	}
}

// TestUnownedCandidates_ExistingFilters pins the filters that were already
// there before the ownership test, so adding ppid did not loosen any of them.
func TestUnownedCandidates_ExistingFilters(t *testing.T) {
	entries := parseProcessTable(psTable(
		"  101     1 ??       claude   01:00:00", // baseline: a candidate
		"  102     1 ttys004  claude   01:00:00", // has a TTY — interactive
		"  103     1 ??       node     01:00:00", // not a tracked agent comm
		"  104     1 ??       claude   00:00:30", // younger than minOrphanAge
		"  105     1 ??       claude   01:00:00", // protected by a tmux/ACP session
	))

	got := candidatePIDs(unownedCandidates(entries, map[int]bool{105: true}))

	if _, ok := got[101]; !ok {
		t.Error("PID 101: want candidate")
	}
	for pid, why := range map[int]string{
		102: "has a controlling TTY",
		103: "comm is not a tracked agent runtime",
		104: "younger than the 60s floor",
		105: "protected tmux/ACP pid",
	} {
		if _, ok := got[pid]; ok {
			t.Errorf("PID %d: candidate, want not a candidate (%s)", pid, why)
		}
	}
}

// TestUnownedCandidates_AgeFloorMatchesParseEtime keeps the candidate age
// (returned to callers for display) in agreement with the ps etime parser.
func TestUnownedCandidates_AgeFloorMatchesParseEtime(t *testing.T) {
	entries := parseProcessTable(psTable("  101     1 ??       claude   01:00:00"))
	candidates := unownedCandidates(entries, nil)
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0].Age != 3600 {
		t.Errorf("Age = %d, want 3600", candidates[0].Age)
	}
}

func TestFindOrphanedClaudeProcesses(t *testing.T) {
	// Live test that checks for orphaned processes on the current system.
	// Should not fail — just returns whatever orphans exist (likely none in CI).
	// Also verifies that processes with a TTY are excluded (only TTY=? reported).
	orphans, err := FindOrphanedClaudeProcesses()
	if err != nil {
		t.Fatalf("FindOrphanedClaudeProcesses() error = %v", err)
	}

	t.Logf("Found %d orphaned claude processes", len(orphans))
	for _, o := range orphans {
		t.Logf("  PID %d: %s (verify TTY=? in 'ps aux')", o.PID, o.Cmd)
	}
}

func TestGetProcessCwd(t *testing.T) {
	// Our own process should have a valid cwd
	cwd := getProcessCwd(os.Getpid())
	if cwd == "" {
		t.Fatal("getProcessCwd(self) returned empty string")
	}
	// Verify it matches os.Getwd
	expected, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error: %v", err)
	}
	if cwd != expected {
		t.Errorf("getProcessCwd(self) = %q, want %q", cwd, expected)
	}
}

func TestIsInGasTownWorkspace(t *testing.T) {
	// NOTE: This test uses os.Chdir on the process-global cwd.
	// Do NOT add t.Parallel() here or to any test in this file—concurrent
	// tests sharing the same process would race on the working directory.

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	// Create a temporary directory structure simulating a Gas Town workspace
	tmpDir := t.TempDir()
	mayorDir := filepath.Join(tmpDir, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	townJSON := filepath.Join(mayorDir, "town.json")
	if err := os.WriteFile(townJSON, []byte(`{"name":"test-town"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Move to a non-workspace temp dir first, so the "not in workspace" check
	// works even when tests are run from inside a real GT workspace.
	nonWorkspaceDir := t.TempDir()
	if err := os.Chdir(nonWorkspaceDir); err != nil {
		t.Fatal(err)
	}

	// Our process is NOT in the temp workspace, so should return false
	if isInGasTownWorkspace(os.Getpid()) {
		t.Error("isInGasTownWorkspace(self) = true, want false (not in a GT workspace)")
	}

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	if !isInGasTownWorkspace(os.Getpid()) {
		t.Error("isInGasTownWorkspace(self) = false, want true (in GT workspace root)")
	}

	// Test from a subdirectory of the workspace
	subDir := filepath.Join(tmpDir, "polecats", "test-polecat")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}
	if !isInGasTownWorkspace(os.Getpid()) {
		t.Error("isInGasTownWorkspace(self) = false, want true (in GT workspace subdir)")
	}
}

// hasTmux returns true if tmux is available on PATH.
func hasTmux() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// tmuxSocketSession creates a session on the given socket and returns the pane PID.
// The caller must kill the session or server in cleanup.
func tmuxSocketSession(t *testing.T, socketName, sessionName string) int {
	t.Helper()
	err := exec.Command("tmux", "-L", socketName, "new-session", "-d",
		"-s", sessionName, "-x", "80", "-y", "24", "sleep", "300").Run()
	if err != nil {
		t.Fatalf("create session %q on socket %q: %v", sessionName, socketName, err)
	}

	out, err := exec.Command("tmux", "-L", socketName,
		"list-panes", "-t", sessionName, "-F", "#{pane_pid}").Output()
	if err != nil {
		t.Fatalf("list-panes for %q on %q: %v", sessionName, socketName, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse pane PID %q: %v", string(out), err)
	}
	return pid
}

// killTmuxServer kills a tmux server by socket name. It goes through
// tmux.KillServer rather than a bare `tmux kill-server` because that also
// unlinks the socket file, which tmux leaves behind when its server exits
// (gt-20di).
func killTmuxServer(socketName string) {
	_ = tmux.NewTmuxWithSocket(socketName).KillServer()
}

func TestGetTmuxSessionPIDs_CrossSocket(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	socketA := fmt.Sprintf("gt-test-orphan-a-%d", os.Getpid())
	socketB := fmt.Sprintf("gt-test-orphan-b-%d", os.Getpid())
	t.Cleanup(func() {
		killTmuxServer(socketA)
		killTmuxServer(socketB)
	})

	pidA := tmuxSocketSession(t, socketA, "session-a")
	pidB := tmuxSocketSession(t, socketB, "session-b")

	// Set default socket to A — simulates being inside Town A's context
	oldSocket := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket(socketA)
	t.Cleanup(func() { tmux.SetDefaultSocket(oldSocket) })

	pids := getTmuxSessionPIDs()

	if !pids[pidA] {
		t.Errorf("PID %d from socket A not in protected set (set size=%d)", pidA, len(pids))
	}
	if !pids[pidB] {
		t.Errorf("PID %d from socket B not in protected set — cross-town process would be killed (set size=%d)", pidB, len(pids))
	}
}

func TestGetTmuxSessionPIDs_SingleSocket(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gt-test-orphan-single-%d", os.Getpid())
	t.Cleanup(func() { killTmuxServer(socket) })

	pid1 := tmuxSocketSession(t, socket, "session-1")
	pid2 := tmuxSocketSession(t, socket, "session-2")

	oldSocket := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket(socket)
	t.Cleanup(func() { tmux.SetDefaultSocket(oldSocket) })

	pids := getTmuxSessionPIDs()

	if !pids[pid1] {
		t.Errorf("PID %d from session-1 not in protected set", pid1)
	}
	if !pids[pid2] {
		t.Errorf("PID %d from session-2 not in protected set", pid2)
	}
}

// realPath resolves symlinks in a path. On macOS, /var -> /private/var,
// and lsof returns the real path while t.TempDir() returns the symlinked one.
func realPath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return resolved
}

func TestResolveTownRoot(t *testing.T) {
	// NOTE: Uses os.Chdir — no t.Parallel().
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	townA := realPath(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(townA, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townA, "mayor", "town.json"),
		[]byte(`{"name":"town-a"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	townB := realPath(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(townB, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townB, "mayor", "town.json"),
		[]byte(`{"name":"town-b"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// From Town A root
	if err := os.Chdir(townA); err != nil {
		t.Fatal(err)
	}
	if got := resolveTownRoot(os.Getpid()); got != townA {
		t.Errorf("resolveTownRoot from town A root = %q, want %q", got, townA)
	}

	// From a subdirectory of Town A
	subDir := filepath.Join(townA, "polecats", "test-polecat")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}
	if got := resolveTownRoot(os.Getpid()); got != townA {
		t.Errorf("resolveTownRoot from town A subdir = %q, want %q", got, townA)
	}

	// From Town B root
	if err := os.Chdir(townB); err != nil {
		t.Fatal(err)
	}
	if got := resolveTownRoot(os.Getpid()); got != townB {
		t.Errorf("resolveTownRoot from town B root = %q, want %q", got, townB)
	}

	// From outside any town
	nonTown := realPath(t, t.TempDir())
	if err := os.Chdir(nonTown); err != nil {
		t.Fatal(err)
	}
	if got := resolveTownRoot(os.Getpid()); got != "" {
		t.Errorf("resolveTownRoot from outside = %q, want empty string", got)
	}
}

func TestResolveTownRoot_DistinguishesAdjacentTowns(t *testing.T) {
	// Two sibling towns under the same parent (mirrors ~/gastown and ~/gt-financing)
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	parent := realPath(t, t.TempDir())

	townA := filepath.Join(parent, "gastown")
	townB := filepath.Join(parent, "gt-financing")
	for _, town := range []string{townA, townB} {
		if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"),
			[]byte(`{"name":"test"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Chdir(townA); err != nil {
		t.Fatal(err)
	}
	if got := resolveTownRoot(os.Getpid()); got != townA {
		t.Errorf("from townA: resolveTownRoot = %q, want %q", got, townA)
	}

	if err := os.Chdir(townB); err != nil {
		t.Fatal(err)
	}
	if got := resolveTownRoot(os.Getpid()); got != townB {
		t.Errorf("from townB: resolveTownRoot = %q, want %q", got, townB)
	}

	// Parent directory itself is NOT a town
	if err := os.Chdir(parent); err != nil {
		t.Fatal(err)
	}
	if got := resolveTownRoot(os.Getpid()); got != "" {
		t.Errorf("from parent: resolveTownRoot = %q, want empty (parent is not a town)", got)
	}
}

func TestOrphanedProcess_TownRoot_Populated(t *testing.T) {
	// Smoke test: verify TownRoot is populated (or "") on every orphan.
	// Does not fail if no orphans exist — mirrors TestFindOrphanedClaudeProcesses.
	orphans, err := FindOrphanedClaudeProcesses()
	if err != nil {
		t.Fatalf("FindOrphanedClaudeProcesses() error = %v", err)
	}

	t.Logf("Found %d orphaned claude processes", len(orphans))
	for _, o := range orphans {
		t.Logf("  PID %d: %s town=%q", o.PID, o.Cmd, o.TownRoot)
		if o.TownRoot != "" {
			if _, err := os.Stat(o.TownRoot); err != nil {
				t.Errorf("PID %d: TownRoot %q does not exist on disk", o.PID, o.TownRoot)
			}
		}
	}
}
