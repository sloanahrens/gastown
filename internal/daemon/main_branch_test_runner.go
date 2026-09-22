package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	// defaultMainBranchTestInterval is how often each rig's main_branch_test
	// patrol runs. Kept at 60m to avoid starving the container-gate slot:
	// 3 rigs × 30m × ~10m holds = slot busy ~50% of the hour, leaving
	// refinery gates and polecat gt done --pre-verified waiting 39-45m.
	// At 60m each rig holds the slot for ~10m of a 60m window, giving other
	// callers predictable 50m gaps (gt-uoqg).
	defaultMainBranchTestInterval = 60 * time.Minute
	defaultMainBranchTestTimeout  = 10 * time.Minute

	// maxDiagnosticLines bounds how many diagnostic lines go into the
	// escalation body. The body is filtered rather than tailed, so it holds
	// only failure evidence; the cap is a backstop against a pathological run
	// (hundreds of failing tests) making the escalation unreadable, not a
	// budget the useful evidence has to fit inside. The full output is always
	// persisted to disk first (gt-1s2g, gt-f57o).
	maxDiagnosticLines = 200

	// maxLoggedCommandChars bounds how much of the gate command is echoed into
	// a run's log line (see oneLine).
	maxLoggedCommandChars = 200

	// mainBranchTestSlotTimeout bounds how long the main_branch_test patrol
	// waits for the container-gate slot before giving up on a rig's run,
	// mirroring acquireBatchGateSlot's batchSlotTimeout (internal/cmd/mq_batch.go).
	mainBranchTestSlotTimeout = 60 * time.Minute

	// mainBranchTestSetupTimeout bounds the git fetch/worktree-add/rev-parse
	// steps that run before the container-gate slot is acquired. Kept
	// separate from the per-rig test timeout so a long slot wait (up to
	// mainBranchTestSlotTimeout) never eats into the budget the actual gate
	// command gets to run (gt-uvxy).
	mainBranchTestSetupTimeout = 5 * time.Minute
)

// diagnosticLinePattern matches the lines worth surfacing from a failing
// test/build run: test failures, panics, and build errors. Plain "ok"
// lines from passing packages are excluded so a long run with one early
// failure doesn't bury it under trailing successes (gt-1s2g).
//
// The pattern is anchored at column 0, because that is where go test writes
// every summary line: "FAIL\t<pkg>\t<secs>s" (which also carries a build
// failure's "[build failed]" suffix), "--- FAIL: TestX (secs)" for a top-level
// test, "panic:", and "# <pkg>" for a build error. Anything go test reports as
// *test output* — a test's own log lines, an assertion under a failing test, a
// test's failure message — is indented past column 0, and an indented match is
// how text that merely looks like go test output (a fixture echoed by a test
// in our own suite) got reported as evidence of a failure in a package that
// does not exist (gt-f57o, second defect).
//
// A failing subtest's marker is indented, but it needs no alternative of its
// own: its parent's top-level marker is printed too, and the parent's block
// consumption below carries the subtest and its assertion.
var diagnosticLinePattern = regexp.MustCompile(`^(FAIL|--- FAIL|panic:|# )`)

// failedTestLine is the marker go test prints for each failing test; the
// assertion block naming what actually broke follows it, indented deeper.
const failedTestLine = "--- FAIL"

// extractDiagnosticLines returns up to maxDiagnosticLines lines of output that
// diagnose a failure, in their original order: pattern matches (FAIL/panic/
// build errors), plus — for every "--- FAIL: TestName" line — the assertion
// block that follows it.
//
// The assertion block matters as much as the marker: "--- FAIL:
// TestWidgetRenders" says a test failed, while the block under it
// ("widget_test.go:42: expected 3, got 4") says why. That block matches no
// pattern of its own, so a filter that keeps only pattern matches reports
// failures with no cause (gt-f57o).
//
// scope is the set of packages the worktree can run (loadModulePackages): a
// line naming a package outside it did not come from a build of this tree, so
// reporting it would send the reader to a package that does not exist. nil
// means the worktree's packages could not be listed, and then every match is
// reported.
//
// Only lines that name a package are filtered. A marker that names none
// ("--- FAIL: TestX", a panic) is kept whatever its neighbors say: the packages
// run in parallel, so under -p=8 the package line after a marker can belong to
// another package's transcript — the 2026-09-21 gate capture has two real
// internal/daemon dolt failures in exactly that position, and dropping markers
// on positional evidence would cost real test names (gt-u4oq).
func extractDiagnosticLines(output string, scope modulePackages) []string {
	lines := strings.Split(output, "\n")
	keepLine := func(line string) bool {
		pkg, ok := packageNamedBy(line)
		return scope == nil || !ok || scope.contains(pkg)
	}

	var diagnostic []string
	appendLine := func(line string) bool { // false once the cap is reached
		if len(diagnostic) >= maxDiagnosticLines {
			return false
		}
		diagnostic = append(diagnostic, line)
		return true
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !diagnosticLinePattern.MatchString(line) || !keepLine(line) {
			continue
		}
		if !appendLine(line) {
			break
		}
		if !strings.HasPrefix(line, failedTestLine) {
			continue
		}

		// Consume this test's assertion block: every following line that is
		// blank-free and indented deeper than the "--- FAIL" line. A blank
		// line or a line at the same depth starts something else. A nested
		// "--- FAIL" for a subtest is itself indented deeper, so it and its
		// own block are consumed here and never re-appended by the outer
		// loop — which is the only way an indented marker reaches the body at
		// all, since the pattern above only matches at column 0.
		indent := leadingIndentWidth(line)
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if strings.TrimSpace(next) == "" || leadingIndentWidth(next) <= indent {
				break
			}
			if !appendLine(next) {
				break
			}
			i = j
		}
	}
	return diagnostic
}

// leadingIndentWidth returns how many leading space/tab characters a line has
// (a tab counts as one). Only relative depth matters here: go test indents a
// failing test's assertion block deeper than its "--- FAIL" line.
func leadingIndentWidth(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

// packageSummaryLine matches go test's per-package summary for a package that
// finished, the trace a run killed mid-suite leaves of how far it got: "ok
// <pkg> <secs>s" and "FAIL <pkg> <secs>s". Anchored at column 0 for the same
// reason diagnosticLinePattern is — that is where go test writes it, and an
// indented copy is test output about a line like this, not the line.
//
// "? <pkg> [no test files]" is deliberately not matched. go prints it while
// walking the package list, before anything runs, so a test-less package
// scheduled late would otherwise overwrite the last package that actually
// finished and report a package nothing was run from as the run's progress
// (gt-59yz).
var packageSummaryLine = regexp.MustCompile(`^(ok|FAIL)[ \t]+\S+`)

// lastPackageReported returns the last per-package summary line in output, or
// "" when the run produced none. It is the closest thing a deadline kill
// leaves to a progress marker: no failure pattern exists to extract, so
// naming where the transcript stops is what tells the reader the run was
// still working rather than dead (gt-59yz).
//
// scope filters the line's package the way extractDiagnosticLines does: a
// summary naming a package this worktree does not contain is text that only
// looks like go test output — a fixture echoed by a test — and reporting it
// as the run's progress points at a package that cannot have produced it
// (gt-u4oq). nil scope means the worktree's packages could not be listed.
func lastPackageReported(output string, scope modulePackages) string {
	last := ""
	for _, line := range strings.Split(output, "\n") {
		if !packageSummaryLine.MatchString(line) {
			continue
		}
		fields := strings.Fields(line)
		if scope != nil && len(fields) > 1 && !scope.contains(fields[1]) {
			continue
		}
		last = strings.TrimSpace(line)
	}
	return last
}

// packageNamedBy returns the package a column-0 line names, or ok false when it
// names none. go spells the failure of a package two ways: the test summary
// "FAIL\t<pkg>\t<secs>s" (a build failure carries a " [build failed]" suffix),
// and the build-error header "# <pkg> [<pkg>.test]".
//
// Indentation disqualifies a line: go prints these at column 0, so an indented
// copy is output *about* a failure — a fixture echoed by a test — not the
// failure itself.
func packageNamedBy(line string) (string, bool) {
	if leadingIndentWidth(line) != 0 {
		return "", false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", false
	}
	if fields[0] == "FAIL" || fields[0] == "#" {
		return fields[1], true
	}
	return "", false
}

// modulePackages is the set of packages go can name in a worktree — what a
// failure line has to be attributed to for it to be evidence about that
// worktree. A line attributed to anything else is text that only looks like go
// test output (gt-u4oq).
type modulePackages map[string]struct{}

// contains reports whether pkg is a package of the worktree the set was built
// from.
func (m modulePackages) contains(pkg string) bool {
	_, ok := m[pkg]
	return ok
}

// loadModulePackages returns the package paths of every module rooted under
// root — root's own go.mod plus any nested module — or nil when root holds no
// module or the tree could not be read. nil means "cannot scope" (gt-u4oq).
//
// A package go can name is a directory holding a .go file, addressed as its
// module path plus that directory's path within the module. The listing is
// coarser than go's own — a directory whose files are all excluded by build
// tags appears here though "go test ./..." would skip it — which is the safe
// direction for the caller: the set exists to reject a package that cannot
// exist, not to insist on one.
func loadModulePackages(root string) modulePackages {
	if root == "" {
		return nil
	}
	moduleRoots := map[string]string{} // dir -> module path go.mod declares
	goDirs := map[string]struct{}{}    // dirs holding a .go file
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipPackageDir(d.Name()) {
				return fs.SkipDir
			}
			modulePath, err := modulePathIn(path)
			if err != nil {
				return err
			}
			if modulePath != "" {
				moduleRoots[path] = modulePath
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") {
			goDirs[filepath.Dir(path)] = struct{}{}
		}
		return nil
	})
	if walkErr != nil || len(moduleRoots) == 0 {
		return nil
	}

	packages := modulePackages{}
	for dir := range goDirs {
		moduleRoot, modulePath, ok := nearestModuleRoot(moduleRoots, dir)
		if !ok {
			continue
		}
		rel, err := filepath.Rel(moduleRoot, dir)
		if err != nil {
			return nil
		}
		if rel == "." {
			packages[modulePath] = struct{}{}
			continue
		}
		packages[modulePath+"/"+filepath.ToSlash(rel)] = struct{}{}
	}
	return packages
}

// skipPackageDir reports whether a directory is one go leaves out of "./..." or
// one whose contents are not this tree's packages: go's own exclusions
// (testdata, and the "." and "_" prefixes), the vendor tree, and the two
// dependency trees that make a walk expensive without adding a package go would
// test here.
func skipPackageDir(name string) bool {
	switch name {
	case "vendor", "node_modules", "testdata":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// nearestModuleRoot returns the module root at or above dir and the path that
// module declares. The innermost root wins: a directory inside a nested module
// is addressed by the nested module, not the one that contains it.
func nearestModuleRoot(moduleRoots map[string]string, dir string) (root, modulePath string, ok bool) {
	for {
		if modulePath, found := moduleRoots[dir]; found {
			return dir, modulePath, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

// modulePathIn returns the module path declared by dir/go.mod, "" when dir
// holds no go.mod. An unreadable go.mod is an error rather than an absent
// module, so a partially read tree withdraws the scope instead of shifting
// its packages onto the parent module's path (gt-u4oq).
func modulePathIn(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // G304: path from the worktree walk
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "module ")
		if !ok {
			continue
		}
		if comment := strings.Index(rest, "//"); comment >= 0 {
			rest = rest[:comment]
		}
		return strings.Trim(strings.TrimSpace(rest), `"`), nil
	}
	return "", nil
}

// oneLine flattens s onto a single log line, escaping newlines and bounding
// the length.
//
// This is not cosmetic. The run's log is the text the failure extractor reads,
// and the log line that announces the command echoes it verbatim: a command
// carrying newlines (any test that passes go-test-shaped text as the command —
// internal/daemon's own fixture-driven tests do) would put "--- FAIL: TestX"
// and "FAIL\t<pkg>\t<secs>s" lines at column 0 of that log, and the extractor
// would report them as the failure. That is how the mayor was handed
// 'internal/widget', a package that does not exist, on 2026-09-21 (gt-f57o,
// second defect). A log entry is one line; nothing echoed into it may add
// another.
func oneLine(s string) string {
	s = strings.NewReplacer("\r\n", `\n`, "\n", `\n`, "\r", `\n`).Replace(s)
	if runes := []rune(s); len(runes) > maxLoggedCommandChars {
		s = string(runes[:maxLoggedCommandChars]) + "..."
	}
	return s
}

// writeMainBranchTestLog captures the full output of a failed run to
// <townRoot>/logs/main_branch_test/<rig>-<sha>-<ts>.log so the mayor can
// inspect it without rerunning the test. The tested head is in the name
// because the temporary worktree the run happened in is removed afterwards,
// taking the output with it — without the sha in the name, a log could not be
// matched to the verdict it belongs to (gt-1s2g, gt-f57o).
func writeMainBranchTestLog(townRoot, rigName, commit, output string) (string, error) {
	dir := filepath.Join(townRoot, "logs", "main_branch_test")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating log dir: %w", err)
	}
	logPath := filepath.Join(dir, fmt.Sprintf("%s-%s-%s.log", rigName, shortCommit(commit), time.Now().UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(logPath, []byte(output), 0644); err != nil {
		return "", fmt.Errorf("writing log file: %w", err)
	}
	return logPath, nil
}

// shortCommit abbreviates a commit sha for a log line or filename. An
// undetermined head (commitTested already logged why) still gets a readable
// label rather than an empty segment, since the log is worth keeping either
// way.
func shortCommit(commit string) string {
	if commit == "" {
		return "unknown"
	}
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// hostLoad is a snapshot of the host-utilization numbers a main_branch_test
// verdict is qualified by, so the mayor can tell a real regression from a
// saturated box without rerunning the suite by hand (gt-f57o).
type hostLoad struct {
	// IdlePercent estimates how much of the CPU is idle.
	IdlePercent float64
	// Load1 is the 1-minute load average the estimate came from.
	Load1 float64
	// NumCPU is the core count the load was measured against.
	NumCPU int
}

func (h hostLoad) String() string {
	return fmt.Sprintf("CPU idle %.1f%% (load1 %.2f on %d cores)", h.IdlePercent, h.Load1, h.NumCPU)
}

// measureHostLoad reads the current host utilization.
func measureHostLoad() hostLoad {
	h := hostLoad{Load1: loadAverage1(), NumCPU: runtime.NumCPU()}
	h.IdlePercent = computeCPUIdlePercent(h.Load1, h.NumCPU)
	return h
}

// measureHostLoadFn is the seam tests use to pin a host-load reading;
// production always measures the real host (see stubHostLoad). Without it the
// skip decision could only be tested by saturating the actual machine
// (gt-f57o, same pattern as slot.SetContainerListerForTest).
var measureHostLoadFn = measureHostLoad

// EstimateCPUIdlePercent gives callers outside this package the same
// host-busy signal hostBusyReason uses (gt-f57o): one load-average read, no
// multi-second sampling.
func EstimateCPUIdlePercent() float64 {
	return measureHostLoad().IdlePercent
}

// computeCPUIdlePercent converts a 1-minute load average into an idle
// percentage: load 0 is 100% idle, load == cores is 0% idle, and more load
// than cores clamps to 0. A non-positive core count reports 100 — nothing
// readable means nothing to gate on, and the gate must fail open rather than
// into a skipped cycle nobody asked for.
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

// hostBusyReason returns why a cycle must be skipped because the host is too
// busy for a red verdict to be trustworthy, or "" when the cycle should run.
// A pure function of the measured values, so the decision is testable without
// a saturated host (gt-f57o).
//
// A floor outside (0, 100] means the gate is off: 0 or less disables it, and
// a percentage above 100 could never be met, which would skip every cycle
// forever. Both run rather than let a config typo silently starve the patrol
// that catches regressions in main.
func hostBusyReason(minIdlePercent float64, host hostLoad) string {
	if minIdlePercent <= 0 || minIdlePercent > 100 {
		return ""
	}
	if host.IdlePercent >= minIdlePercent {
		return ""
	}
	return fmt.Sprintf("host busy: %s < %.0f%% idle minimum", host, minIdlePercent)
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

	// MinCPUIdlePercent is the minimum estimated CPU idle percentage required
	// before a cycle starts. Below it the host is too busy for a red verdict
	// to be trustworthy — the suite's own contention produces failures that
	// pass in isolation — so the cycle is skipped and logged as
	// "skipped: host busy" instead of escalating a false FAILED (gt-f57o).
	//
	// Disabled by default (0), like the sibling spawn-pressure gate
	// (operational.daemon.pressure_cpu_threshold, also opt-in): a gate that
	// skips by default can silently stop the patrol that catches regressions
	// in main. Set 25 to skip a cycle when less than a quarter of the host's
	// CPU is idle. Values outside (0, 100] are treated as disabled.
	MinCPUIdlePercent *float64 `json:"min_cpu_idle_percent,omitempty"`
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

// mainBranchTestMinCPUIdlePercent returns the configured CPU-idle floor, or 0
// (host-busy gate disabled) when unset.
func mainBranchTestMinCPUIdlePercent(config *DaemonPatrolConfig) float64 {
	if config != nil && config.Patrols != nil && config.Patrols.MainBranchTest != nil &&
		config.Patrols.MainBranchTest.MinCPUIdlePercent != nil {
		return *config.Patrols.MainBranchTest.MinCPUIdlePercent
	}
	return 0
}

// rigGateConfig holds the gate/test configuration extracted from a rig's config.json.
type rigGateConfig struct {
	SetupCommand string
	TestCommand  string
	Gates        map[string]string // gate name → command
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
		SetupCommand *string                    `json:"setup_command"`
		TestCommand  *string                    `json:"test_command"`
		Gates        map[string]json.RawMessage `json:"gates"`
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

	if mq.SetupCommand != nil && *mq.SetupCommand != "" {
		cfg.SetupCommand = *mq.SetupCommand
	}

	if len(cfg.Gates) == 0 && cfg.TestCommand == "" {
		return nil, nil // No runnable commands
	}

	return cfg, nil
}

// triggerMainBranchTests starts a main_branch_test cycle on its own
// goroutine, guarded by mainBranchTestRunning so an overlapping tick is
// skipped rather than stacking a second concurrent cycle on top of one still
// waiting on a container-gate slot or mid-run (gt-uvxy). Returns true if a
// cycle was started, false if one was already in progress.
func (d *Daemon) triggerMainBranchTests() bool {
	if !d.mainBranchTestRunning.CompareAndSwap(false, true) {
		d.logger.Printf("main_branch_test: previous cycle still running, skipping this tick")
		return false
	}
	go func() {
		defer d.mainBranchTestRunning.Store(false)
		d.runMainBranchTests()
	}()
	return true
}

// errMainBranchTestInterrupted marks a run that the daemon itself stopped —
// its context (d.ctx, or one descending from it) was canceled while a gate
// command was in flight — as opposed to a run that reached a verdict about
// main. The patrol loop reports it and counts it as neither pass nor fail
// (gt-59yz); see analyzeRunFailure for where it is raised.
var errMainBranchTestInterrupted = errors.New("main_branch_test run interrupted")

// runMainBranchTests runs quality gates on each rig's main branch.
// It fetches the latest main, runs configured gates/tests, and escalates failures.
func (d *Daemon) runMainBranchTests() {
	if !d.isPatrolActive("main_branch_test") {
		return
	}

	d.logger.Printf("main_branch_test: starting patrol cycle")

	// Opt-in host-busy gate: on a saturated host the suite's own contention
	// produces failures that pass in isolation, so a red verdict from such a
	// run is not evidence of a regression (gt-f57o). Disabled unless
	// patrols.main_branch_test.min_cpu_idle_percent is set, and every skip is
	// logged with the measured values so a silently-never-running patrol is
	// visible in the daemon log rather than inferred from a missing cycle.
	if minIdle := mainBranchTestMinCPUIdlePercent(d.patrolConfig); minIdle > 0 {
		host := measureHostLoadFn()
		if reason := hostBusyReason(minIdle, host); reason != "" {
			d.logger.Printf("main_branch_test: skipped: %s", reason)
			return
		}
		// Say which of the two "did not skip" cases this is: a floor above
		// 100 is a config typo the gate ignores, and reporting it as a
		// satisfied minimum would make the typo invisible forever.
		if minIdle > 100 {
			d.logger.Printf("main_branch_test: ignoring out-of-range min_cpu_idle_percent %.0f (valid range (0,100]); running cycle", minIdle)
		} else {
			d.logger.Printf("main_branch_test: host is idle enough to run: %s >= %.0f%% minimum", host, minIdle)
		}
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
		err := d.testRigMainBranch(rigName, rigPath, timeout)
		if errors.Is(err, errMainBranchTestInterrupted) {
			// The daemon stopped this run (its context was canceled while the
			// gate command was in flight), so nothing was verified and the kill
			// signal says nothing about main. Counting it either way would
			// misreport: as a failure it is the same false red as a timeout
			// reported as a crash, and as a pass it would clear a live alert on
			// a run that checked nothing (gt-59yz).
			d.logger.Printf("main_branch_test: %s: interrupted, not a verdict about main: %v", rigName, err)
			continue
		}
		if err != nil {
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
		d.escalateAlert(alertKeyMainBranchTest, "main_branch_test", msg)
	} else if tested > 0 {
		// Every rig that was checked passed, so the condition the alert
		// describes is gone. Guarded on tested > 0: a cycle that ran nothing
		// (all rigs filtered out by config) re-checked nothing and must not
		// clear an alert that may still hold.
		d.clearAlerts("main branch tests green", alertKeyMainBranchTest)
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

	// Setup (fetch + worktree add + commit lookup) runs on its own bounded
	// context, separate from the test-run timeout created below after the
	// slot is held — see mainBranchTestSetupTimeout's comment.
	setupCtx, setupCancel := context.WithTimeout(d.ctx, mainBranchTestSetupTimeout)
	defer setupCancel()

	// Fetch latest main
	fetchCmd := exec.CommandContext(setupCtx, "git", "fetch", "origin", defaultBranch)
	fetchCmd.Dir = bareRepoPath
	util.SetDetachedProcessGroup(fetchCmd)
	if output, err := fetchCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}

	// Create temporary worktree at origin/<default_branch>
	addCmd := exec.CommandContext(setupCtx, "git", "worktree", "add", "--detach", worktreePath, "origin/"+defaultBranch)
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

	commit := d.commitTested(setupCtx, rigName, worktreePath)

	// Acquire the container-gate slot before running gates/tests: this
	// baseline run spins the same Docker-backed suites (Dolt, testcontainers)
	// that every other caller guards with 'gt slot run', so it must be a
	// first-class slot holder instead of an invisible occupant that collides
	// with other rigs' container suites on the shared Docker VM (gt-hpce;
	// same class as gt-afe4/gt-tuiy, wrapped here in Go rather than shelling
	// out, matching acquireBatchGateSlot in internal/cmd/mq_batch.go). The
	// hold must also stay visible to the processes this run spawns — agent
	// sessions, dogs, plugins — which inherit the reentrant marker: it names
	// this runner's role, so a refinery gate or 'gt done' verify suite
	// descending from it queues behind this hold instead of skipping its
	// lock (gt-off9, see acquireMainBranchTestSlot).
	h, err := acquireMainBranchTestSlot(d.config.TownRoot, rigName)
	if err != nil {
		return fmt.Errorf("acquiring container-gate slot: %w", err)
	}
	defer func() { _ = h.Release() }()

	// The test-run timeout starts here, after the slot is held, not at the
	// top of this function — a slot wait can take up to
	// mainBranchTestSlotTimeout (60m), and starting the clock before that
	// wait handed the gate command an already-expired context, producing a
	// "context deadline exceeded" false red with no evidence the command
	// itself ever ran (gt-uvxy).
	runCtx, runCancel := context.WithTimeout(d.ctx, timeout)
	defer runCancel()

	return d.runRigGates(runCtx, rigName, commit, worktreePath, gateCfg)
}

// runRigGates runs the configured setup command (if any) followed by gates
// or the legacy test command, in that order, on the given worktree. Setup
// runs first, using the same timeout ctx, because a fresh worktree from
// .repo.git has no installed dependencies — any rig needing an install step
// (e.g. npm ci for mango's nextapp/jest) would otherwise fail every cycle
// with a misleading "test failed" when the real problem is "setup failed"
// (gt-znj8). A missing setup_command is a no-op, so rigs that don't
// configure one are unaffected. Split out from testRigMainBranch so the
// setup-then-gates ordering is testable without the git-worktree/bare-repo
// harness that function requires (gt-znj8, following
// acquireMainBranchTestSlot's precedent for the same need above).
func (d *Daemon) runRigGates(ctx context.Context, rigName, commit, workDir string, gateCfg *rigGateConfig) error {
	if gateCfg.SetupCommand != "" {
		if err := d.runCommandOnWorktree(ctx, rigName, commit, workDir, "setup", gateCfg.SetupCommand); err != nil {
			return err
		}
	}

	if len(gateCfg.Gates) > 0 {
		return d.runGatesOnWorktree(ctx, rigName, commit, workDir, gateCfg.Gates)
	}
	return d.runCommandOnWorktree(ctx, rigName, commit, workDir, "test", gateCfg.TestCommand)
}

// acquireMainBranchTestSlot acquires the container-gate slot for a rig's
// main_branch_test run. Split out from testRigMainBranch so it's testable
// without the full git-worktree/bare-repo harness that function requires
// (gt-hpce, following acquireBatchGateSlot's precedent in
// internal/cmd/mq_batch.go).
//
// slot.AcquirePoolReal, not slot.AcquirePool (gt-off9): the daemon is a
// long-lived process, and the marker it inherits from its own ancestors
// outlives them, so riding a reentrant marker could hand this runner a slot
// nobody holds — its hold would then be invisible to everyone else, with no
// flock, no owner file and no docker-ps check. See AcquirePoolReal's doc
// comment.
func acquireMainBranchTestSlot(townRoot, rigName string) (*slot.Handle, error) {
	cg := agentconfig.LoadOperationalConfig(townRoot).GetContainerGateConfig()
	pool := slot.Pool{Slots: cg.SlotsV(), ReservedForGate: cg.ReservedForGateV()}
	return slot.AcquirePoolReal(townRoot, rigName+"/main-branch-test", mainBranchTestSlotTimeout, pool)
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
	interrupted := false
	for name, cmd := range gates {
		if err := d.runCommandOnWorktree(ctx, rigName, commit, workDir, name, cmd); err != nil {
			failures = append(failures, fmt.Sprintf("gate %q: %v", name, err))
			// The gates' messages are joined as text, which would drop the
			// sentinel: a canceled run has to stay recognizable as a stopped
			// run, not become an ordinary failure on the way out (gt-59yz).
			interrupted = interrupted || errors.Is(err, errMainBranchTestInterrupted)
		}
	}
	if len(failures) > 0 {
		joined := strings.Join(failures, "; ")
		if interrupted {
			return fmt.Errorf("%w: %s", errMainBranchTestInterrupted, joined)
		}
		return fmt.Errorf("%s", joined)
	}
	return nil
}

// runCommandOnWorktree runs a single shell command in the given worktree directory.
// On failure it captures the full output to a log file and builds an error
// carrying the failing rig, the commit tested, the host load at start and end
// of the run, the lines that actually diagnose the failure (FAIL/panic/
// build-error lines and their assertions, not a blind tail that can be
// all-"ok" noise from later packages), and the log path so the mayor can
// triage without rerunning (gt-1s2g, gt-f57o).
func (d *Daemon) runCommandOnWorktree(ctx context.Context, rigName, commit, workDir, label, command string) error {
	// The tested head goes in the log line: a verdict that cannot be matched
	// to a head cannot be checked against the refinery's green run on the
	// same commit (gt-f57o). The command goes through oneLine so that echoing
	// it cannot add lines to the log the extractor reads.
	d.logger.Printf("main_branch_test: %s: running %s on %s: %s", rigName, label, shortCommit(commit), oneLine(command))

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // G204: command is from trusted rig config
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "CI=true") // Signal test environment
	// SetProcessGroup, not SetDetachedProcessGroup: the gate command is a shell
	// that execs go test, which spawns <pkg>.test binaries that inherit the
	// output pipes. CommandContext's default Cancel SIGKILLs only the direct
	// child, so the orphaned test binaries keep the pipes open and Wait — which
	// blocks on the copy goroutines when WaitDelay is unset — returns long
	// after the deadline, or never. That is the shape behind the 2026-09-11
	// runs: a 10m deadline whose kill surfaced 11m21s after the suite started
	// (gt-59yz). The Cancel hook kills the whole group, and WaitDelay bounds
	// the drain so a stuck pipe-holder can't hold the slot and the worktree
	// open past it (gt-pxlg's sibling: internal/daemon/plugin_script.go).
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	startHost := measureHostLoadFn()
	start := time.Now()
	output, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	endHost := measureHostLoadFn()
	if err == nil {
		return nil
	}

	// budget is how much of the run's own clock this command was given: the
	// context's remaining lifetime when it started, not the configured timeout,
	// since setup and every gate share one context and can leave this command
	// with less (gt-59yz).
	budget := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		budget = max(deadline.Sub(start), 0)
	}

	bodyText, interrupted := analyzeRunFailure(runFailure{
		label:   label,
		output:  string(output),
		err:     err,
		ctxErr:  ctx.Err(),
		budget:  budget,
		elapsed: elapsed,
		rigName: rigName,
		commit:  commit,
		hosts:   [2]string{startHost.String(), endHost.String()},
		workDir: workDir,
	})

	logPath, logErr := writeMainBranchTestLog(d.config.TownRoot, rigName, commit, string(output))
	if logErr != nil {
		d.logger.Printf("main_branch_test: %s: warning: could not write diagnostic log: %v", rigName, logErr)
	}
	if logPath != "" {
		bodyText += "\nlog: " + logPath
	}
	if interrupted {
		// Carried out as a distinct error so the patrol cycle can refuse to
		// count it as a verdict about main (gt-59yz).
		return fmt.Errorf("%w: %s", errMainBranchTestInterrupted, bodyText)
	}
	return errors.New(bodyText)
}

// runFailure is everything one failed gate/test invocation knows about itself
// when the body is written: what it printed, why it ended, and the clocks that
// say whether it ended on a verdict or on the run's own budget.
type runFailure struct {
	label   string
	output  string
	err     error
	ctxErr  error
	budget  time.Duration // the run clock this command was given
	elapsed time.Duration // start to return, including any post-kill drain
	rigName string
	commit  string
	hosts   [2]string
	workDir string
}

// analyzeRunFailure builds the escalation body for a command that did not
// succeed, naming which of the four ways it ended this was. The distinction is
// the whole defect gt-59yz reports: exec.CommandContext stops a command that
// overran its context with SIGKILL, which reaches the caller as the bare
// "signal: killed" — a string that reads as a crash and names no cause. The
// context's own error is what separates "ran out of time" from "died", and it
// is readable only there, before the deferred cancel. gastown's suite runs ~12m
// against this runner's 10m budget, so the crash wording was escalated every
// cycle alongside a 100%-green transcript.
//
// Only the last case is a verdict about main. A timeout is the runner's own
// clock, a command that never started is the run's budget spent by earlier
// gates, and an interrupted command is the daemon stopping — in all three the
// reader is told what happened instead of being handed a crash report.
// It returns the body plus whether the run was interrupted, which the caller
// turns into errMainBranchTestInterrupted so the cycle does not count a
// stopped run as either a pass or a failure. The body comes back as a string,
// not the strings.Builder it was assembled in: a Builder must not be copied,
// and the caller still appends the log path.
func analyzeRunFailure(f runFailure) (string, bool) {
	timedOut := f.ctxErr == context.DeadlineExceeded
	interrupted := f.ctxErr == context.Canceled
	// os/exec's Start short-circuits on an already-expired context and hands
	// back ctx.Err() with nothing run and nothing printed, which must not be
	// reported as a command that was still going when the deadline fired.
	neverStarted := timedOut && errors.Is(f.err, context.DeadlineExceeded) && f.output == ""

	// The worktree's own packages bound what can be a failure here: a transcript
	// naming a package this tree does not contain is go-test-shaped text from
	// somewhere else — a fixture echoed into the output — and naming it as the
	// failure sends the mayor to a package that does not exist (gt-u4oq).
	scope := loadModulePackages(f.workDir)
	diagnostic := extractDiagnosticLines(f.output, scope)
	// Matches before scoping, which is a different question from the scoped
	// count: lines dropped as another tree's packages still mean the run
	// reported failures, so they must not license the "transcript is green"
	// reassurance below (gt-59yz).
	matchedAnyFailureLine := len(extractDiagnosticLines(f.output, nil)) > 0

	var body strings.Builder
	switch {
	case interrupted:
		fmt.Fprintf(&body, "%s was stopped, not failed: the run's context was canceled while the command was in flight (daemon shutting down or restarting), so %v says nothing about main\n", f.label, f.err)
	case neverStarted:
		fmt.Fprintf(&body, "%s did not run: the run's budget was already spent when this command started, so nothing was verified on main (err: %v)\n", f.label, f.err)
	case timedOut:
		// The escalation's first line is the verdict the mayor reads, and
		// "signal: killed" there was the whole false red. The timeout leads
		// instead, and the signal it caused is named as its own doing.
		fmt.Fprintf(&body, "%s TIMED OUT after %s of the run's budget — the command was still running when its deadline fired (killed by: %v; this is the runner's timeout, not a crash)\n", f.label, f.budget.Round(time.Second), f.err)
		fmt.Fprintf(&body, "run budget: patrols.main_branch_test.timeout for this rig, shared by setup and every gate and started after the slot wait; the command returned %s after it started (deadline plus any wait for orphaned children to release the output pipes) — raise it if the suite legitimately needs longer\n", f.elapsed.Round(time.Second))
		if last := lastPackageReported(f.output, scope); last != "" {
			// Packages run in parallel, so this names where the run had
			// reached when the deadline hit, not which package was slow.
			fmt.Fprintf(&body, "last package reported: %s\n", last)
		}
	default:
		fmt.Fprintf(&body, "%s failed: %v\n", f.label, f.err)
	}
	fmt.Fprintf(&body, "rig: %s\n", f.rigName)
	if f.commit != "" {
		fmt.Fprintf(&body, "commit: %s\n", f.commit)
	}
	// Contention context, so a red that is really this town's documented
	// full-suite flakiness is distinguishable from a regression without
	// re-running the package by hand (gt-f57o).
	fmt.Fprintf(&body, "host at start: %s\n", f.hosts[0])
	fmt.Fprintf(&body, "host at end: %s\n", f.hosts[1])

	// The tail of a run that was killed rather than concluded, with no failure
	// pattern in it: its transcript stops wherever the suite had got to, and on
	// this suite that point is passing, so the tail is all "ok" lines.
	// Presented as failure evidence it reads as a report that everything
	// passed, which is how this reached the mayor as a red main every 30m.
	// Keep it, labeled for what it is.
	greenTailIsOnlyContext := false
	if len(diagnostic) == 0 && !neverStarted {
		// Nothing matched a known failure pattern (e.g. a shell error before
		// the test binary even ran) — fall back to a short tail so the body
		// isn't empty.
		greenTailIsOnlyContext = timedOut && !matchedAnyFailureLine
		lines := strings.Split(strings.TrimSpace(f.output), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		diagnostic = lines
	}
	if greenTailIsOnlyContext {
		body.WriteString("no FAIL/panic/build-error line matched: the killed run's transcript is green, so the tail below is context (how far the run got), not a defect\n")
	}
	body.WriteString(strings.Join(diagnostic, "\n"))
	return body.String(), interrupted
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
