package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	defaultMainBranchTestInterval = 30 * time.Minute
	defaultMainBranchTestTimeout  = 10 * time.Minute

	// minCPUIdlePercentForMainBranchTest is the minimum estimated CPU idle
	// percentage required before running main_branch_test gates. Below this,
	// the host is too busy to trust a red result — the cycle is skipped and
	// reported as "skipped: host busy" rather than a false FAILED, mirroring
	// the load-based pressure gate refineries use before spawning agents
	// (see checkPressure in pressure.go).
	minCPUIdlePercentForMainBranchTest = 25.0

	// maxEscalationOutputLines caps the filtered test output included in an
	// escalation body. The full output is always persisted to disk first.
	maxEscalationOutputLines = 200
)

// MainBranchTestConfig holds configuration for the main_branch_test patrol.
// This patrol periodically runs quality gates on each rig's main branch to
// catch regressions from direct-to-main pushes, bad merges, or sequential
// merge conflicts that individually pass but break together.
type MainBranchTestConfig struct {
	// Enabled controls whether the main-branch test runner runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "30m").
	IntervalStr string `json:"interval,omitempty"`

	// TimeoutStr is the maximum time each rig's test run can take.
	// Default: "10m".
	TimeoutStr string `json:"timeout,omitempty"`

	// Rigs limits testing to specific rigs. If empty, all rigs are tested.
	Rigs []string `json:"rigs,omitempty"`
}

// mainBranchTestInterval returns the configured interval, or the default (30m).
func mainBranchTestInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.MainBranchTest != nil {
		if config.Patrols.MainBranchTest.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.MainBranchTest.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultMainBranchTestInterval
}

// mainBranchTestTimeout returns the configured per-rig timeout, or the default (10m).
func mainBranchTestTimeout(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.MainBranchTest != nil {
		if config.Patrols.MainBranchTest.TimeoutStr != "" {
			if d, err := time.ParseDuration(config.Patrols.MainBranchTest.TimeoutStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultMainBranchTestTimeout
}

// mainBranchTestRigs returns the configured rig filter, or nil (all rigs).
func mainBranchTestRigs(config *DaemonPatrolConfig) []string {
	if config != nil && config.Patrols != nil && config.Patrols.MainBranchTest != nil {
		return config.Patrols.MainBranchTest.Rigs
	}
	return nil
}

// rigGateConfig holds the gate/test configuration extracted from a rig's config.json.
type rigGateConfig struct {
	TestCommand string
	Gates       map[string]string // gate name → command
}

// loadRigGateConfig reads the merge_queue section from a rig's config.json
// to discover what test/gate commands to run.
func loadRigGateConfig(rigPath string) (*rigGateConfig, error) {
	configPath := filepath.Join(rigPath, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No config, skip
		}
		return nil, err
	}

	var raw struct {
		MergeQueue json.RawMessage `json:"merge_queue"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config.json: %w", err)
	}

	if raw.MergeQueue == nil {
		return nil, nil // No merge_queue section
	}

	var mq struct {
		TestCommand *string                    `json:"test_command"`
		Gates       map[string]json.RawMessage `json:"gates"`
	}
	if err := json.Unmarshal(raw.MergeQueue, &mq); err != nil {
		return nil, fmt.Errorf("parsing merge_queue: %w", err)
	}

	cfg := &rigGateConfig{}

	// Extract gates (preferred over legacy test_command)
	if len(mq.Gates) > 0 {
		cfg.Gates = make(map[string]string, len(mq.Gates))
		for name, rawGate := range mq.Gates {
			var gate struct {
				Cmd string `json:"cmd"`
			}
			if err := json.Unmarshal(rawGate, &gate); err == nil && gate.Cmd != "" {
				cfg.Gates[name] = gate.Cmd
			}
		}
	}

	// Fall back to legacy test_command
	if mq.TestCommand != nil && *mq.TestCommand != "" {
		cfg.TestCommand = *mq.TestCommand
	}

	if len(cfg.Gates) == 0 && cfg.TestCommand == "" {
		return nil, nil // No runnable commands
	}

	return cfg, nil
}

// runMainBranchTests runs quality gates on each rig's main branch.
// It fetches the latest main, runs configured gates/tests, and escalates failures.
func (d *Daemon) runMainBranchTests() {
	if !d.isPatrolActive("main_branch_test") {
		return
	}

	d.logger.Printf("main_branch_test: starting patrol cycle")

	if idle := cpuIdlePercent(); idle < minCPUIdlePercentForMainBranchTest {
		d.logger.Printf("main_branch_test: skipped: host busy (CPU idle %.1f%% < %.0f%%)", idle, minCPUIdlePercentForMainBranchTest)
		return
	}

	rigNames := d.getKnownRigs()
	if len(rigNames) == 0 {
		d.logger.Printf("main_branch_test: no rigs found")
		return
	}

	allowedRigs := mainBranchTestRigs(d.patrolConfig)
	timeout := mainBranchTestTimeout(d.patrolConfig)

	var tested, failed int
	var failures []string

	for _, rigName := range rigNames {
		if len(allowedRigs) > 0 && !sliceContains(allowedRigs, rigName) {
			continue
		}

		rigPath := filepath.Join(d.config.TownRoot, rigName)
		if err := d.testRigMainBranch(rigName, rigPath, timeout); err != nil {
			d.logger.Printf("main_branch_test: %s: FAILED: %v", rigName, err)
			failures = append(failures, fmt.Sprintf("%s: %v", rigName, err))
			failed++
		} else {
			d.logger.Printf("main_branch_test: %s: passed", rigName)
		}
		tested++
	}

	if len(failures) > 0 {
		msg := fmt.Sprintf("main branch test failures:\n%s", strings.Join(failures, "\n"))
		d.logger.Printf("main_branch_test: escalating %d failure(s)", len(failures))
		d.escalate("main_branch_test", msg)
	}

	d.logger.Printf("main_branch_test: patrol cycle complete (%d tested, %d failed)", tested, failed)
}

// testRigMainBranch tests a single rig's main branch.
func (d *Daemon) testRigMainBranch(rigName, rigPath string, timeout time.Duration) error {
	// Load gate config from the rig's config.json
	gateCfg, err := loadRigGateConfig(rigPath)
	if err != nil {
		return fmt.Errorf("loading gate config: %w", err)
	}
	if gateCfg == nil {
		d.logger.Printf("main_branch_test: %s: no test commands configured, skipping", rigName)
		return nil
	}

	// Determine default branch
	defaultBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(rigPath); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}

	// Create a temporary worktree for testing to avoid interfering with
	// the refinery's working directory.
	worktreePath := filepath.Join(rigPath, ".main-test-worktree")
	bareRepoPath := filepath.Join(rigPath, ".repo.git")

	// Verify bare repo exists
	if _, err := os.Stat(bareRepoPath); os.IsNotExist(err) {
		return fmt.Errorf("bare repo not found at %s", bareRepoPath)
	}

	// Clean up stale worktree if it exists
	if _, err := os.Stat(worktreePath); err == nil {
		cleanupCmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
		cleanupCmd.Dir = bareRepoPath
		util.SetDetachedProcessGroup(cleanupCmd)
		_ = cleanupCmd.Run()
	}

	ctx, cancel := context.WithTimeout(d.ctx, timeout)
	defer cancel()

	// Fetch latest main
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "origin", defaultBranch)
	fetchCmd.Dir = bareRepoPath
	util.SetDetachedProcessGroup(fetchCmd)
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}

	// Create temporary worktree at origin/<default_branch>
	addCmd := exec.CommandContext(ctx, "git", "worktree", "add", "--detach", worktreePath, "origin/"+defaultBranch)
	addCmd.Dir = bareRepoPath
	util.SetDetachedProcessGroup(addCmd)
	if output, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}

	// Always clean up the worktree
	defer func() {
		removeCmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
		removeCmd.Dir = bareRepoPath
		util.SetDetachedProcessGroup(removeCmd)
		if err := removeCmd.Run(); err != nil {
			d.logger.Printf("main_branch_test: %s: warning: worktree cleanup failed: %v", rigName, err)
		}
	}()

	// Determine the tested HEAD sha so the verdict can be matched to a head
	// and the evidence log can be named for it.
	sha, err := gitRevParseHead(ctx, worktreePath)
	if err != nil {
		d.logger.Printf("main_branch_test: %s: warning: could not determine tested HEAD sha: %v", rigName, err)
		sha = "unknown"
	}

	// Run gates or legacy test command
	if len(gateCfg.Gates) > 0 {
		return d.runGatesOnWorktree(ctx, rigName, worktreePath, sha, gateCfg.Gates)
	}
	return d.runCommandOnWorktree(ctx, rigName, worktreePath, "test", gateCfg.TestCommand, sha)
}

// gitRevParseHead returns the current HEAD sha of the git worktree at workDir.
func gitRevParseHead(ctx context.Context, workDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = workDir
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// runGatesOnWorktree runs all configured gates sequentially on the given worktree.
func (d *Daemon) runGatesOnWorktree(ctx context.Context, rigName, workDir, sha string, gates map[string]string) error {
	var failures []string
	for name, cmd := range gates {
		if err := d.runCommandOnWorktree(ctx, rigName, workDir, name, cmd, sha); err != nil {
			failures = append(failures, fmt.Sprintf("gate %q: %v", name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

// runCommandOnWorktree runs a single shell command in the given worktree directory.
// sha is the tested HEAD sha, recorded in the log line and in the persisted
// evidence log's filename so a verdict can be matched to a head.
func (d *Daemon) runCommandOnWorktree(ctx context.Context, rigName, workDir, label, command, sha string) error {
	d.logger.Printf("main_branch_test: %s: running %s on %s: %s", rigName, label, sha, command)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // G204: command is from trusted rig config
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "CI=true") // Signal test environment
	util.SetDetachedProcessGroup(cmd)

	idleStart := cpuIdlePercent()
	output, err := cmd.CombinedOutput()
	idleEnd := cpuIdlePercent()
	if err != nil {
		// Persist the FULL output before any truncation/filtering — this is
		// the evidence a later investigator needs to tell a real regression
		// from contention, since the temporary worktree is removed after the
		// run.
		logPath, logErr := persistMainBranchTestLog(d.config.TownRoot, rigName, sha, string(output))
		if logErr != nil {
			d.logger.Printf("main_branch_test: %s: warning: failed to persist evidence log: %v", rigName, logErr)
			logPath = fmt.Sprintf("(not persisted: %v)", logErr)
		}
		body := filterTestFailureOutput(string(output))
		return fmt.Errorf(
			"%s failed: %v\nsha: %s\nlog: %s\nCPU idle (start->end): %.1f%% -> %.1f%%\n%s",
			label, err, sha, logPath, idleStart, idleEnd, body,
		)
	}
	return nil
}

// persistMainBranchTestLog writes the full combined command output to
// <townRoot>/logs/main_branch_test/<rig>-<sha>.log, returning the log path.
func persistMainBranchTestLog(townRoot, rigName, sha, output string) (string, error) {
	logDir := filepath.Join(townRoot, "logs", "main_branch_test")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return "", fmt.Errorf("creating log dir: %w", err)
	}
	shaLabel := sha
	if shaLabel == "" {
		shaLabel = "unknown"
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s.log", rigName, shaLabel))
	if err := os.WriteFile(logPath, []byte(output), 0644); err != nil {
		return "", fmt.Errorf("writing log file: %w", err)
	}
	return logPath, nil
}

// filterTestFailureOutput extracts the diagnostically relevant lines from a
// test run's combined output: every "--- FAIL" line together with its
// following assertion line, plus "FAIL" summary lines. This replaces a naive
// tail, which after a long suite is mostly "ok" lines and drops the actual
// failing test names off the end. Capped at maxEscalationOutputLines after
// filtering.
func filterTestFailureOutput(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")

	var filtered []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "--- FAIL"):
			filtered = append(filtered, line)
			if i+1 < len(lines) {
				filtered = append(filtered, lines[i+1])
			}
		case strings.HasPrefix(trimmed, "FAIL"):
			filtered = append(filtered, line)
		}
	}

	if len(filtered) == 0 {
		// No recognizable test-failure markers (e.g. a build or lint
		// failure) — fall back to the tail so the escalation still carries
		// something rather than nothing.
		filtered = lines
	}

	if len(filtered) > maxEscalationOutputLines {
		filtered = filtered[len(filtered)-maxEscalationOutputLines:]
	}
	return strings.Join(filtered, "\n")
}

// cpuIdlePercent estimates the current CPU idle percentage from the
// 1-minute load average and core count. Returns 100 (fully idle) when load
// data is unavailable, so an inability to read load average never blocks a
// run.
func cpuIdlePercent() float64 {
	return computeCPUIdlePercent(loadAverage1(), runtime.NumCPU())
}

func computeCPUIdlePercent(load1 float64, numCPU int) float64 {
	if numCPU <= 0 {
		return 100
	}
	idle := 100 * (1 - load1/float64(numCPU))
	if idle < 0 {
		return 0
	}
	if idle > 100 {
		return 100
	}
	return idle
}

// contains checks if a string slice contains a value.
func sliceContains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}
