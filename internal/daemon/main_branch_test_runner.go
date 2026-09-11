package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	defaultMainBranchTestInterval = 30 * time.Minute
	defaultMainBranchTestTimeout  = 10 * time.Minute

	// maxDiagnosticLines bounds how many matching failure lines go into the
	// escalation body — enough for the mayor to triage without a rerun,
	// short enough to stay readable.
	maxDiagnosticLines = 40

	// mainBranchTestSlotTimeout bounds how long the main_branch_test patrol
	// waits for the container-gate slot before giving up on a rig's run,
	// mirroring acquireBatchGateSlot's batchSlotTimeout (internal/cmd/mq_batch.go).
	mainBranchTestSlotTimeout = 60 * time.Minute
)

// diagnosticLinePattern matches the lines worth surfacing from a failing
// test/build run: test failures, panics, and build errors. Plain "ok"
// lines from passing packages are excluded so a long run with one early
// failure doesn't bury it under trailing successes (gt-1s2g).
var diagnosticLinePattern = regexp.MustCompile(`^(FAIL|--- FAIL|panic:|# |.*\[build failed\])`)

// extractDiagnosticLines returns the first maxDiagnosticLines lines of output
// that match diagnosticLinePattern, in their original order.
func extractDiagnosticLines(output string) []string {
	var diagnostic []string
	for _, line := range strings.Split(output, "\n") {
		if diagnosticLinePattern.MatchString(line) {
			diagnostic = append(diagnostic, line)
			if len(diagnostic) >= maxDiagnosticLines {
				break
			}
		}
	}
	return diagnostic
}

// writeMainBranchTestLog captures the full output of a failed run to
// ~/gt/logs/main_branch_test/<rig>-<ts>.log so the mayor can inspect it
// without rerunning the test (gt-1s2g).
func writeMainBranchTestLog(townRoot, rigName, output string) (string, error) {
	dir := filepath.Join(townRoot, "logs", "main_branch_test")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating log dir: %w", err)
	}
	logPath := filepath.Join(dir, fmt.Sprintf("%s-%s.log", rigName, time.Now().UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(logPath, []byte(output), 0644); err != nil {
		return "", fmt.Errorf("writing log file: %w", err)
	}
	return logPath, nil
}

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

	commit := d.commitTested(ctx, rigName, worktreePath)

	// Acquire the container-gate slot before running gates/tests: this
	// baseline run spins the same Docker-backed suites (Dolt, testcontainers)
	// that every other caller guards with 'gt slot run', so it must be a
	// first-class slot holder instead of an invisible occupant that collides
	// with other rigs' container suites on the shared Docker VM (gt-hpce;
	// same class as gt-afe4/gt-tuiy, wrapped here in Go rather than shelling
	// out, matching acquireBatchGateSlot in internal/cmd/mq_batch.go).
	h, err := acquireMainBranchTestSlot(d.config.TownRoot, rigName)
	if err != nil {
		return fmt.Errorf("acquiring container-gate slot: %w", err)
	}
	defer h.Release()

	// Run gates or legacy test command
	if len(gateCfg.Gates) > 0 {
		return d.runGatesOnWorktree(ctx, rigName, commit, worktreePath, gateCfg.Gates)
	}
	return d.runCommandOnWorktree(ctx, rigName, commit, worktreePath, "test", gateCfg.TestCommand)
}

// acquireMainBranchTestSlot acquires the container-gate slot for a rig's
// main_branch_test run. Split out from testRigMainBranch so it's testable
// without the full git-worktree/bare-repo harness that function requires
// (gt-hpce, following acquireBatchGateSlot's precedent in
// internal/cmd/mq_batch.go).
func acquireMainBranchTestSlot(townRoot, rigName string) (*slot.Handle, error) {
	return slot.Acquire(townRoot, rigName+"/main-branch-test", mainBranchTestSlotTimeout)
}

// commitTested returns the commit SHA checked out in the worktree, or ""
// if it can't be determined — the escalation body degrades gracefully
// rather than failing the whole test run over this.
func (d *Daemon) commitTested(ctx context.Context, rigName, worktreePath string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = worktreePath
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	if err != nil {
		d.logger.Printf("main_branch_test: %s: warning: could not determine tested commit: %v", rigName, err)
		return ""
	}
	return strings.TrimSpace(string(output))
}

// runGatesOnWorktree runs all configured gates sequentially on the given worktree.
func (d *Daemon) runGatesOnWorktree(ctx context.Context, rigName, commit, workDir string, gates map[string]string) error {
	var failures []string
	for name, cmd := range gates {
		if err := d.runCommandOnWorktree(ctx, rigName, commit, workDir, name, cmd); err != nil {
			failures = append(failures, fmt.Sprintf("gate %q: %v", name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

// runCommandOnWorktree runs a single shell command in the given worktree directory.
// On failure it captures the full output to a log file and builds an error
// carrying the failing rig, the commit tested, the lines that actually
// diagnose the failure (FAIL/panic/build-error lines, not a blind tail that
// can be all-"ok" noise from later packages), and the log path so the mayor
// can triage without rerunning (gt-1s2g).
func (d *Daemon) runCommandOnWorktree(ctx context.Context, rigName, commit, workDir, label, command string) error {
	d.logger.Printf("main_branch_test: %s: running %s: %s", rigName, label, command)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // G204: command is from trusted rig config
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "CI=true") // Signal test environment
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	logPath, logErr := writeMainBranchTestLog(d.config.TownRoot, rigName, string(output))
	if logErr != nil {
		d.logger.Printf("main_branch_test: %s: warning: could not write diagnostic log: %v", rigName, logErr)
	}

	diagnostic := extractDiagnosticLines(string(output))
	if len(diagnostic) == 0 {
		// Nothing matched a known failure pattern (e.g. a shell error before
		// the test binary even ran) — fall back to a short tail so the body
		// isn't empty.
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		diagnostic = lines
	}

	var body strings.Builder
	fmt.Fprintf(&body, "%s failed: %v\n", label, err)
	fmt.Fprintf(&body, "rig: %s\n", rigName)
	if commit != "" {
		fmt.Fprintf(&body, "commit: %s\n", commit)
	}
	body.WriteString(strings.Join(diagnostic, "\n"))
	if logPath != "" {
		fmt.Fprintf(&body, "\nlog: %s", logPath)
	}
	return fmt.Errorf("%s", body.String())
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
