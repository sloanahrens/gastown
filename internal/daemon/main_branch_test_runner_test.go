package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/slot"
)

func TestMainBranchTestInterval(t *testing.T) {
	// Nil config returns default
	if got := mainBranchTestInterval(nil); got != defaultMainBranchTestInterval {
		t.Errorf("expected default %v, got %v", defaultMainBranchTestInterval, got)
	}

	// Configured interval
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			MainBranchTest: &MainBranchTestConfig{
				Enabled:     true,
				IntervalStr: "15m",
			},
		},
	}
	if got := mainBranchTestInterval(config); got.Minutes() != 15 {
		t.Errorf("expected 15m, got %v", got)
	}

	// Invalid interval returns default
	config.Patrols.MainBranchTest.IntervalStr = "bad"
	if got := mainBranchTestInterval(config); got != defaultMainBranchTestInterval {
		t.Errorf("expected default for invalid interval, got %v", got)
	}
}

func TestMainBranchTestTimeout(t *testing.T) {
	// Nil config returns default
	if got := mainBranchTestTimeout(nil); got != defaultMainBranchTestTimeout {
		t.Errorf("expected default %v, got %v", defaultMainBranchTestTimeout, got)
	}

	// Configured timeout
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			MainBranchTest: &MainBranchTestConfig{
				Enabled:    true,
				TimeoutStr: "5m",
			},
		},
	}
	if got := mainBranchTestTimeout(config); got.Minutes() != 5 {
		t.Errorf("expected 5m, got %v", got)
	}
}

func TestMainBranchTestRigs(t *testing.T) {
	// Nil config returns nil
	if got := mainBranchTestRigs(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}

	// Configured rigs
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			MainBranchTest: &MainBranchTestConfig{
				Enabled: true,
				Rigs:    []string{"gastown", "beads"},
			},
		},
	}
	got := mainBranchTestRigs(config)
	if len(got) != 2 || got[0] != "gastown" || got[1] != "beads" {
		t.Errorf("expected [gastown beads], got %v", got)
	}
}

func TestIsPatrolEnabledMainBranchTest(t *testing.T) {
	// Nil config — disabled (opt-in)
	if IsPatrolEnabled(nil, "main_branch_test") {
		t.Error("expected main_branch_test disabled with nil config")
	}

	// Explicitly disabled
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			MainBranchTest: &MainBranchTestConfig{
				Enabled: false,
			},
		},
	}
	if IsPatrolEnabled(config, "main_branch_test") {
		t.Error("expected main_branch_test disabled when Enabled=false")
	}

	// Enabled
	config.Patrols.MainBranchTest.Enabled = true
	if !IsPatrolEnabled(config, "main_branch_test") {
		t.Error("expected main_branch_test enabled when Enabled=true")
	}
}

func TestLoadRigGateConfig(t *testing.T) {
	t.Run("no config file", func(t *testing.T) {
		cfg, err := loadRigGateConfig("/nonexistent/path")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg != nil {
			t.Errorf("expected nil config for nonexistent path, got %+v", cfg)
		}
	})

	t.Run("no merge_queue section", func(t *testing.T) {
		dir := t.TempDir()
		data := `{"type":"rig","version":1,"name":"test"}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadRigGateConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg != nil {
			t.Errorf("expected nil config for no merge_queue, got %+v", cfg)
		}
	})

	t.Run("test_command only", func(t *testing.T) {
		dir := t.TempDir()
		data := map[string]interface{}{
			"merge_queue": map[string]interface{}{
				"test_command": "go test ./...",
			},
		}
		raw, _ := json.Marshal(data)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadRigGateConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if cfg.TestCommand != "go test ./..." {
			t.Errorf("expected 'go test ./...', got %q", cfg.TestCommand)
		}
	})

	t.Run("gates configured", func(t *testing.T) {
		dir := t.TempDir()
		data := map[string]interface{}{
			"merge_queue": map[string]interface{}{
				"gates": map[string]interface{}{
					"build": map[string]interface{}{"cmd": "go build ./..."},
					"test":  map[string]interface{}{"cmd": "go test ./..."},
					"lint":  map[string]interface{}{"cmd": "golangci-lint run"},
				},
			},
		}
		raw, _ := json.Marshal(data)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadRigGateConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if len(cfg.Gates) != 3 {
			t.Errorf("expected 3 gates, got %d", len(cfg.Gates))
		}
		if cfg.Gates["build"] != "go build ./..." {
			t.Errorf("expected build gate 'go build ./...', got %q", cfg.Gates["build"])
		}
	})

	t.Run("setup_command with test_command", func(t *testing.T) {
		dir := t.TempDir()
		data := map[string]interface{}{
			"merge_queue": map[string]interface{}{
				"setup_command": "npm ci",
				"test_command":  "npx jest",
			},
		}
		raw, _ := json.Marshal(data)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadRigGateConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if cfg.SetupCommand != "npm ci" {
			t.Errorf("expected setup command 'npm ci', got %q", cfg.SetupCommand)
		}
		if cfg.TestCommand != "npx jest" {
			t.Errorf("expected test command 'npx jest', got %q", cfg.TestCommand)
		}
	})

	t.Run("missing setup_command unchanged", func(t *testing.T) {
		dir := t.TempDir()
		data := map[string]interface{}{
			"merge_queue": map[string]interface{}{
				"test_command": "go test ./...",
			},
		}
		raw, _ := json.Marshal(data)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadRigGateConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if cfg.SetupCommand != "" {
			t.Errorf("expected empty setup command, got %q", cfg.SetupCommand)
		}
		if cfg.TestCommand != "go test ./..." {
			t.Errorf("expected test command unchanged, got %q", cfg.TestCommand)
		}
	})

	t.Run("no test commands", func(t *testing.T) {
		dir := t.TempDir()
		data := map[string]interface{}{
			"merge_queue": map[string]interface{}{
				"enabled": true,
			},
		}
		raw, _ := json.Marshal(data)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadRigGateConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg != nil {
			t.Errorf("expected nil for no test commands, got %+v", cfg)
		}
	})
}

// goTestFixtureOneFailingPackage mimics real `go test ./...` output where
// most packages pass and one, deep in the middle of the run, fails — the
// exact shape that let the old last-50-lines tail bury the failure under
// later "ok" lines (gt-1s2g).
const goTestFixtureOneFailingPackage = `ok  	github.com/steveyegge/gastown/internal/aaa	0.010s
ok  	github.com/steveyegge/gastown/internal/bbb	0.020s
--- FAIL: TestWidgetRenders (0.00s)
    widget_test.go:42: expected 3, got 4
FAIL
FAIL	github.com/steveyegge/gastown/internal/widget	0.030s
ok  	github.com/steveyegge/gastown/internal/ccc	0.010s
ok  	github.com/steveyegge/gastown/internal/ddd	0.010s
ok  	github.com/steveyegge/gastown/internal/eee	0.010s
`

func TestExtractDiagnosticLines(t *testing.T) {
	diagnostic := extractDiagnosticLines(goTestFixtureOneFailingPackage)

	found := false
	for _, line := range diagnostic {
		if strings.Contains(line, "internal/widget") {
			found = true
		}
		if strings.HasPrefix(line, "ok") {
			t.Errorf("expected only failure lines, got passing-package line: %q", line)
		}
	}
	if !found {
		t.Errorf("expected diagnostic lines to name the failing package internal/widget, got %v", diagnostic)
	}
}

func TestExtractDiagnosticLines_CapsAtMax(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < maxDiagnosticLines+10; i++ {
		sb.WriteString("--- FAIL: TestSomething\n")
	}
	diagnostic := extractDiagnosticLines(sb.String())
	if len(diagnostic) != maxDiagnosticLines {
		t.Errorf("expected %d lines, got %d", maxDiagnosticLines, len(diagnostic))
	}
}

func TestWriteMainBranchTestLog(t *testing.T) {
	townRoot := t.TempDir()
	logPath, err := writeMainBranchTestLog(townRoot, "gastown", "37ab61b2c4d1e5f6", goTestFixtureOneFailingPackage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(logPath, filepath.Join(townRoot, "logs", "main_branch_test")) {
		t.Errorf("expected log under logs/main_branch_test, got %q", logPath)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("could not read log file: %v", err)
	}
	if string(data) != goTestFixtureOneFailingPackage {
		t.Errorf("log file content mismatch")
	}
}

// TestWriteMainBranchTestLog_NamesTheTestedHead is the gt-f57o requirement
// that a persisted log can be matched to the head it was produced from: the
// temporary worktree is removed after the run, so the log file is the only
// surviving record, and a log named only by rig and timestamp cannot be tied
// to the verdict it belongs to.
func TestWriteMainBranchTestLog_NamesTheTestedHead(t *testing.T) {
	townRoot := t.TempDir()
	logPath, err := writeMainBranchTestLog(townRoot, "gastown", "37ab61b2c4d1e5f6a7b8", goTestFixtureOneFailingPackage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base := filepath.Base(logPath); !strings.Contains(base, "37ab61b2c4d1") {
		t.Errorf("expected the tested head in the log filename, got %q", base)
	}

	// An undetermined head still yields a usable name rather than an empty
	// segment or a bare "gastown-.log".
	logPath, err = writeMainBranchTestLog(townRoot, "gastown", "", goTestFixtureOneFailingPackage)
	if err != nil {
		t.Fatalf("unexpected error for unknown head: %v", err)
	}
	if base := filepath.Base(logPath); !strings.HasPrefix(base, "gastown-unknown-") {
		t.Errorf("expected an 'unknown' segment for an undetermined head, got %q", base)
	}
}

// TestExtractDiagnosticLines_KeepsAssertionAfterFail is the gt-f57o core
// requirement: the escalation must name the failing TEST and what it
// asserted, not just the package. A "--- FAIL: TestX" marker with no
// assertion line says a test failed but not what broke, and the assertion
// line matches no failure pattern of its own.
func TestExtractDiagnosticLines_KeepsAssertionAfterFail(t *testing.T) {
	diagnostic := extractDiagnosticLines(goTestFixtureOneFailingPackage)
	joined := strings.Join(diagnostic, "\n")

	if !strings.Contains(joined, "--- FAIL: TestWidgetRenders") {
		t.Errorf("expected the failing test name, got:\n%s", joined)
	}
	if !strings.Contains(joined, "widget_test.go:42: expected 3, got 4") {
		t.Errorf("expected the assertion line that follows the failing test, got:\n%s", joined)
	}
}

// TestExtractDiagnosticLines_KeepsNestedSubtestBlock covers the shape go test
// prints for a failing subtest: the subtest's "--- FAIL" and its assertion are
// both indented deeper than the parent's block, so a filter that anchored on
// column zero, or that consumed only one line per marker, would lose them.
func TestExtractDiagnosticLines_KeepsNestedSubtestBlock(t *testing.T) {
	const fixture = `--- FAIL: TestParent (0.00s)
    --- FAIL: TestParent/sub (0.00s)
        parent_test.go:88: got 1, want 2
FAIL
FAIL	github.com/steveyegge/gastown/internal/parent	0.050s
`
	joined := strings.Join(extractDiagnosticLines(fixture), "\n")
	if !strings.Contains(joined, "parent_test.go:88: got 1, want 2") {
		t.Errorf("expected the nested subtest assertion, got:\n%s", joined)
	}
	if strings.Count(joined, "--- FAIL: TestParent/sub (0.00s)") != 1 {
		t.Errorf("expected the nested failure to appear exactly once, got:\n%s", joined)
	}
	if !strings.Contains(joined, "internal/parent") {
		t.Errorf("expected the failing package summary, got:\n%s", joined)
	}
}

// TestExtractDiagnosticLines_CapHoldsWithAssertionBlocks proves the cap is
// enforced even when every entry carries an assertion block, which can consume
// two lines per marker.
func TestExtractDiagnosticLines_CapHoldsWithAssertionBlocks(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < maxDiagnosticLines; i++ {
		sb.WriteString("--- FAIL: TestSomething\n    something_test.go:1: boom\n")
	}
	diagnostic := extractDiagnosticLines(sb.String())
	if len(diagnostic) > maxDiagnosticLines {
		t.Errorf("expected at most %d lines, got %d", maxDiagnosticLines, len(diagnostic))
	}
}

func TestComputeCPUIdlePercent(t *testing.T) {
	tests := []struct {
		name   string
		load1  float64
		numCPU int
		want   float64
	}{
		{"fully idle host", 0, 8, 100},
		{"half the cores busy", 4, 8, 50},
		{"all cores busy", 8, 8, 0},
		{"oversubscribed clamps to zero", 40, 8, 0},
		{"unreadable core count fails open", 40, 0, 100},
		{"negative load is nonsense, reads as idle", -1, 8, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := computeCPUIdlePercent(tt.load1, tt.numCPU); got != tt.want {
				t.Errorf("computeCPUIdlePercent(%v, %d) = %v, want %v", tt.load1, tt.numCPU, got, tt.want)
			}
		})
	}
}

// TestHostBusyReason pins the skip decision, including the two configurations
// that must NOT skip: an unset floor (the default) and an out-of-range one,
// since a floor above 100 could never be met and would skip every cycle
// forever — the "silently starve the patrol" failure the gate must not have.
func TestHostBusyReason(t *testing.T) {
	busy := hostLoad{IdlePercent: 4.0, Load1: 7.68, NumCPU: 8}
	idle := hostLoad{IdlePercent: 87.5, Load1: 1.0, NumCPU: 8}

	tests := []struct {
		name     string
		minIdle  float64
		host     hostLoad
		wantSkip bool
	}{
		{"saturated host trips the gate", 25, busy, true},
		{"idle host runs", 25, idle, false},
		{"floor disabled by default", 0, busy, false},
		{"floor above 100 cannot starve the patrol", 150, busy, false},
		{"exactly at the floor runs", 25, hostLoad{IdlePercent: 25, Load1: 6, NumCPU: 8}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := hostBusyReason(tt.minIdle, tt.host)
			if tt.wantSkip {
				if reason == "" {
					t.Fatalf("expected host-busy skip for %+v with floor %v", tt.host, tt.minIdle)
				}
				// The skip must carry the numbers that justify it, so a
				// skipped patrol is diagnosable from the log alone.
				for _, want := range []string{"host busy", "CPU idle", "load1", "cores"} {
					if !strings.Contains(reason, want) {
						t.Errorf("skip reason %q missing %q", reason, want)
					}
				}
				return
			}
			if reason != "" {
				t.Errorf("expected no skip for %+v with floor %v, got %q", tt.host, tt.minIdle, reason)
			}
		})
	}
}

// TestHostLoadString pins the shape of the host line the escalation body
// carries, since the mayor reads it to tell a regression from contention.
func TestHostLoadString(t *testing.T) {
	got := (hostLoad{IdlePercent: 4.3, Load1: 7.65, NumCPU: 8}).String()
	want := "CPU idle 4.3% (load1 7.65 on 8 cores)"
	if got != want {
		t.Errorf("hostLoad.String() = %q, want %q", got, want)
	}
}

// TestMinCPUIdlePercentConfigWiring proves the gate is actually configurable
// from patrols.main_branch_test — a knob that parses nowhere is not a knob.
func TestMinCPUIdlePercentConfigWiring(t *testing.T) {
	if got := mainBranchTestMinCPUIdlePercent(nil); got != 0 {
		t.Errorf("expected the gate disabled with nil config, got %v", got)
	}

	var config DaemonPatrolConfig
	raw := `{"type":"daemon-patrol-config","version":1,"patrols":{"main_branch_test":{"enabled":true,"min_cpu_idle_percent":25}}}`
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := mainBranchTestMinCPUIdlePercent(&config); got != 25 {
		t.Errorf("expected configured floor 25, got %v", got)
	}

	// Unconfigured patrol: still disabled, not a zero-value pointer surprise.
	var other DaemonPatrolConfig
	if err := json.Unmarshal([]byte(`{"patrols":{"main_branch_test":{"enabled":true}}}`), &other); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := mainBranchTestMinCPUIdlePercent(&other); got != 0 {
		t.Errorf("expected the gate disabled when unset, got %v", got)
	}
}

// stubHostLoad pins the host-load reading for the duration of t so the
// host-busy decision is testable without saturating the real machine.
func stubHostLoad(t *testing.T, h hostLoad) {
	t.Helper()
	prev := measureHostLoadFn
	measureHostLoadFn = func() hostLoad { return h }
	t.Cleanup(func() { measureHostLoadFn = prev })
}

// TestRunMainBranchTests_SkipsWhenHostBusy is the gt-f57o acceptance case for
// "run it while the box is saturated, the runner reports skipped, not FAILED":
// with the gate configured, a busy host must end the cycle as a labelled skip
// — not a red verdict, and not a cycle that quietly runs anyway.
func TestRunMainBranchTests_SkipsWhenHostBusy(t *testing.T) {
	stubHostLoad(t, hostLoad{IdlePercent: 4.0, Load1: 7.68, NumCPU: 8})

	minIdle := 25.0
	var logged bytes.Buffer
	d := &Daemon{
		logger: log.New(&logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				MainBranchTest: &MainBranchTestConfig{Enabled: true, MinCPUIdlePercent: &minIdle},
			},
		},
	}

	d.runMainBranchTests()

	out := logged.String()
	if !strings.Contains(out, "skipped: host busy") {
		t.Errorf("expected a host-busy skip, got:\n%s", out)
	}
	if strings.Contains(out, "FAILED") {
		t.Errorf("a busy host must not produce a FAILED verdict:\n%s", out)
	}
	if strings.Contains(out, "starting patrol cycle") && strings.Contains(out, "patrol cycle complete") {
		t.Errorf("expected the cycle to stop at the skip, not run to completion:\n%s", out)
	}
}

// TestRunMainBranchTests_RunsWhenHostIdleEnough is the guard's other half: the
// gate must not skip a cycle it has no reason to skip.
func TestRunMainBranchTests_RunsWhenHostIdleEnough(t *testing.T) {
	stubHostLoad(t, hostLoad{IdlePercent: 87.5, Load1: 1.0, NumCPU: 8})

	minIdle := 25.0
	var logged bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()}, // no rigs.json — the cycle finds no rigs and returns
		logger: log.New(&logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				MainBranchTest: &MainBranchTestConfig{Enabled: true, MinCPUIdlePercent: &minIdle},
			},
		},
	}

	d.runMainBranchTests()

	out := logged.String()
	if strings.Contains(out, "skipped: host busy") {
		t.Errorf("an idle host must not be skipped:\n%s", out)
	}
	if !strings.Contains(out, "host is idle enough to run") {
		t.Errorf("expected the gate to record why it ran:\n%s", out)
	}
}

// TestRunMainBranchTests_HostBusyGateDisabledByDefault is the regression test
// for the reviewer's objection to the first attempt at this gate: it must not
// skip by default. With no configured floor, even a saturated host runs the
// cycle, so a config typo or an unset knob can never silently stop the patrol
// that catches regressions in main.
func TestRunMainBranchTests_HostBusyGateDisabledByDefault(t *testing.T) {
	stubHostLoad(t, hostLoad{IdlePercent: 0, Load1: 40, NumCPU: 8})

	var logged bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				MainBranchTest: &MainBranchTestConfig{Enabled: true},
			},
		},
	}

	d.runMainBranchTests()

	if out := logged.String(); strings.Contains(out, "skipped: host busy") {
		t.Errorf("the host-busy gate must be opt-in, but it skipped:\n%s", out)
	}
}

// TestRunMainBranchTests_OutOfRangeFloorIsNotSilent covers the other way this
// gate could starve the patrol: a floor above 100 can never be met, so the
// cycle must both run and say in the log that the configured floor was
// rejected. A misconfiguration that looks like a satisfied minimum would be
// invisible in exactly the situation the gate exists to make legible.
func TestRunMainBranchTests_OutOfRangeFloorIsNotSilent(t *testing.T) {
	stubHostLoad(t, hostLoad{IdlePercent: 0, Load1: 40, NumCPU: 8})

	minIdle := 150.0
	var logged bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				MainBranchTest: &MainBranchTestConfig{Enabled: true, MinCPUIdlePercent: &minIdle},
			},
		},
	}

	d.runMainBranchTests()

	out := logged.String()
	if strings.Contains(out, "skipped: host busy") {
		t.Errorf("an out-of-range floor must not skip every cycle:\n%s", out)
	}
	if !strings.Contains(out, "ignoring out-of-range min_cpu_idle_percent") {
		t.Errorf("expected the rejected floor to be reported, got:\n%s", out)
	}
}

// TestRunCommandOnWorktree_FailureBodyNamesFailingPackage is the fixture test
// called out in gt-1s2g: a run with one failing package (buried among
// passing ones) must produce an escalation body naming that package, plus
// the rig, the commit tested, and the on-disk log path — not a blind tail
// of trailing "ok" lines and not just the bare exit status.
func TestRunCommandOnWorktree_FailureBodyNamesFailingPackage(t *testing.T) {
	townRoot := t.TempDir()
	workDir := t.TempDir()
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(os.Stderr, "", 0),
	}

	cmd := "printf '%s' " + shellQuote(goTestFixtureOneFailingPackage) + "; exit 1"

	err := d.runCommandOnWorktree(context.Background(), "gastown", "deadbeef", workDir, "test", cmd)
	if err == nil {
		t.Fatal("expected error from failing command")
	}

	body := err.Error()
	if !strings.Contains(body, "internal/widget") {
		t.Errorf("expected body to name the failing package, got:\n%s", body)
	}
	if !strings.Contains(body, "rig: gastown") {
		t.Errorf("expected body to name the rig, got:\n%s", body)
	}
	if !strings.Contains(body, "commit: deadbeef") {
		t.Errorf("expected body to name the commit tested, got:\n%s", body)
	}
	if !strings.Contains(body, filepath.Join(townRoot, "logs", "main_branch_test")) {
		t.Errorf("expected body to reference the log path, got:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ok") {
			t.Errorf("expected no passing-package lines in body, got:\n%s", body)
		}
	}
}

// fixtureEchoCommand is the command shape internal/daemon's own tests use to
// exercise the extractor against realistic go test output: the whole fixture
// as a printf argument. It is also the shape that fabricated a failure on
// 2026-09-21 — the runner echoes the command it runs, so the fixture's
// "--- FAIL: ..." and "FAIL\t<pkg>\t<secs>s" lines landed at column 0 of the
// run's log and the extractor reported them as the failing package (gt-f57o,
// second defect).
func fixtureEchoCommand() string {
	return "printf '%s' " + shellQuote(goTestFixtureOneFailingPackage) + "; exit 1"
}

// TestRunCommandOnWorktree_LogCannotFabricateFailures is the regression test
// for the second defect on this bead: the run's own log lines must not look
// like test failures. The mayor was handed 'FAIL
// github.com/steveyegge/gastown/internal/widget' — a package that does not
// exist — because the log line announcing the command echoed a fixture with
// newlines in it, putting go-test-shaped lines at column 0 of the log the
// extractor reads. Before the command was flattened onto one line, the
// extractor found four fabricated lines here.
func TestRunCommandOnWorktree_LogCannotFabricateFailures(t *testing.T) {
	var logged bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logged, "", 0),
	}

	_ = d.runCommandOnWorktree(context.Background(), "gastown", "deadbeef", t.TempDir(), "test", fixtureEchoCommand())

	if fabricated := extractDiagnosticLines(logged.String()); len(fabricated) != 0 {
		t.Errorf("the runner's own log lines must not read as test failures, got:\n%s", strings.Join(fabricated, "\n"))
	}
}

// TestExtractDiagnosticLines_IgnoresIndentedMarkers is the same defect one
// layer down: go test indents a failing test's output — its log lines, its
// failure message — past column 0, so go-test-shaped text there is context,
// not a verdict. Anchoring the pattern at column 0 is what separates the two;
// an indented copy may still appear in the body as a block under a real
// marker (that is how a nested subtest's assertion gets in), but it can never
// stand as a failure on its own.
func TestExtractDiagnosticLines_IgnoresIndentedMarkers(t *testing.T) {
	const fixtureInAMessage = `--- FAIL: TestRunCommandOnWorktree_FailureBodyNamesFailingPackage (0.01s)
    main_branch_test_runner_test.go:344: expected body to name the failing package, got:
        --- FAIL: TestWidgetRenders (0.00s)
        widget_test.go:42: expected 3, got 4
        FAIL
        FAIL	github.com/steveyegge/gastown/internal/widget	0.030s
FAIL
FAIL	github.com/steveyegge/gastown/internal/daemon	901.045s
`
	diagnostic := extractDiagnosticLines(fixtureInAMessage)
	joined := strings.Join(diagnostic, "\n")

	if !strings.Contains(joined, "--- FAIL: TestRunCommandOnWorktree_FailureBodyNamesFailingPackage") {
		t.Errorf("expected the real failing test marker, got:\n%s", joined)
	}
	if !strings.Contains(joined, "FAIL\tgithub.com/steveyegge/gastown/internal/daemon") {
		t.Errorf("expected the real failing package, got:\n%s", joined)
	}

	for _, line := range diagnostic {
		if leadingIndentWidth(line) != 0 {
			continue // block context under a real marker, not a verdict
		}
		if strings.Contains(line, "internal/widget") || strings.Contains(line, "TestWidgetRenders") {
			t.Errorf("a line go test wrote as test output was reported as a failure marker: %q", line)
		}
	}
}

// TestOneLine pins the sanitizer: a log entry is one line, and nothing echoed
// into it may add another.
func TestOneLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single line unchanged", "GOFLAGS=-p=8 make test", "GOFLAGS=-p=8 make test"},
		{"newlines escaped", "printf 'a\n--- FAIL: TestX\nFAIL'", `printf 'a\n--- FAIL: TestX\nFAIL'`},
		{"crlf escaped", "a\r\nb", `a\nb`},
		{"bare carriage return escaped", "a\rb", `a\nb`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := oneLine(tt.in); got != tt.want {
				t.Errorf("oneLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	t.Run("length bounded", func(t *testing.T) {
		got := oneLine(strings.Repeat("x", maxLoggedCommandChars+50))
		if len(got) != maxLoggedCommandChars+3 {
			t.Errorf("expected %d chars plus an ellipsis, got %d", maxLoggedCommandChars, len(got))
		}
	})

	t.Run("multi-byte runes are not split", func(t *testing.T) {
		got := oneLine(strings.Repeat("é", maxLoggedCommandChars+50))
		if !utf8.ValidString(got) {
			t.Errorf("oneLine produced invalid UTF-8: %q", got)
		}
	})
}

// TestRunCommandOnWorktree_LogLineNamesTheTestedHead is the gt-f57o
// requirement that a verdict be matchable to a head: the run's log line must
// carry the commit under test, so three red verdicts can be compared against
// the heads the refinery verified green instead of being indistinguishable
// "gastown: running test" entries.
func TestRunCommandOnWorktree_LogLineNamesTheTestedHead(t *testing.T) {
	var logged bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logged, "", 0),
	}

	if err := d.runCommandOnWorktree(context.Background(), "gastown", "37ab61b2c4d1e5f6", t.TempDir(), "test", "exit 0"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if out := logged.String(); !strings.Contains(out, "running test on 37ab61b2c4d1") {
		t.Errorf("expected the tested head in the log line, got: %s", out)
	}
}

// TestRunCommandOnWorktree_BodyNamesTestAndHostLoad is the gt-f57o acceptance
// case: an escalation must let the mayor triage without re-running the
// package by hand — the failing test and its assertion, the tested sha, and
// the host load the verdict was produced under.
func TestRunCommandOnWorktree_BodyNamesTestAndHostLoad(t *testing.T) {
	stubHostLoad(t, hostLoad{IdlePercent: 3.5, Load1: 7.72, NumCPU: 8})

	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(os.Stderr, "", 0),
	}

	cmd := "printf '%s' " + shellQuote(goTestFixtureOneFailingPackage) + "; exit 1"

	err := d.runCommandOnWorktree(context.Background(), "gastown", "37ab61b2c4d1e5f6a7b8", t.TempDir(), "test", cmd)
	if err == nil {
		t.Fatal("expected error from failing command")
	}

	body := err.Error()
	for _, want := range []string{
		"--- FAIL: TestWidgetRenders",              // the failing test
		"widget_test.go:42: expected 3, got 4",     // what it asserted
		"commit: 37ab61b2c4d1e5f6a7b8",             // the head tested
		"host at start: CPU idle 3.5% (load1 7.72", // contention context
		"host at end: CPU idle 3.5% (load1 7.72",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q, got:\n%s", want, body)
		}
	}

	// The body's log path must name a file that really holds the full output:
	// the worktree the run happened in is deleted afterwards, so a body
	// pointing at a log that was never written leaves the mayor with the same
	// rerun-by-hand problem as before (gt-f57o).
	idx := strings.Index(body, "log: ")
	if idx < 0 {
		t.Fatalf("expected a log path in the body, got:\n%s", body)
	}
	logPath := strings.TrimSpace(body[idx+len("log: "):])
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("body named log %q, which could not be read: %v", logPath, readErr)
	}
	if string(data) != goTestFixtureOneFailingPackage {
		t.Errorf("persisted log does not hold the full combined output")
	}
	if !strings.Contains(filepath.Base(logPath), "37ab61b2c4d1") {
		t.Errorf("expected the tested head in the log filename, got %q", filepath.Base(logPath))
	}
}

// TestRunRigGates_SetupRunsBeforeTest is the regression test for gt-znj8:
// a fresh worktree has no installed dependencies, so a configured
// setup_command must run before the test command, in the same call. It
// writes a marker file so ordering is observed directly rather than
// inferred from a green run (see the adversarial-test-criterion memory:
// absence of a failure doesn't prove the right thing ran first).
func TestRunRigGates_SetupRunsBeforeTest(t *testing.T) {
	townRoot := t.TempDir()
	workDir := t.TempDir()
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(os.Stderr, "", 0),
	}

	marker := filepath.Join(workDir, "setup-ran")
	gateCfg := &rigGateConfig{
		SetupCommand: "touch " + shellQuote(marker),
		TestCommand:  "test -f " + shellQuote(marker),
	}

	if err := d.runRigGates(context.Background(), "gastown", "deadbeef", workDir, gateCfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("expected setup command to have run, marker file missing: %v", err)
	}
}

// TestRunRigGates_SetupFailureReportedAsSetupNotTest is the regression test
// for gt-znj8's "failure = 'setup failed', not 'test failed'" requirement:
// a rig missing an installed dependency (e.g. mango's jest) must escalate
// as a setup failure, not a misleading test failure, and the test command
// must never run.
func TestRunRigGates_SetupFailureReportedAsSetupNotTest(t *testing.T) {
	townRoot := t.TempDir()
	workDir := t.TempDir()
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(os.Stderr, "", 0),
	}

	marker := filepath.Join(workDir, "test-ran")
	gateCfg := &rigGateConfig{
		SetupCommand: "jest-not-installed", // mimics "sh: jest: command not found" (exit 127)
		TestCommand:  "touch " + shellQuote(marker),
	}

	err := d.runRigGates(context.Background(), "gastown", "deadbeef", workDir, gateCfg)
	if err == nil {
		t.Fatal("expected error from failing setup command")
	}
	if !strings.HasPrefix(err.Error(), "setup failed:") {
		t.Errorf("expected error to be reported as a setup failure, got: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("expected test command to be skipped after setup failure, but it ran")
	}
}

// TestRunRigGates_MissingSetupCommandUnchanged is the regression test for
// gt-znj8's "missing setup_command unchanged" requirement: a rig that never
// configures setup_command must behave exactly as before this change.
func TestRunRigGates_MissingSetupCommandUnchanged(t *testing.T) {
	townRoot := t.TempDir()
	workDir := t.TempDir()
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(os.Stderr, "", 0),
	}

	gateCfg := &rigGateConfig{TestCommand: "exit 0"}
	if err := d.runRigGates(context.Background(), "gastown", "deadbeef", workDir, gateCfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// shellQuote wraps s in single quotes for safe use as a literal sh argument,
// escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestContains(t *testing.T) {
	if !sliceContains([]string{"a", "b", "c"}, "b") {
		t.Error("expected true for 'b' in [a b c]")
	}
	if sliceContains([]string{"a", "b", "c"}, "d") {
		t.Error("expected false for 'd' in [a b c]")
	}
	if sliceContains(nil, "a") {
		t.Error("expected false for nil slice")
	}
}

// stubNoContainers overrides package slot's docker-ps lookup for the
// duration of t so these tests never shell out to the real docker CLI (see
// internal/cmd/mq_batch_slot_test.go's stubNoContainers for the same need).
func stubNoContainers(t *testing.T) {
	t.Helper()
	t.Cleanup(slot.SetContainerListerForTest(func() ([]string, error) { return nil, nil }))
}

// TestAcquireMainBranchTestSlot_AcquiresAndReleases is the regression test
// for gt-hpce: testRigMainBranch's gate/test run must be a first-class
// holder of the container-gate slot instead of an invisible occupant that
// collides with other rigs' Docker-backed suites (gt-tuiy/gt-afe4's class of
// bug, but for the daemon's own baseline run rather than a formula-invoked
// test_command).
func TestAcquireMainBranchTestSlot_AcquiresAndReleases(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := acquireMainBranchTestSlot(townRoot, "gastown")
	if err != nil {
		t.Fatalf("acquireMainBranchTestSlot: %v", err)
	}
	if h == nil {
		t.Fatalf("acquireMainBranchTestSlot returned a nil handle")
	}

	rep, err := slot.Status(townRoot)
	if err != nil {
		t.Fatalf("slot.Status while held: %v", err)
	}
	if !rep.Held {
		t.Fatalf("slot.Status reports not held while acquireMainBranchTestSlot's handle is outstanding: %+v", rep)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	rep, err = slot.Status(townRoot)
	if err != nil {
		t.Fatalf("slot.Status after release: %v", err)
	}
	if rep.Held {
		t.Fatalf("slot.Status still reports held after Release: %+v", rep)
	}
}

// TestAcquireMainBranchTestSlot_TakesTheRealHold is the gt-off9 regression
// test: the runner must acquire as a first-class holder — flock, owner file,
// docker-ps check — even when the daemon's own environment carries a marker
// naming this very role, which is what a marker inherited from a
// predecessor daemon process looks like. The kernel drops that process's
// flock when it dies, but the marker lives on in everything it spawned, so
// riding it would let every cycle "acquire" a slot nobody holds and run with
// no flock, no owner file and no docker-ps check: invisible to the refinery
// gates and gt done verifies it is supposed to queue behind.
func TestAcquireMainBranchTestSlot_TakesTheRealHold(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	inherited := slot.SlotLockPath(townRoot, 0) + "|" + strconv.Itoa(os.Getpid()+100000) + "|gastown/main-branch-test"
	t.Setenv(slot.ReentrantEnvVar, inherited)

	h, err := acquireMainBranchTestSlot(townRoot, "gastown")
	if err != nil {
		t.Fatalf("acquireMainBranchTestSlot: %v", err)
	}

	rep, err := slot.Status(townRoot)
	if err != nil {
		t.Fatalf("slot.Status while held: %v", err)
	}
	if !rep.Held {
		t.Fatalf("no flock taken — the daemon's suite is invisible to every other caller: %+v", rep)
	}
	if rep.Owner == nil || rep.Owner.Role != "gastown/main-branch-test" || rep.Owner.PID != os.Getpid() {
		t.Fatalf("owner file should name the daemon's own hold: %+v", rep.Owner)
	}

	// The marker the hold arms for its descendants names this runner's role,
	// overwriting the stale one — which is what keeps it harmless in the
	// unrelated processes the daemon spawns while holding: only a caller
	// doing the same role's work may ride it (gt-off9).
	want := slot.SlotLockPath(townRoot, h.Index) + "|" + strconv.Itoa(os.Getpid()) + "|gastown/main-branch-test"
	if got := os.Getenv(slot.ReentrantEnvVar); got != want {
		t.Fatalf("marker armed by the daemon's hold = %q, want %q", got, want)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if rep, _ = slot.Status(townRoot); rep.Held {
		t.Fatalf("the hold outlived Release: %+v", rep)
	}
}

// TestAcquireMainBranchTestSlot_NeverInvokesRealDockerCLI proves
// stubNoContainers above is actually wired to the machinery
// acquireMainBranchTestSlot uses (see the identical concern documented on
// internal/cmd/mq_batch_slot_test.go's TestAcquireBatchGateSlot_NeverInvokesRealDockerCLI):
// without the stub, a real docker/testcontainers suite already up on a
// shared host would make this poll the real `docker ps` for the full
// mainBranchTestSlotTimeout (60m), hanging the package's test run.
func TestAcquireMainBranchTestSlot_NeverInvokesRealDockerCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	restore := slot.SetContainerListerForTest(func() ([]string, error) {
		return []string{"dolt/dolt-sql-server:2.2.0 someone-elses-suite"}, nil
	})
	defer restore()

	townRoot := t.TempDir()
	timeout := slot.DefaultPollInterval + 500*time.Millisecond
	if _, err := slot.Acquire(townRoot, "gastown/main-branch-test", timeout); err == nil {
		t.Fatalf("Acquire succeeded even though the stubbed lister reported a running container — the real (docker-absent) lister must have been consulted instead of the stub")
	}
}

// TestTriggerMainBranchTests_SingleFlight is the regression test for
// gt-uvxy: an overlapping tick must be skipped rather than stacking a second
// concurrent main_branch_test cycle on top of one that's still running.
func TestTriggerMainBranchTests_SingleFlight(t *testing.T) {
	d := &Daemon{
		logger: log.New(os.Stderr, "", 0),
		// patrolConfig is nil, so main_branch_test is inactive and
		// runMainBranchTests returns immediately without touching rigs —
		// this test exercises only the single-flight guard, not a real cycle.
	}

	if started := d.triggerMainBranchTests(); !started {
		t.Fatal("expected first trigger to start a cycle")
	}

	// Wait for the goroutine launched above to finish and clear the flag,
	// so the "already running" case below tests a real in-flight state
	// rather than racing the first goroutine's cleanup.
	deadline := time.Now().Add(2 * time.Second)
	for d.mainBranchTestRunning.Load() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for first cycle to clear mainBranchTestRunning")
		}
		time.Sleep(time.Millisecond)
	}

	// Simulate a still-running cycle and verify the next tick is skipped,
	// not started concurrently.
	d.mainBranchTestRunning.Store(true)
	if started := d.triggerMainBranchTests(); started {
		t.Error("expected trigger to skip when a cycle is already running")
	}
	if !d.mainBranchTestRunning.Load() {
		t.Error("expected mainBranchTestRunning to remain true after a skipped trigger")
	}
}

func TestDefaultLifecycleConfigIncludesMainBranchTest(t *testing.T) {
	config := DefaultLifecycleConfig()
	if config.Patrols.MainBranchTest == nil {
		t.Fatal("expected MainBranchTest in default lifecycle config")
	}
	if !config.Patrols.MainBranchTest.Enabled {
		t.Error("expected MainBranchTest.Enabled=true")
	}
	if config.Patrols.MainBranchTest.IntervalStr != "60m" {
		t.Errorf("expected interval '60m', got %q", config.Patrols.MainBranchTest.IntervalStr)
	}
	if config.Patrols.MainBranchTest.TimeoutStr != "10m" {
		t.Errorf("expected timeout '10m', got %q", config.Patrols.MainBranchTest.TimeoutStr)
	}
}

func TestEnsureLifecycleDefaultsFillsMainBranchTest(t *testing.T) {
	config := &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &PatrolsConfig{}, // All nil
	}
	changed := EnsureLifecycleDefaults(config)
	if !changed {
		t.Error("expected changed=true when MainBranchTest was nil")
	}
	if config.Patrols.MainBranchTest == nil {
		t.Fatal("expected MainBranchTest to be populated")
	}
	if !config.Patrols.MainBranchTest.Enabled {
		t.Error("expected MainBranchTest.Enabled=true after defaults")
	}
}
