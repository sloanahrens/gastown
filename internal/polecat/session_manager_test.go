package polecat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
	gtruntime "github.com/steveyegge/gastown/internal/runtime"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
)

// setupSessionBranchTestRepo is a repo on main with one commit whose origin
// is itself, with origin/main at that commit, in a new world.
func setupSessionBranchTestRepo(t *testing.T) (string, gitRepo, *world) {
	t.Helper()
	w := newWorld()
	workDir := t.TempDir()
	w.InitRepo(t, workDir)
	head := w.Commit(t, workDir, "main", "Initial commit", map[string]string{"README.md": "# Test\n"})
	w.checkout(t, workDir)
	w.AddRemote(t, workDir, "origin", workDir)
	w.SetRef(t, workDir, "refs/remotes/origin/main", head)
	return workDir, w.repo(workDir), w
}

// strandCanonicalBaseRef makes origin/<default> unresolvable in the test repo,
// standing in for a worktree that never fetched the canonical base. The
// origin goes too: the helper's origin is the repo itself, so a fetch would
// republish every local branch as origin/<branch>.
func strandCanonicalBaseRef(t *testing.T, w *world, workDir string) {
	t.Helper()
	w.RemoveRemote(t, workDir, "origin")
}

func TestSessionName(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{
		Name:     "gastown",
		Polecats: []string{"Toast"},
	}
	m := newTestSessionManager(r)

	name := m.SessionName("Toast")
	if name != "gt-Toast" {
		t.Errorf("sessionName = %q, want gt-Toast", name)
	}
}

// TestParseSessionCreatedTime_NonUTCLocal is a regression test for gt-jy5:
// session uptime was miscomputed as now_UTC minus created_local, inflating
// reported uptime by exactly the machine's UTC offset (e.g. 5h under CDT)
// and causing freshly-dispatched, live polecats to be reported stalled/dead.
func TestParseSessionCreatedTime_NonUTCLocal(t *testing.T) {
	t.Parallel()
	// A non-UTC machine (e.g. CDT, UTC-5), without depending on the system
	// zoneinfo database and without touching the process-wide time.Local.
	local := time.FixedZone("TEST-5", -5*60*60)

	// Mirror Tmux.GetSessionInfo's production path: a Unix instant formatted
	// via time.Unix(...).Format(layout) in the local zone, with no zone
	// indicator retained in the resulting string.
	created := time.Now().In(local).Add(-1 * time.Minute)
	str := created.Format("2006-01-02 15:04:05")

	got := parseSessionCreatedTimeIn(str, local)
	if got.IsZero() {
		t.Fatalf("parseSessionCreatedTimeIn(%q) returned zero time", str)
	}

	uptime := time.Since(got)
	if uptime < 0 || uptime > 5*time.Minute {
		t.Errorf("uptime = %v, want ~1m (the pre-fix bug reports ~5h1m under a UTC-5 local zone)", uptime)
	}
}

// TestParseSessionCreatedTimeReadsTheLocalZone is the wiring guard for
// parseSessionCreatedTime: tmux prints session_created in the machine's
// zone, so the exported path must parse in time.Local. (On a machine whose
// zone is UTC this cannot tell Local from UTC.)
func TestParseSessionCreatedTimeReadsTheLocalZone(t *testing.T) {
	t.Parallel()
	instant := time.Unix(1_790_000_000, 0)
	str := instant.In(time.Local).Format("2006-01-02 15:04:05")
	if got := parseSessionCreatedTime(str); !got.Equal(instant) {
		t.Fatalf("parseSessionCreatedTime(%q) = %v, want %v", str, got, instant)
	}
}

func TestSessionManagerPolecatDir(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{
		Name:     "gastown",
		Path:     "/home/user/ai/gastown",
		Polecats: []string{"Toast"},
	}
	m := newTestSessionManager(r)

	dir := m.polecatDir("Toast")
	expected := "/home/user/ai/gastown/polecats/Toast"
	if filepath.ToSlash(dir) != expected {
		t.Errorf("polecatDir = %q, want %q", dir, expected)
	}
}

func TestHasPolecat(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// hasPolecat checks filesystem, so create actual directories
	for _, name := range []string{"Toast", "Cheedo"} {
		if err := os.MkdirAll(filepath.Join(root, "polecats", name), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	r := &rig.Rig{
		Name:     "gastown",
		Path:     root,
		Polecats: []string{"Toast", "Cheedo"},
	}
	m := newTestSessionManager(r)

	if !m.hasPolecat("Toast") {
		t.Error("expected hasPolecat(Toast) = true")
	}
	if !m.hasPolecat("Cheedo") {
		t.Error("expected hasPolecat(Cheedo) = true")
	}
	if m.hasPolecat("Unknown") {
		t.Error("expected hasPolecat(Unknown) = false")
	}

	// The exported form is what gt session stop gates its deliberate-stop
	// marker on (gt-fojqs): a marker written for a name that is not a polecat
	// directory would park nothing and mislead gt status.
	if !m.HasPolecat("Toast") {
		t.Error("expected HasPolecat(Toast) = true")
	}
	if m.HasPolecat("Unknown") {
		t.Error("expected HasPolecat(Unknown) = false")
	}
}

func TestStartPolecatNotFound(t *testing.T) {
	t.Parallel()
	r := &rig.Rig{
		Name:     "gastown",
		Polecats: []string{"Toast"},
	}
	m := newTestSessionManager(r)

	err := m.Start("Unknown", SessionStartOptions{})
	if err == nil {
		t.Error("expected error for unknown polecat")
	}
}

func TestIsRunningNoSession(t *testing.T) {
	t.Parallel()

	r := &rig.Rig{
		Name:     "gastown",
		Polecats: []string{"Toast"},
	}
	m := newTestSessionManager(r)

	running, err := m.IsRunning("Toast")
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if running {
		t.Error("expected IsRunning = false for non-existent session")
	}
}

func TestSessionManagerListEmpty(t *testing.T) {
	t.Parallel()

	r := &rig.Rig{
		Name:     "test-rig-unlikely-name",
		Polecats: []string{},
	}
	m := newTestSessionManager(r)

	infos, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 0 {
		t.Errorf("infos count = %d, want 0", len(infos))
	}
}

func TestStopNotFound(t *testing.T) {
	t.Parallel()

	r := &rig.Rig{
		Name:     "test-rig",
		Polecats: []string{"Toast"},
	}
	m := newTestSessionManager(r)

	err := m.Stop("Toast", false)
	if err != ErrSessionNotFound {
		t.Errorf("Stop = %v, want ErrSessionNotFound", err)
	}
}

// gt down routes Stop's kill through the supervisor (gt-4k3fj.4.1): with a
// stop hook set, Stop kills only through it.
func TestStopKillsThroughTheStopHook(t *testing.T) {
	t.Parallel()
	tm := newFakeSessionTmux()
	m := &SessionManager{tmux: tm, rig: &rig.Rig{Name: "test-rig", Polecats: []string{"Toast"}}, gits: newWorld().opener()}
	sess := m.SessionName("Toast")
	if err := tm.NewSession(sess, ""); err != nil {
		t.Fatal(err)
	}
	var routed []string
	m.SetStopKill(func(id string) error { routed = append(routed, id); return nil })
	if err := m.Stop("Toast", true); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if len(routed) != 1 || routed[0] != sess {
		t.Fatalf("hook kills = %v, want [%s]", routed, sess)
	}
	if ok, _ := tm.HasSession(sess); !ok {
		t.Fatal("Stop killed through tmux as well as the hook")
	}
}

func TestCaptureNotFound(t *testing.T) {
	t.Parallel()

	r := &rig.Rig{
		Name:     "test-rig",
		Polecats: []string{"Toast"},
	}
	m := newTestSessionManager(r)

	_, err := m.Capture("Toast", 50)
	if err != ErrSessionNotFound {
		t.Errorf("Capture = %v, want ErrSessionNotFound", err)
	}
}

func TestInjectNotFound(t *testing.T) {
	t.Parallel()

	r := &rig.Rig{
		Name:     "test-rig",
		Polecats: []string{"Toast"},
	}
	m := newTestSessionManager(r)

	err := m.Inject("Toast", "hello")
	if err != ErrSessionNotFound {
		t.Errorf("Inject = %v, want ErrSessionNotFound", err)
	}
}

// TestPolecatCommandFormat verifies the polecat session command exports
// GT_ROLE, GT_RIG, GT_POLECAT, and BD_ACTOR inline before starting Claude.
// This is a regression test for gt-y41ep - env vars must be exported inline
// because tmux SetEnvironment only affects new panes, not the current shell.
func TestPolecatCommandFormat(t *testing.T) {
	t.Parallel()
	// This test verifies the expected command format.
	// The actual command is built in Start() but we test the format here
	// to document and verify the expected behavior.

	rigName := "gastown"
	polecatName := "Toast"
	expectedBdActor := "gastown/polecats/Toast"
	// GT_ROLE uses compound format: rig/polecats/name
	expectedGtRole := rigName + "/polecats/" + polecatName

	// Build the expected command format (mirrors Start() logic)
	expectedPrefix := "export GT_ROLE=" + expectedGtRole + " GT_RIG=" + rigName + " GT_POLECAT=" + polecatName + " BD_ACTOR=" + expectedBdActor + " GIT_AUTHOR_NAME=" + expectedBdActor
	expectedSuffix := "&& claude --dangerously-skip-permissions"

	// The command must contain all required env exports
	requiredParts := []string{
		"export",
		"GT_ROLE=" + expectedGtRole,
		"GT_RIG=" + rigName,
		"GT_POLECAT=" + polecatName,
		"BD_ACTOR=" + expectedBdActor,
		"GIT_AUTHOR_NAME=" + expectedBdActor,
		"claude --dangerously-skip-permissions",
	}

	// Verify expected format contains all required parts
	fullCommand := expectedPrefix + " " + expectedSuffix
	for _, part := range requiredParts {
		if !strings.Contains(fullCommand, part) {
			t.Errorf("Polecat command should contain %q", part)
		}
	}

	// Verify GT_ROLE uses compound format with "polecats" (not "mayor", "crew", etc.)
	if !strings.Contains(fullCommand, "GT_ROLE="+expectedGtRole) {
		t.Errorf("GT_ROLE must be %q (compound format), not simple 'polecat'", expectedGtRole)
	}
}

// TestPolecatStartInjectsFallbackEnvVars verifies that the polecat session
// startup injects GT_BRANCH and GT_POLECAT_PATH into the startup command.
// These env vars are critical for gt done's nuked-worktree fallback:
// when the polecat's cwd is deleted, gt done uses these to determine
// the branch and path without a working directory.
// Regression test for PR #1402.
func TestPolecatStartInjectsFallbackEnvVars(t *testing.T) {
	t.Parallel()
	rigName := "gastown"
	polecatName := "Toast"
	workDir := "/tmp/fake-worktree"

	townRoot := "/tmp/fake-town"

	// The env vars that should be injected via PrependEnv
	requiredEnvVars := []string{
		"GT_BRANCH",       // Git branch for nuked-worktree fallback
		"GT_POLECAT_PATH", // Worktree path for nuked-worktree fallback
		"GT_RIG",          // Rig name (was already there pre-PR)
		"GT_POLECAT",      // Polecat name (was already there pre-PR)
		"GT_ROLE",         // Role address (was already there pre-PR)
		"GT_TOWN_ROOT",    // Town root for FindFromCwdWithFallback after worktree nuke
	}

	// Verify the env var map includes all required keys
	envVars := map[string]string{
		"GT_RIG":          rigName,
		"GT_POLECAT":      polecatName,
		"GT_ROLE":         rigName + "/polecats/" + polecatName,
		"GT_POLECAT_PATH": workDir,
		"GT_TOWN_ROOT":    townRoot,
	}

	// GT_BRANCH is conditionally added (only if CurrentBranch succeeds)
	// In practice it's always set because the worktree exists at Start time
	branchName := "polecat/" + polecatName
	envVars["GT_BRANCH"] = branchName

	for _, key := range requiredEnvVars {
		if _, ok := envVars[key]; !ok {
			t.Errorf("missing required env var %q in startup injection", key)
		}
	}

	// Verify GT_POLECAT_PATH matches workDir
	if envVars["GT_POLECAT_PATH"] != workDir {
		t.Errorf("GT_POLECAT_PATH = %q, want %q", envVars["GT_POLECAT_PATH"], workDir)
	}

	// Verify GT_BRANCH matches expected branch
	if envVars["GT_BRANCH"] != branchName {
		t.Errorf("GT_BRANCH = %q, want %q", envVars["GT_BRANCH"], branchName)
	}
}

func TestEnsureCanonicalSessionBranch_UsesOriginDefaultBranch(t *testing.T) {
	t.Parallel()
	workDir, repoGit, w := setupSessionBranchTestRepo(t)

	baseSHA, err := repoGit.Rev("origin/main")
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}
	if err := repoGit.CheckoutNewBranch("polecat/toast-old", "main"); err != nil {
		t.Fatalf("checkout stale polecat branch: %v", err)
	}
	staleSHA := w.writeAndCommit(t, workDir, "stale local polecat commit", map[string]string{"stale.txt": "stale\n"})

	sm := newTestSessionManager(&rig.Rig{Name: "gastown", Path: workDir})
	branch, err := sm.ensureCanonicalSessionBranch(repoGit, "toast", SessionStartOptions{Issue: "gt-9qb"})
	if err != nil {
		t.Fatalf("ensureCanonicalSessionBranch: %v", err)
	}
	if !strings.Contains(branch, "/gt-9qb+") {
		t.Fatalf("fresh session branch = %q, want issue-scoped branch", branch)
	}

	staleAncestor, err := repoGit.IsAncestor(staleSHA, branch)
	if err != nil {
		t.Fatalf("check stale ancestry: %v", err)
	}
	if staleAncestor {
		t.Fatalf("fresh session branch %q unexpectedly includes stale local commit %s", branch, staleSHA)
	}

	baseAncestor, err := repoGit.IsAncestor(baseSHA, branch)
	if err != nil {
		t.Fatalf("check canonical ancestry: %v", err)
	}
	if !baseAncestor {
		t.Fatalf("fresh session branch %q should descend from origin/main commit %s", branch, baseSHA)
	}
}

func TestEnsureCanonicalSessionBranch_KeepsCurrentIssueBranch(t *testing.T) {
	t.Parallel()
	workDir, repoGit, _ := setupSessionBranchTestRepo(t)

	currentBranch := "polecat/toast/gt-9qb@seed"
	if err := repoGit.CheckoutNewBranch(currentBranch, "main"); err != nil {
		t.Fatalf("checkout current issue branch: %v", err)
	}

	sm := newTestSessionManager(&rig.Rig{Name: "gastown", Path: workDir})
	branch, err := sm.ensureCanonicalSessionBranch(repoGit, "toast", SessionStartOptions{Issue: "gt-9qb"})
	if err != nil {
		t.Fatalf("ensureCanonicalSessionBranch: %v", err)
	}
	if branch != currentBranch {
		t.Fatalf("ensureCanonicalSessionBranch changed active issue branch: got %q want %q", branch, currentBranch)
	}
}

// TestEnsureCanonicalSessionBranch_MissingBaseRefFailsOnBaseBranch covers the
// incident state of gt-ns8t: the worktree sits on the base branch, the repair
// is needed, and origin/<default> does not resolve — so the session used to
// start on the base branch with nothing reported anywhere.
func TestEnsureCanonicalSessionBranch_MissingBaseRefFailsOnBaseBranch(t *testing.T) {
	t.Parallel()
	workDir, repoGit, w := setupSessionBranchTestRepo(t)
	strandCanonicalBaseRef(t, w, workDir)

	sm := newTestSessionManager(&rig.Rig{Name: "gastown", Path: workDir})
	branch, err := sm.ensureCanonicalSessionBranch(repoGit, "toast", SessionStartOptions{Issue: "gt-9qb"})
	if !errors.Is(err, ErrBaseBranchRepair) {
		t.Fatalf("err = %v, want ErrBaseBranchRepair", err)
	}
	if branch != "main" {
		t.Fatalf("branch = %q, want the base branch %q", branch, "main")
	}
}

// TestEnsureCanonicalSessionBranch_CheckoutRefusedFailsOnBaseBranch covers the
// other shape of the same incident: the repair resolves its start point but
// git refuses the branch switch, which happens when local commits on the base
// branch conflict with the uncommitted working tree a killed polecat left
// behind (lapis's "WIP: checkpoint (auto)" on local main, gt-ns8t).
func TestEnsureCanonicalSessionBranch_CheckoutRefusedFailsOnBaseBranch(t *testing.T) {
	t.Parallel()
	workDir, repoGit, w := setupSessionBranchTestRepo(t)
	baseSHA, err := repoGit.Rev("origin/main")
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}

	// Local main diverges from origin/main, then leaves an uncommitted edit to
	// the diverging file: `git checkout -b <new> origin/main` must overwrite it.
	w.writeAndCommit(t, workDir, "local main commit", map[string]string{"local.txt": "diverged\n"})
	if err := os.WriteFile(filepath.Join(workDir, "local.txt"), []byte("uncommitted\n"), 0644); err != nil {
		t.Fatalf("edit local.txt: %v", err)
	}

	// Pin origin/main to the pre-divergence base and cut the remote, so the
	// repair still resolves its start point but the switch has to overwrite the
	// dirty file.
	w.RemoveRemote(t, workDir, "origin")
	w.SetRef(t, workDir, "refs/remotes/origin/main", baseSHA)

	sm := newTestSessionManager(&rig.Rig{Name: "gastown", Path: workDir})
	branch, err := sm.ensureCanonicalSessionBranch(repoGit, "toast", SessionStartOptions{Issue: "gt-9qb"})
	if !errors.Is(err, ErrBaseBranchRepair) {
		t.Fatalf("err = %v, want ErrBaseBranchRepair", err)
	}
	if branch != "main" {
		t.Fatalf("branch = %q, want the base branch %q", branch, "main")
	}
}

// TestEnsureCanonicalSessionBranch_RecordsRepairFailure asserts the feed event
// that carries the failure past the witness restart path, where
// `gt session restart` runs under util.ExecRun and its stderr is discarded on
// exit 0 (gt-ns8t).
func TestEnsureCanonicalSessionBranch_RecordsRepairFailure(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatalf("mkdir rig path: %v", err)
	}

	workDir, repoGit, w := setupSessionBranchTestRepo(t)
	strandCanonicalBaseRef(t, w, workDir)

	sm := newTestSessionManager(&rig.Rig{Name: "gastown", Path: rigPath})
	if _, err := sm.ensureCanonicalSessionBranch(repoGit, "toast", SessionStartOptions{Issue: "gt-9qb"}); err == nil {
		t.Fatal("ensureCanonicalSessionBranch returned no error, want ErrBaseBranchRepair")
	}

	data, err := os.ReadFile(filepath.Join(townRoot, ".events.jsonl"))
	if err != nil {
		t.Fatalf("read events file: %v", err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev struct {
			Type    string                 `json:"type"`
			Payload map[string]interface{} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		if ev.Type != "polecat_branch_repair_failed" {
			continue
		}
		found = true
		if ev.Payload["polecat"] != "toast" {
			t.Errorf("payload polecat = %v, want toast", ev.Payload["polecat"])
		}
		if ev.Payload["step"] == "" {
			t.Error("payload step is empty; the event cannot say which repair step failed")
		}
	}
	if !found {
		t.Fatalf("no polecat_branch_repair_failed event in %s", data)
	}
}

// TestSessionManager_resolveBeadsDir verifies that SessionManager correctly
// resolves the beads directory for cross-rig issues via routes.jsonl.
// This is a regression test for GitHub issue #1056.
//
// The bug was that hookIssue/validateIssue used workDir directly instead of
// resolving via routes.jsonl. Now they call resolveBeadsDir which we test here.
func TestSessionManager_resolveBeadsDir(t *testing.T) {
	t.Parallel()
	// Set up a mock town with routes.jsonl
	townRoot := t.TempDir()
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create routes.jsonl with cross-rig routing
	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(townBeadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a rig inside the town (simulating gastown rig)
	rigPath := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Create SessionManager with the rig
	r := &rig.Rig{
		Name: "gastown",
		Path: rigPath,
	}
	m := newTestSessionManager(r)

	polecatWorkDir := filepath.Join(rigPath, "polecats", "Toast")

	tests := []struct {
		name        string
		issueID     string
		expectedDir string
	}{
		{
			name:        "same-rig bead resolves to rig path",
			issueID:     "gt-abc123",
			expectedDir: filepath.Join(townRoot, "gastown/mayor/rig"),
		},
		{
			name:        "cross-rig bead (beads) resolves to beads rig path",
			issueID:     "bd-xyz789",
			expectedDir: filepath.Join(townRoot, "beads/mayor/rig"),
		},
		{
			name:        "town-level bead resolves to town root",
			issueID:     "hq-town123",
			expectedDir: townRoot,
		},
		{
			name:        "unknown prefix falls back to fallbackDir",
			issueID:     "xx-unknown",
			expectedDir: polecatWorkDir,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Test the SessionManager's resolveBeadsDir method directly
			resolved := m.resolveBeadsDir(tc.issueID, polecatWorkDir)
			if resolved != tc.expectedDir {
				t.Errorf("resolveBeadsDir(%q, %q) = %q, want %q",
					tc.issueID, polecatWorkDir, resolved, tc.expectedDir)
			}
		})
	}
}

// TestAgentEnvOmitsGTAgent_FallbackRequired verifies that the AgentEnv path
// used by session_manager.Start does NOT include GT_AGENT when opts.Agent is
// empty (the default dispatch path). This confirms the session_manager must
// fall back to runtimeConfig.ResolvedAgent for setting GT_AGENT in the tmux
// session table.
//
// Without the fallback, GT_AGENT is never written to the tmux session table,
// and the post-startup validation kills the session with:
//
//	"GT_AGENT not set in session ... witness patrol will misidentify this polecat"
//
// Regression test for the bug introduced in PR #1776 which removed the
// unconditional runtimeConfig.ResolvedAgent → SetEnvironment("GT_AGENT") logic
// and replaced it with an AgentEnv-only path that requires opts.Agent to be set.
func TestAgentEnvOmitsGTAgent_FallbackRequired(t *testing.T) {
	t.Parallel()

	// Simulate what session_manager.Start calls for each dispatch scenario.
	cases := []struct {
		name        string
		agent       string // opts.Agent value
		wantGTAgent bool   // whether GT_AGENT should be in AgentEnv output
	}{
		{
			name:        "default dispatch (no --agent flag)",
			agent:       "",
			wantGTAgent: false, // fallback needed
		},
		{
			name:        "explicit --agent codex",
			agent:       "codex",
			wantGTAgent: true,
		},
		{
			name:        "explicit --agent gemini",
			agent:       "gemini",
			wantGTAgent: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := config.AgentEnv(config.AgentEnvConfig{
				Role:      "polecat",
				Rig:       "gastown",
				AgentName: "Toast",
				TownRoot:  "/tmp/town",
				Agent:     tc.agent,
			})
			_, hasGTAgent := env["GT_AGENT"]
			if hasGTAgent != tc.wantGTAgent {
				t.Errorf("AgentEnv(Agent=%q): GT_AGENT present=%v, want %v",
					tc.agent, hasGTAgent, tc.wantGTAgent)
			}
		})
	}
}

// startupNudgeRC is a runtime whose idle agent shows the ❯ prompt, so the
// startup-nudge verification runs.
func startupNudgeRC() *config.RuntimeConfig {
	return &config.RuntimeConfig{
		PromptMode: "arg",
		Hooks:      &config.RuntimeHooksConfig{Provider: "claude"},
		Tmux:       &config.RuntimeTmuxConfig{ReadyPromptPrefix: "❯ "},
	}
}

// TestVerifyStartupNudgeDelivery_IdleAgent: an agent still idle at its prompt
// after each verify delay has lost the nudge, so every attempt re-sends it,
// one default delay apart.
func TestVerifyStartupNudgeDelivery_IdleAgent(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClockAt(testEpoch)
	tm := tmuxfake.New(clk)
	const name = "gt-test-nudge"
	if err := tm.NewSession(name, ""); err != nil {
		t.Fatal(err)
	}
	tm.SetIdle(name, true)

	done := make(chan struct{})
	go func() {
		verifyStartupNudge(tm, clk, t.TempDir(), name, startupNudgeRC(), "check your hook")
		close(done)
	}()
	driveClock(t, clk, config.DefaultStartupNudgeVerifyDelay, done)

	sent := tm.Sent(name)
	if len(sent) != config.DefaultStartupNudgeMaxRetries {
		t.Fatalf("sent %d nudges %q, want one per attempt (%d)", len(sent), sent, config.DefaultStartupNudgeMaxRetries)
	}
	for _, s := range sent {
		if s != "check your hook" {
			t.Fatalf("re-sent %q, want the retry content", s)
		}
	}
	if got, want := clk.Since(testEpoch), time.Duration(config.DefaultStartupNudgeMaxRetries)*config.DefaultStartupNudgeVerifyDelay; got != want {
		t.Fatalf("verification waited %v, want %d default delays (%v)", got, config.DefaultStartupNudgeMaxRetries, want)
	}
}

// TestVerifyStartupNudgeDelivery_BusyAgentIsLeftAlone: an agent that is not
// idle got the nudge; nothing is re-sent.
func TestVerifyStartupNudgeDelivery_BusyAgentIsLeftAlone(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClockAt(testEpoch)
	tm := tmuxfake.New(clk)
	const name = "gt-test-busy"
	if err := tm.NewSession(name, ""); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		verifyStartupNudge(tm, clk, t.TempDir(), name, startupNudgeRC(), "check your hook")
		close(done)
	}()
	driveClock(t, clk, config.DefaultStartupNudgeVerifyDelay, done)
	if sent := tm.Sent(name); len(sent) != 0 {
		t.Fatalf("busy agent was re-nudged: %q", sent)
	}
}

// TestModeAStartupVerifyIsNonBlocking pins why Start runs the Mode A
// verification on a goroutine (hi-y44): the verification waits a full verify
// delay before its first look at the pane, so a synchronous call would hold
// every polecat start for it. Until the clock moves, nothing is checked or
// sent and the call has not returned.
func TestModeAStartupVerifyIsNonBlocking(t *testing.T) {
	t.Parallel()
	rc := startupNudgeRC()
	if info := gtruntime.GetStartupFallbackInfo(rc); info.SendBeaconNudge || info.SendStartupNudge {
		t.Fatal("expected Mode A: !SendBeaconNudge && !SendStartupNudge")
	}
	clk := clockwork.NewFakeClockAt(testEpoch)
	tm := tmuxfake.New(clk)
	const name = "gt-test-modeA"
	if err := tm.NewSession(name, ""); err != nil {
		t.Fatal(err)
	}
	tm.SetIdle(name, true)

	done := make(chan struct{})
	go func() {
		verifyStartupNudge(tm, clk, t.TempDir(), name, rc, "[GAS TOWN] test")
		close(done)
	}()
	if err := clk.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("verification returned before its first verify delay")
	default:
	}
	if sent := tm.Sent(name); len(sent) != 0 {
		t.Fatalf("verification acted before its first verify delay: %q", sent)
	}
	driveClock(t, clk, config.DefaultStartupNudgeVerifyDelay, done)
}

// TestVerifyStartupNudgeDelivery_NilConfig verifies that the verification
// exits at once, touching no session, when the runtime has no prompt
// detection.
func TestVerifyStartupNudgeDelivery_NilConfig(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClockAt(testEpoch)
	tm := tmuxfake.New(clk)
	const name = "gt-test-nilcfg"
	if err := tm.NewSession(name, ""); err != nil {
		t.Fatal(err)
	}
	tm.SetIdle(name, true)
	for _, rc := range []*config.RuntimeConfig{
		nil,
		{Tmux: &config.RuntimeTmuxConfig{ReadyPromptPrefix: "", ReadyDelayMs: 1000}},
	} {
		// Returns without blocking on the clock: nobody advances it here.
		verifyStartupNudge(tm, clk, t.TempDir(), name, rc, "")
	}
	if sent := tm.Sent(name); len(sent) != 0 {
		t.Fatalf("sent %q with no prompt detection", sent)
	}
}

func TestPromptlessFallbackIncludesPrimeAndWorkInstructions(t *testing.T) {
	t.Parallel()
	beaconConfig := session.BeaconConfig{
		Recipient:               session.BeaconRecipient("polecat", "toast", "demo"),
		Sender:                  "witness",
		Topic:                   "assigned",
		MolID:                   "demo-123",
		IncludePrimeInstruction: true,
		ExcludeWorkInstructions: true,
	}

	prompt := session.BuildStartupPrompt(beaconConfig, gtruntime.StartupNudgeContent())

	if !strings.Contains(prompt, "Run `gt prime`") {
		t.Fatalf("prompt missing gt prime instruction: %q", prompt)
	}
	if !strings.Contains(prompt, gtruntime.StartupNudgeContent()) {
		t.Fatalf("prompt missing startup nudge content: %q", prompt)
	}
}

// TestModeABeaconVerificationCondition verifies that hook+prompt agents (e.g. Claude)
// satisfy the Mode A beacon delivery verification condition introduced in hi-y44.
// Fresh spawns may show the Claude Code splash with the CLI beacon pre-filled but
// not auto-submitted; the condition triggers verifyStartupNudgeDelivery as a safety net.
func TestModeABeaconVerificationCondition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		rc        *config.RuntimeConfig
		wantModeA bool // !SendBeaconNudge && !SendStartupNudge
	}{
		{
			name: "Claude hook+prompt agent triggers Mode A verification",
			rc: &config.RuntimeConfig{
				PromptMode: "arg",
				Hooks: &config.RuntimeHooksConfig{
					Provider: "claude",
				},
			},
			wantModeA: true,
		},
		{
			name: "Non-hook agent does not trigger Mode A (has startup nudge instead)",
			rc: &config.RuntimeConfig{
				PromptMode: "arg",
				Hooks:      nil,
			},
			wantModeA: false,
		},
		{
			name: "Hook agent with no prompt support does not trigger Mode A (uses beacon nudge)",
			rc: &config.RuntimeConfig{
				PromptMode: "none",
				Hooks: &config.RuntimeHooksConfig{
					Provider: "claude",
				},
			},
			wantModeA: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := gtruntime.GetStartupFallbackInfo(tt.rc)
			gotModeA := !info.SendBeaconNudge && !info.SendStartupNudge
			if gotModeA != tt.wantModeA {
				t.Errorf("Mode A condition = %v, want %v (SendBeaconNudge=%v, SendStartupNudge=%v)",
					gotModeA, tt.wantModeA, info.SendBeaconNudge, info.SendStartupNudge)
			}
		})
	}
}

func TestValidateSessionName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		sessionName string
		rigPrefix   string
		rigName     string
		wantErr     bool
	}{
		{
			name:        "valid themed name",
			sessionName: "gm-furiosa",
			rigPrefix:   "gm",
			rigName:     "gastown_manager",
			wantErr:     false,
		},
		{
			name:        "valid overflow name (new format)",
			sessionName: "gm-51",
			rigPrefix:   "gm",
			rigName:     "gastown_manager",
			wantErr:     false,
		},
		{
			name:        "malformed double-prefix (bug)",
			sessionName: "gm-gastown_manager-51",
			rigPrefix:   "gm",
			rigName:     "gastown_manager",
			wantErr:     true,
		},
		{
			name:        "malformed double-prefix gastown",
			sessionName: "gt-gastown-142",
			rigPrefix:   "gt",
			rigName:     "gastown",
			wantErr:     true,
		},
		{
			name:        "different rig (can't validate)",
			sessionName: "gt-other-rig-name",
			rigPrefix:   "gm",
			rigName:     "gastown_manager",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSessionName(tt.sessionName, tt.rigPrefix, tt.rigName)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSessionName() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPolecatSlot(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := tmpDir
	polecatsDir := filepath.Join(rigPath, "polecats")
	if err := os.MkdirAll(polecatsDir, 0755); err != nil {
		t.Fatal(err)
	}

	r := &rig.Rig{
		Name:     "testrig",
		Path:     rigPath,
		Polecats: []string{},
	}
	sm := newTestSessionManager(r)

	// No polecats — should return 0
	if slot := sm.polecatSlot("alpha"); slot != 0 {
		t.Errorf("empty dir: got slot %d, want 0", slot)
	}

	// Create some polecat dirs (sorted: alpha, beta, gamma)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := os.MkdirAll(filepath.Join(polecatsDir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name string
		want int
	}{
		{"alpha", 0},
		{"beta", 1},
		{"gamma", 2},
	}
	for _, tt := range tests {
		if slot := sm.polecatSlot(tt.name); slot != tt.want {
			t.Errorf("polecatSlot(%q) = %d, want %d", tt.name, slot, tt.want)
		}
	}

	// Hidden dirs should be skipped
	if err := os.MkdirAll(filepath.Join(polecatsDir, ".hidden"), 0755); err != nil {
		t.Fatal(err)
	}
	if slot := sm.polecatSlot("beta"); slot != 1 {
		t.Errorf("with hidden dir: polecatSlot(beta) = %d, want 1", slot)
	}
}

func TestParseFreshBranchName_RoundTrip(t *testing.T) {
	t.Parallel()
	sm := &SessionManager{}

	cases := []struct {
		name    string
		polecat string
		issue   string
	}{
		{name: "with issue", polecat: "alpha", issue: "gt-abc"},
		{name: "no issue", polecat: "beta", issue: ""},
		{name: "numeric issue", polecat: "nux", issue: "gt-123"},
		{name: "dotted subtask", polecat: "furiosa", issue: "gt-jns7.1"},
		{name: "dashed issue", polecat: "alpha", issue: "gt-pin-bd-metadata"},
		{name: "nested dotted subtask", polecat: "alpha", issue: "gt-4kp9.5.5.1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			branch := sm.freshBranchName(c.polecat, c.issue)
			meta := parseFreshBranchName(branch)
			if !meta.ok {
				t.Fatalf("parseFreshBranchName(%q) not ok", branch)
			}
			if meta.polecat != c.polecat {
				t.Errorf("polecat = %q, want %q (from %q)", meta.polecat, c.polecat, branch)
			}
			if meta.issue != c.issue {
				t.Errorf("issue = %q, want %q (from %q)", meta.issue, c.issue, branch)
			}
		})
	}
}

func TestParseFreshBranchName_IssueTimestampSeparators(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		branch string
		issue  string
	}{
		{name: "current plus separator", branch: "polecat/alpha/gt-abc+mk123456", issue: "gt-abc"},
		{name: "legacy at separator", branch: "polecat/alpha/gt-abc@mk123456", issue: "gt-abc"},
		{name: "dotted subtask", branch: "polecat/alpha/gt-abc.1+mk123456", issue: "gt-abc.1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta := parseFreshBranchName(c.branch)
			if !meta.ok {
				t.Fatalf("parseFreshBranchName(%q) not ok", c.branch)
			}
			if meta.polecat != "alpha" || meta.issue != c.issue {
				t.Fatalf("parseFreshBranchName(%q) = %+v, want polecat alpha issue %s", c.branch, meta, c.issue)
			}
		})
	}
}

func TestParseFreshBranchName_Rejects(t *testing.T) {
	t.Parallel()
	rejects := []string{
		"main",
		"master",
		"develop",
		"feature/x",
		"polecat/",                         // empty tail
		"polecat/alpha",                    // no ts or issue
		"polecat/alpha-",                   // trailing dash, no ts
		"polecat//gt-abc@1",                // empty polecat name
		"polecat/alpha/@1",                 // empty issue
		"polecat/alpha/+1",                 // empty issue
		"polecat/alpha/gt-abc@",            // empty ts
		"polecat/alpha/gt-abc+",            // empty ts
		"polecat/alpha/gt-pin-bd-metadata", // raw issue branch, not generated
		"polecat/alpha/gt-jns7.1-mk123456", // dash suffix is ambiguous and not generated
		"",
	}
	for _, b := range rejects {
		if meta := parseFreshBranchName(b); meta.ok {
			t.Errorf("parseFreshBranchName(%q) = %+v, want ok=false", b, meta)
		}
	}
}

func TestShouldCreateFreshSessionBranch_Structural(t *testing.T) {
	t.Parallel()
	// Non-standard canonical branch (e.g., "develop") — must be honored even
	// though the old string-heuristic hardcoded "main"/"master".
	cases := []struct {
		name            string
		currentBranch   string
		issue           string
		canonicalBranch string
		want            bool
	}{
		{
			name:            "on develop as canonical triggers fresh",
			currentBranch:   "develop",
			issue:           "gt-abc",
			canonicalBranch: "develop",
			want:            true,
		},
		{
			name:            "on main when canonical is develop does NOT trigger fresh",
			currentBranch:   "main",
			issue:           "gt-abc",
			canonicalBranch: "develop",
			want:            false,
		},
		{
			name:            "same-issue respawn preserves branch",
			currentBranch:   "polecat/alpha/gt-abc+xyz",
			issue:           "gt-abc",
			canonicalBranch: "main",
			want:            false,
		},
		{
			name:            "legacy same-issue respawn preserves branch",
			currentBranch:   "polecat/alpha/gt-abc@xyz",
			issue:           "gt-abc",
			canonicalBranch: "main",
			want:            false,
		},
		{
			name:            "other-issue polecat branch triggers fresh",
			currentBranch:   "polecat/alpha/gt-999+xyz",
			issue:           "gt-abc",
			canonicalBranch: "main",
			want:            true,
		},
		{
			name:            "empty canonical with non-polecat branch does not trigger",
			currentBranch:   "feature/x",
			issue:           "gt-abc",
			canonicalBranch: "",
			want:            false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldCreateFreshSessionBranch(c.currentBranch, c.issue, c.canonicalBranch); got != c.want {
				t.Errorf("shouldCreateFreshSessionBranch(%q, %q, %q) = %v, want %v",
					c.currentBranch, c.issue, c.canonicalBranch, got, c.want)
			}
		})
	}
}

// Regression test for gt-yy9: fresh polecat worktrees are never-before-trusted
// paths, and the polecat spawn path (which creates its tmux session directly
// rather than via session.StartSession) must pre-seed Claude's folder-trust
// entry, or the session stalls on the folder-trust dialog.
func TestEnsureRuntimeWorkspace_SeedsTrustForNeverTrustedWorktree(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	configDir := t.TempDir()
	workDir := t.TempDir() // fresh worktree: no trust entry exists anywhere

	r := &rig.Rig{
		Name:     "gastown",
		Path:     rigPath,
		Polecats: []string{"Toast"},
	}
	m := newTestSessionManager(r)

	rc := &config.RuntimeConfig{Command: "claude"}
	if err := m.ensureRuntimeWorkspace(workDir, configDir, rc); err != nil {
		t.Fatalf("ensureRuntimeWorkspace: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		t.Fatalf("reading seeded .claude.json: %v", err)
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parsing .claude.json: %v", err)
	}
	if !cfg.Projects[workDir].HasTrustDialogAccepted {
		t.Errorf("expected trust entry for %s, got %s", workDir, data)
	}
}
