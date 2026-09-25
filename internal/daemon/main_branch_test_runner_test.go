package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/slot"
)

// discardLogger is what a test reaches for when it needs a Daemon.logger but
// never inspects what it writes: log.New(os.Stderr, ...) was the shape every
// one of these tests used before gt-tw45, and os.Stderr is the real,
// process-wide stderr fd — the same fd `go test ./...` inherits and streams,
// unbuffered, straight into whatever captured the parent invocation. A test
// in this file that hits that logger runs printf'd go-test-shaped fixture
// text (goTestFixtureOneFailingPackage's "ok .../internal/aaa", "--- FAIL:
// TestWidgetRenders", "FAIL .../internal/widget") through it, so a
// concurrently-running package's real output and this test's fixture text
// landed side by side in one gate log — the incident gt-tw45 reports: a
// refinery gate log's "FAIL .../internal/widget" for a package that has never
// existed in this tree. io.Discard writes touch no fd at all, closing the
// leak at its source rather than trying to keep the one Printf call this bead
// was filed over flattened forever.
var discardLogger = log.New(io.Discard, "", 0)

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
		cfg := loadRigGateConfig("/nonexistent/path")
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
		cfg := loadRigGateConfig(dir)
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
		cfg := loadRigGateConfig(dir)
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
		cfg := loadRigGateConfig(dir)
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
		cfg := loadRigGateConfig(dir)
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
		cfg := loadRigGateConfig(dir)
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
		cfg := loadRigGateConfig(dir)
		if cfg != nil {
			t.Errorf("expected nil for no test commands, got %+v", cfg)
		}
	})

	// TestLoadRigGateConfig/setup_command_from_repo_settings_tier is the
	// regression test for gt-kh4w: loadRigGateConfig used to parse rig-root
	// config.json directly, so a setup_command set only at the
	// repo-committed mayor/rig/.gastown/settings.json tier — which every
	// other gate-command site resolves via rig.ResolveMergeQueueConfig's
	// three-tier precedence — was invisible to this patrol.
	t.Run("setup_command from repo settings tier (gt-kh4w)", func(t *testing.T) {
		dir := t.TempDir()
		// Rig-root config.json has no merge_queue section at all.
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"type":"rig","version":1,"name":"test"}`), 0644); err != nil {
			t.Fatal(err)
		}

		repoSettingsDir := filepath.Join(dir, "mayor", "rig", ".gastown")
		if err := os.MkdirAll(repoSettingsDir, 0755); err != nil {
			t.Fatal(err)
		}
		repoSettings := map[string]interface{}{
			"type":    "rig-settings",
			"version": 1,
			"merge_queue": map[string]interface{}{
				"setup_command": "npm ci",
				"test_command":  "npx jest",
			},
		}
		raw, _ := json.Marshal(repoSettings)
		if err := os.WriteFile(filepath.Join(repoSettingsDir, "settings.json"), raw, 0644); err != nil {
			t.Fatal(err)
		}

		cfg := loadRigGateConfig(dir)
		if cfg == nil {
			t.Fatal("expected non-nil config: setup/test commands set only at the repo-committed settings tier must still resolve")
		}
		if cfg.SetupCommand != "npm ci" {
			t.Errorf("expected setup command 'npm ci' resolved from repo-committed settings, got %q", cfg.SetupCommand)
		}
		if cfg.TestCommand != "npx jest" {
			t.Errorf("expected test command 'npx jest' resolved from repo-committed settings, got %q", cfg.TestCommand)
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
	diagnostic := extractDiagnosticLines(goTestFixtureOneFailingPackage, nil)

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
	diagnostic := extractDiagnosticLines(sb.String(), nil)
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

// TestWriteMainBranchTestLog_SameRigCommitAndSecondNeverCollide is the
// gt-tw45 regression test for the run-id gap: the filename's timestamp
// component only has second granularity, so two runs for the same rig and
// commit — the two calls below land well within the same wall-clock second —
// used to resolve to the same path, and the second os.WriteFile silently
// clobbered the first run's evidence.
func TestWriteMainBranchTestLog_SameRigCommitAndSecondNeverCollide(t *testing.T) {
	townRoot := t.TempDir()

	path1, err := writeMainBranchTestLog(townRoot, "gastown", "deadbeef", "first run\n")
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	path2, err := writeMainBranchTestLog(townRoot, "gastown", "deadbeef", "second run\n")
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if path1 == path2 {
		t.Fatalf("two runs for the same rig and commit were given the same path: %s", path1)
	}

	data1, err := os.ReadFile(path1)
	if err != nil {
		t.Fatalf("reading %s: %v", path1, err)
	}
	if string(data1) != "first run\n" {
		t.Errorf("first run's log was overwritten by the second: got %q", data1)
	}
	data2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatalf("reading %s: %v", path2, err)
	}
	if string(data2) != "second run\n" {
		t.Errorf("second run's log holds the wrong content: got %q", data2)
	}
}

// TestRunCommandOnWorktree_ConcurrentRunsSeeOnlyTheirOwnOutput is the gt-tw45
// acceptance case: two main_branch_test-shaped runs firing at the same
// time — realistic once more than one rig is configured, or a restart
// overlaps a cycle already in flight — must not let either run's escalation
// body or persisted log pick up a line the other run produced. Every
// goroutine below shares one Daemon (one logger, one townRoot) and uses the
// same rig and commit, the worst case for the log filename, so a collision
// in either the in-memory body or the on-disk log shows up here if one
// exists.
func TestRunCommandOnWorktree_ConcurrentRunsSeeOnlyTheirOwnOutput(t *testing.T) {
	townRoot := t.TempDir()
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: discardLogger,
	}

	const n = 8
	logPaths := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("internal/run%d", i)
			output := fmt.Sprintf("--- FAIL: TestRun%d (0.00s)\n    run_test.go:1: boom\nFAIL\nFAIL\tgithub.com/steveyegge/gastown/%s\t0.01s\n", i, marker)
			cmd := "printf '%s' " + shellQuote(output) + "; exit 1"

			err := d.runCommandOnWorktree(context.Background(), "gastown", "deadbeef", t.TempDir(), "test", cmd)
			if err == nil {
				t.Errorf("run %d: expected an error", i)
				return
			}
			body := err.Error()
			idx := strings.Index(body, "log: ")
			if idx < 0 {
				t.Errorf("run %d: expected a log path in the body, got:\n%s", i, body)
				return
			}
			logPaths[i] = strings.TrimSpace(body[idx+len("log: "):])
		}(i)
	}
	wg.Wait()

	seen := map[string]int{}
	for i, path := range logPaths {
		if path == "" {
			continue // this run already failed its own assertions above
		}
		if prev, ok := seen[path]; ok {
			t.Fatalf("run %d and run %d were handed the same log path %s", prev, i, path)
		}
		seen[path] = i

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("run %d: could not read %s: %v", i, path, err)
		}
		mine := fmt.Sprintf("internal/run%d", i)
		if !strings.Contains(string(data), mine) {
			t.Errorf("run %d: %s does not contain its own package %q, got:\n%s", i, path, mine, data)
		}
		for j := 0; j < n; j++ {
			if j == i {
				continue
			}
			other := fmt.Sprintf("internal/run%d", j)
			if strings.Contains(string(data), other) {
				t.Errorf("run %d: %s contains run %d's package %q — output crossed between concurrent runs", i, path, j, other)
			}
		}
	}
}

// TestExtractDiagnosticLines_KeepsAssertionAfterFail is the gt-f57o core
// requirement: the escalation must name the failing TEST and what it
// asserted, not just the package. A "--- FAIL: TestX" marker with no
// assertion line says a test failed but not what broke, and the assertion
// line matches no failure pattern of its own.
func TestExtractDiagnosticLines_KeepsAssertionAfterFail(t *testing.T) {
	diagnostic := extractDiagnosticLines(goTestFixtureOneFailingPackage, nil)
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
	joined := strings.Join(extractDiagnosticLines(fixture, nil), "\n")
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
	diagnostic := extractDiagnosticLines(sb.String(), nil)
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
		logger: discardLogger,
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

// goTestFixtureNestedTranscript is the gt-u4oq shape: a whole synthetic
// transcript at column 0 — the aaa/bbb/widget packages this suite's own
// fixtures use — sitting in the same output as a failure that really happened.
// Nothing about the synthetic half is distinguishable from a real failure by
// its own text: only the module says internal/widget does not exist.
const goTestFixtureNestedTranscript = goTestFixtureOneFailingPackage + `--- FAIL: TestScan_ContextCancelled_MidIteration (470.00s)
    scan_test.go:88: context canceled mid-iteration
FAIL
FAIL	github.com/steveyegge/gastown/internal/daemon	901.045s
`

// writeTree writes a map of slash-separated paths (relative to dir) to content.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
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

	if fabricated := extractDiagnosticLines(logged.String(), nil); len(fabricated) != 0 {
		t.Errorf("the runner's own log lines must not read as test failures, got:\n%s", strings.Join(fabricated, "\n"))
	}
}

// TestRunCommandOnWorktree_ReportsThePackageThatExists is the gt-u4oq
// acceptance case: the run's output carries a synthetic transcript naming a
// package the worktree does not contain beside a failure that happened, and the
// escalation names only the latter. The full output still reaches the on-disk
// log — the scope filters the report, not the record.
func TestRunCommandOnWorktree_ReportsThePackageThatExists(t *testing.T) {
	workDir := t.TempDir()
	writeTree(t, workDir, map[string]string{
		"go.mod":                  "module github.com/steveyegge/gastown\n",
		"internal/daemon/scan.go": "package daemon\n",
	})
	townRoot := t.TempDir()
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: discardLogger,
	}

	cmd := "printf '%s' " + shellQuote(goTestFixtureNestedTranscript) + "; exit 1"
	err := d.runCommandOnWorktree(context.Background(), "gastown", "deadbeef", workDir, "test", cmd)
	if err == nil {
		t.Fatal("expected error from failing command")
	}

	body := err.Error()
	if !strings.Contains(body, "TestScan_ContextCancelled_MidIteration") {
		t.Errorf("expected the failure that happened, got:\n%s", body)
	}
	if !strings.Contains(body, "FAIL\tgithub.com/steveyegge/gastown/internal/daemon") {
		t.Errorf("expected the failing package the worktree contains, got:\n%s", body)
	}
	if strings.Contains(body, "internal/widget") {
		t.Errorf("expected no package the worktree does not contain, got:\n%s", body)
	}

	logPath := filepath.Join(townRoot, "logs", "main_branch_test")
	entries, globErr := filepath.Glob(filepath.Join(logPath, "*"))
	if globErr != nil || len(entries) != 1 {
		t.Fatalf("expected one persisted log under %s, got %v (err %v)", logPath, entries, globErr)
	}
	data, readErr := os.ReadFile(entries[0])
	if readErr != nil {
		t.Fatalf("could not read %s: %v", entries[0], readErr)
	}
	if !strings.Contains(string(data), "internal/widget") {
		t.Errorf("expected the persisted log to hold the full output including the synthetic transcript")
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
	diagnostic := extractDiagnosticLines(fixtureInAMessage, nil)
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

// TestExtractDiagnosticLines_ScopesAttributionToTheWorktree is the gt-u4oq
// requirement at the extractor: the package summary a synthetic transcript
// carries is the only line that says which package failed, so a package the
// module does not contain takes its summary with it and the failure the
// worktree really has is what remains.
func TestExtractDiagnosticLines_ScopesAttributionToTheWorktree(t *testing.T) {
	scope := modulePackages{"github.com/steveyegge/gastown/internal/daemon": {}}
	joined := strings.Join(extractDiagnosticLines(goTestFixtureNestedTranscript, scope), "\n")

	if !strings.Contains(joined, "--- FAIL: TestScan_ContextCancelled_MidIteration") {
		t.Errorf("expected the failing test that really ran, got:\n%s", joined)
	}
	if !strings.Contains(joined, "scan_test.go:88: context canceled mid-iteration") {
		t.Errorf("expected its assertion, got:\n%s", joined)
	}
	if !strings.Contains(joined, "FAIL\tgithub.com/steveyegge/gastown/internal/daemon") {
		t.Errorf("expected the failing package the worktree has, got:\n%s", joined)
	}
	for _, line := range strings.Split(joined, "\n") {
		if strings.Contains(line, "internal/widget") {
			t.Errorf("reported a failure in a package the worktree does not contain: %q", line)
		}
	}
}

// TestExtractDiagnosticLines_KeepsMarkersFromInterleavedOutput is the shape the
// real 2026-09-21 gate capture showed: with the package runs in parallel, the
// package line after a marker can belong to another package's transcript. Both
// of internal/daemon's failing dolt tests sat in exactly that position, so a
// marker is filtered only when the marker itself names a package.
func TestExtractDiagnosticLines_KeepsMarkersFromInterleavedOutput(t *testing.T) {
	const interleaved = "ok  \tgithub.com/steveyegge/gastown/internal/crew\t(cached)\n" +
		"--- FAIL: TestOpenDoltDB_SurvivesQueryLongerThanOldReadTimeout (60.18s)\n" +
		"    dolt_remotes_test.go:210: SELECT SLEEP(45s) failed: context deadline exceeded\n" +
		"--- FAIL: TestPushDatabase_UsesLiveServerConnection (15.00s)\n" +
		"    dolt_remotes_test.go:228: create database: context deadline exceeded\n" +
		"FAIL\n" +
		"FAIL\tgithub.com/steveyegge/gastown/internal/widget\t0.030s\n" +
		"FAIL\n" +
		"FAIL\tgithub.com/steveyegge/gastown/internal/daemon\t354.894s\n"

	scope := modulePackages{"github.com/steveyegge/gastown/internal/daemon": {}}
	joined := strings.Join(extractDiagnosticLines(interleaved, scope), "\n")

	for _, want := range []string{
		"--- FAIL: TestOpenDoltDB_SurvivesQueryLongerThanOldReadTimeout",
		"dolt_remotes_test.go:210: SELECT SLEEP(45s) failed",
		"--- FAIL: TestPushDatabase_UsesLiveServerConnection",
		"FAIL\tgithub.com/steveyegge/gastown/internal/daemon",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in the body, got:\n%s", want, joined)
		}
	}
	for _, line := range strings.Split(joined, "\n") {
		if strings.Contains(line, "internal/widget") {
			t.Errorf("reported a failure in a package the worktree does not contain: %q", line)
		}
	}
}

// TestExtractDiagnosticLines_KeepsEvidenceItCannotAttribute pins the two ways
// output survives scoping with no package to check: a line attributed to no
// package at all, and a worktree that could not be listed (nil scope). Dropping
// either trades a false attribution for no evidence.
func TestExtractDiagnosticLines_KeepsEvidenceItCannotAttribute(t *testing.T) {
	scope := modulePackages{"example.com/other/pkg": {}}

	t.Run("no package line follows a marker", func(t *testing.T) {
		const orphanedPanic = "panic: runtime error: index out of range [3] with length 3\n"
		if got := extractDiagnosticLines(orphanedPanic, scope); len(got) != 1 {
			t.Errorf("expected the panic to survive any scope, got %v", got)
		}
	})

	t.Run("a package outside the scope is dropped", func(t *testing.T) {
		const foreign = "FAIL\tgithub.com/steveyegge/gastown/internal/widget\t0.030s\n"
		if got := extractDiagnosticLines(foreign, scope); len(got) != 0 {
			t.Errorf("expected a package the scope does not list to be dropped, got %v", got)
		}
	})

	t.Run("an unscopeable worktree reports everything", func(t *testing.T) {
		joined := strings.Join(extractDiagnosticLines(goTestFixtureOneFailingPackage, nil), "\n")
		if !strings.Contains(joined, "internal/widget") {
			t.Errorf("expected the transcript to be reported when no scope was found, got:\n%s", joined)
		}
	})
}

// TestLoadModulePackages pins what counts as a package of a worktree: go's own
// exclusions from "./...", the vendor and dependency trees, and — the part
// scoping turns on — a nested module's packages being addressed by the nested
// module rather than the one containing it.
func TestLoadModulePackages(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":                        "module example.com/m\n",
		"main.go":                       "package main\n",
		"internal/real/real.go":         "package real\n",
		"internal/testonly/x_test.go":   "package testonly\n",
		"testdata/broken/broken.go":     "package broken\n",
		".hidden/hidden.go":             "package hidden\n",
		"_scratch/scratch.go":           "package scratch\n",
		"vendor/example.com/dep/dep.go": "package dep\n",
		"node_modules/junk/junk.go":     "package junk\n",
		"nested/go.mod":                 "module example.com/nested\n",
		"nested/nested.go":              "package nested\n",
		"nested/pkg/pkg.go":             "package pkg\n",
	})

	got := loadModulePackages(root)
	want := []string{
		"example.com/m",
		"example.com/m/internal/real",
		"example.com/m/internal/testonly",
		"example.com/nested",
		"example.com/nested/pkg",
	}
	if len(got) != len(want) {
		t.Errorf("expected %d packages, got %d: %v", len(want), len(got), got)
	}
	for _, pkg := range want {
		if !got.contains(pkg) {
			t.Errorf("expected %q in the package set, got %v", pkg, got)
		}
	}

	t.Run("no module is no scope", func(t *testing.T) {
		if got := loadModulePackages(t.TempDir()); got != nil {
			t.Errorf("expected nil for a tree with no go.mod, got %v", got)
		}
	})
}

// TestModulePathIn covers the go.mod spellings the module declaration may take,
// including the trailing comment go allows and the quoting a path may need.
func TestModulePathIn(t *testing.T) {
	tests := []struct{ name, goMod, want string }{
		{"plain", "module example.com/m\n", "example.com/m"},
		{"trailing comment", "module example.com/m // primary module\n", "example.com/m"},
		{"quoted", "module \"example.com/m\"\n", "example.com/m"},
		{"no module line", "go 1.22\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{"go.mod": tt.goMod})
			got, err := modulePathIn(dir)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("modulePathIn = %q, want %q", got, tt.want)
			}
		})
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
		logger: discardLogger,
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

// killedSuiteTranscript is the shape the 2026-09-11 main_branch_test runs left
// behind: package summaries for the packages that had finished, in the order
// and with the timings from the 09:01Z log, and no failure of any kind. A run
// killed at its deadline cannot look like anything else (gt-59yz).
const killedSuiteTranscript = "ok  \tgithub.com/steveyegge/gastown/internal/daemon\t463.9s\n" +
	"ok  \tgithub.com/steveyegge/gastown/internal/tmux\t141.5s\n"

// timedOutCommand reports the killed-suite transcript and then keeps running,
// so only the deadline can end it. The exec replaces the shell with the sleep:
// the context's kill then reaches the process holding the output pipes, and the
// run returns at the deadline instead of waiting the sleep out.
func timedOutCommand() string {
	return "printf '%s' " + shellQuote(killedSuiteTranscript) + "; exec sleep 30"
}

// TestRunCommandOnWorktree_TimeoutIsReportedAsTimeout is the gt-59yz
// acceptance case. The suite outran its budget, exec.CommandContext killed it,
// and the escalation the mayor reads must say that: before this, the body's
// first line — the verdict — was "test failed: signal: killed", which is
// character-for-character what a crash reports, while the transcript under it
// was green and the run had simply needed longer. The kill stays in the body,
// named as the deadline's doing, so the underlying error is not lost to the
// rewording.
//
// The signal that kill used is not pinned: SetProcessGroup escalates SIGTERM to
// SIGKILL, so the cause is "terminated" unless the group had to be forced, and
// the verdict is a timeout because the context's deadline expired, not because
// of which signal the group-kill reached for (gt-6t43).
func TestRunCommandOnWorktree_TimeoutIsReportedAsTimeout(t *testing.T) {
	stubHostLoad(t, hostLoad{IdlePercent: 3.5, Load1: 7.72, NumCPU: 8})
	workDir := t.TempDir()
	writeTree(t, workDir, map[string]string{
		"go.mod":                  "module github.com/steveyegge/gastown\n",
		"internal/daemon/scan.go": "package daemon\n",
		"internal/tmux/scan.go":   "package tmux\n",
	})
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: discardLogger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := d.runCommandOnWorktree(ctx, "gastown", "deadbeef", workDir, "test", timedOutCommand())
	if err == nil {
		t.Fatal("expected error from a command killed at its deadline")
	}

	body := err.Error()
	firstLine, _, _ := strings.Cut(body, "\n")
	if !strings.HasPrefix(firstLine, "test TIMED OUT after ") {
		t.Errorf("expected the verdict line to lead with the timeout, got: %q", firstLine)
	}
	if strings.Contains(firstLine, "failed:") {
		t.Errorf("expected no failure wording on a deadline kill, got: %q", firstLine)
	}
	for _, want := range []string{
		"of the run's budget",                       // not presented as a plain failure
		"still running when its deadline fired",     // that it was alive, not crashed
		"killed by: signal: ",                       // the raw cause, preserved
		"this is the runner's timeout, not a crash", // and classified
		"run budget: patrols.main_branch_test.timeout", // so the fix (raise it) is visible
		"last package reported: ok  \t" + "github.com/steveyegge/gastown/internal/tmux",
		"the killed run's transcript is green", // the all-green tail, labeled
		"commit: deadbeef",
		"host at start: CPU idle 3.5%", // the contention context survives the refactor
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q, got:\n%s", want, body)
		}
	}

	// The budget is the clock this command was given, measured at its start.
	// Asserted as a range rather than an exact string: it is derived from a
	// real deadline minus however long the run took to reach this command, so
	// pinning "2s" would fail on a box slow enough to lose half a second
	// between the context and the first instruction.
	if got := parseBudgetSeconds(t, body); got < time.Second || got > 2*time.Second {
		t.Errorf("expected the reported budget to be within [1s, 2s] of the 2s timeout, got %v:\n%s", got, body)
	}
}

// parseBudgetSeconds pulls the budget out of a timeout body, so the assertions
// about it can be tolerant of the sub-second time between the context being
// created and the command starting.
func parseBudgetSeconds(t *testing.T, body string) time.Duration {
	t.Helper()
	const marker = "TIMED OUT after "
	_, after, ok := strings.Cut(body, marker)
	if !ok {
		t.Fatalf("no %q in body:\n%s", marker, body)
	}
	fields := strings.Fields(after)
	if len(fields) == 0 {
		t.Fatalf("nothing after %q in body:\n%s", marker, body)
	}
	d, err := time.ParseDuration(fields[0])
	if err != nil {
		t.Fatalf("budget %q in body is not a duration: %v", fields[0], err)
	}
	return d
}

// TestRunCommandOnWorktree_RealFailureIsNotCalledATimeout guards the other
// direction of the gt-59yz fix: a command that fails on its own under an
// unexpired deadline is still a failure. Only the context's own error
// separates the two, and treating every killed command as a timeout would
// hide the crashes this runner exists to report.
func TestRunCommandOnWorktree_RealFailureIsNotCalledATimeout(t *testing.T) {
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: discardLogger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := d.runCommandOnWorktree(ctx, "gastown", "deadbeef", t.TempDir(), "test", "exit 3")
	if err == nil {
		t.Fatal("expected error from a failing command")
	}
	body := err.Error()
	if strings.Contains(body, "TIMED OUT") {
		t.Errorf("a command that exited on its own is not a timeout, got:\n%s", body)
	}
	if !strings.HasPrefix(body, "test failed: exit status 3") {
		t.Errorf("expected the exit status to lead the body, got:\n%s", body)
	}
}

// TestRunCommandOnWorktree_CancelledIsNotAVerdict is the shutdown case: the
// daemon cancels d.ctx on the way down, which kills an in-flight gate command
// exactly as a deadline does. Reporting that as "failed: signal: killed" is the
// same false red the timeout fix removes, so it is surfaced as an interruption
// the cycle does not count as either a pass or a failure (gt-59yz).
func TestRunCommandOnWorktree_CancelledIsNotAVerdict(t *testing.T) {
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: discardLogger,
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	defer cancel()
	err := d.runCommandOnWorktree(ctx, "gastown", "deadbeef", t.TempDir(), "test", "exec sleep 30")
	if err == nil {
		t.Fatal("expected error from a command killed by its canceled context")
	}
	if !errors.Is(err, errMainBranchTestInterrupted) {
		t.Errorf("expected the interruption sentinel so the cycle does not count it, got: %v", err)
	}
	body := err.Error()
	if strings.Contains(body, "failed: signal: ") {
		t.Errorf("a canceled run must not read as a crash, got:\n%s", body)
	}
	if !strings.Contains(body, "was stopped, not failed") || !strings.Contains(body, "says nothing about main") {
		t.Errorf("expected the cancellation to be named as the cause, got:\n%s", body)
	}
}

// TestRunGatesOnWorktree_InterruptionSurvivesTheJoin is the propagation half:
// gates report into one joined message string, which would drop the sentinel and
// turn a stopped run back into an ordinary failure — the cycle would then
// escalate a red main because the daemon restarted (gt-59yz).
func TestRunGatesOnWorktree_InterruptionSurvivesTheJoin(t *testing.T) {
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: discardLogger,
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	defer cancel()

	err := d.runGatesOnWorktree(ctx, "gastown", "deadbeef", t.TempDir(), map[string]string{
		"build-check": "exec sleep 30",
	})
	if err == nil {
		t.Fatal("expected error from a canceled gate")
	}
	if !errors.Is(err, errMainBranchTestInterrupted) {
		t.Errorf("expected the interruption sentinel to survive the join, got: %v", err)
	}
}

// TestRunCommandOnWorktree_ExpiredBudgetNeverRan covers the third way a command
// ends without a verdict: the run's context was already spent when this command
// started (earlier gates ate a shared budget), so os/exec's Start hands back
// ctx.Err() with nothing run and nothing printed. Calling that "still running
// when its deadline fired" would describe a command that never executed, and
// the empty output would draw the all-green reassurance on top of it.
func TestRunCommandOnWorktree_ExpiredBudgetNeverRan(t *testing.T) {
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: discardLogger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()

	marker := filepath.Join(t.TempDir(), "ran")
	err := d.runCommandOnWorktree(ctx, "gastown", "deadbeef", t.TempDir(), "build-check", "touch "+shellQuote(marker))
	if err == nil {
		t.Fatal("expected error from a command whose context was already expired")
	}
	body := err.Error()
	if !strings.HasPrefix(body, "build-check did not run:") {
		t.Errorf("expected the body to say nothing ran, got:\n%s", body)
	}
	for _, unwanted := range []string{"still running", "transcript is green", "not a crash"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("a command that never started must not contain %q, got:\n%s", unwanted, body)
		}
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("expected the command not to have run, marker exists: %v", statErr)
	}
}

// TestAnalyzeRunFailure_GreenReassuranceNeedsNoMatchesAtAll is the gt-u4oq
// interaction: a transcript whose only FAIL lines name packages this worktree
// does not contain is dropped by extractDiagnosticLines, and the resulting
// empty diagnostic must not license the sentence that says the transcript is
// green. It was not green — the matches were filtered, not absent.
func TestAnalyzeRunFailure_GreenReassuranceNeedsNoMatchesAtAll(t *testing.T) {
	workDir := t.TempDir()
	writeTree(t, workDir, map[string]string{
		"go.mod":                  "module github.com/steveyegge/gastown\n",
		"internal/daemon/scan.go": "package daemon\n",
	})

	output := killedSuiteTranscript + goTestFixtureNestedTranscript
	body, _ := analyzeRunFailure(runFailure{
		label:   "test",
		output:  output,
		err:     errors.New("signal: killed"),
		ctxErr:  context.DeadlineExceeded,
		budget:  time.Minute,
		rigName: "gastown",
		workDir: workDir,
	})

	if strings.Contains(body, "transcript is green") {
		t.Errorf("filtered-out FAIL lines must not license the green reassurance, got:\n%s", body)
	}
}

// TestLastPackageReported covers the progress marker a deadline kill leaves:
// with no failure to extract, where the transcript stops is the only evidence
// the run was working rather than dead (gt-59yz).
func TestLastPackageReported(t *testing.T) {
	gastown := modulePackages{
		"github.com/steveyegge/gastown/internal/daemon": {},
		"github.com/steveyegge/gastown/internal/tmux":   {},
	}

	tests := []struct {
		name   string
		output string
		scope  modulePackages
		want   string
	}{
		{"no summaries", "go: downloading deps\n", nil, ""},
		{"last of several", killedSuiteTranscript, nil, "ok  \tgithub.com/steveyegge/gastown/internal/tmux\t141.5s"},
		// A package with no test files was never run, so it is not progress:
		// go prints it while walking the list, and it would otherwise overwrite
		// the last package that actually finished.
		{"no-test-files package is not progress", killedSuiteTranscript + "?   \tgithub.com/steveyegge/gastown/internal/slot\t[no test files]\n", nil, "ok  \tgithub.com/steveyegge/gastown/internal/tmux\t141.5s"},
		{"scope drops a package this tree lacks", killedSuiteTranscript + "ok  \tgithub.com/steveyegge/gastown/internal/widget\t9.9s\n", gastown, "ok  \tgithub.com/steveyegge/gastown/internal/tmux\t141.5s"},
		// an indented copy is a test's own output about a summary, not one
		{"indented copy is not the run's progress", "    ok  \tgithub.com/steveyegge/gastown/internal/daemon\t1s\n", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastPackageReported(tc.output, tc.scope); got != tc.want {
				t.Errorf("lastPackageReported(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
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
		logger: discardLogger,
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
		logger: discardLogger,
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
		logger: discardLogger,
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
		logger: discardLogger,
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

// --- gt-lf2r: main_branch_test yields to a busy merge gate ---

// poolHeldBy builds the held/owner picture a status report shows when the named
// roles hold slots 0..n-1, in order — the shape slot.StatusPoolLocksOnly
// returns, and the "fake slot state" the skip decision is driven with here.
func poolHeldBy(roles ...string) slot.Report {
	rep := slot.Report{Total: len(roles)}
	for i, role := range roles {
		rep.Slots = append(rep.Slots, slot.SlotState{
			Index: i,
			Held:  true,
			Owner: &slot.Owner{Role: role, PID: 4242, Slot: i},
		})
	}
	rep.HeldCount = len(roles)
	rep.Held = len(roles) > 0
	if len(roles) > 0 {
		rep.Owner = rep.Slots[0].Owner
	}
	return rep
}

// stubGatePool pins the container-gate pool's held/owner picture for the
// duration of t so the skip decision is driven by a named pool state instead of
// racing a real refinery into a real flock — the same need stubHostLoad serves
// for the host-busy gate (gt-lf2r).
func stubGatePool(t *testing.T, rep slot.Report) {
	t.Helper()
	prev := mainBranchTestGatePoolStatusFn
	mainBranchTestGatePoolStatusFn = func(string) (slot.Report, error) { return rep, nil }
	t.Cleanup(func() { mainBranchTestGatePoolStatusFn = prev })
}

// stubGatePoolError makes the pool unreadable, for the fail-open path.
func stubGatePoolError(t *testing.T, err error) {
	t.Helper()
	prev := mainBranchTestGatePoolStatusFn
	mainBranchTestGatePoolStatusFn = func(string) (slot.Report, error) { return slot.Report{}, err }
	t.Cleanup(func() { mainBranchTestGatePoolStatusFn = prev })
}

// writeTestTownRig lays out the minimum a cycle needs to reach a rig: the rig
// listed in mayor/rigs.json and, when withGateConfig is set, a config.json whose
// merge_queue gives the rig something to run. The rig has no .repo.git, so a
// cycle that gets past the skip decision fails at setup — which is what makes
// "was this rig skipped?" observable from the error alone.
func writeTestTownRig(t *testing.T, townRoot, rigName string, withGateConfig bool) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	rigs, err := json.Marshal(map[string]interface{}{
		"rigs": map[string]interface{}{rigName: map[string]interface{}{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), rigs, 0644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	if !withGateConfig {
		return
	}
	cfg, err := json.Marshal(map[string]interface{}{
		"type": "rig", "version": 1, "name": rigName,
		"merge_queue": map[string]interface{}{"test_command": "go test ./..."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), cfg, 0644); err != nil {
		t.Fatal(err)
	}
}

// newMainBranchTestDaemon builds the daemon a cycle test drives: a town root it
// owns, a logger it can read back, and main_branch_test enabled.
func newMainBranchTestDaemon(townRoot string, logged *bytes.Buffer, cfg *MainBranchTestConfig) *Daemon {
	return &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(logged, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{MainBranchTest: cfg},
		},
	}
}

// TestIsRefineryGateRole pins the suffix match that decides the skip, including
// the near-misses: Contains("/refinery") would match a role that merely mentions
// one, and treating this runner's own role as a busy gate would make every cycle
// skip itself.
func TestIsRefineryGateRole(t *testing.T) {
	busy := []string{
		"gastown/refinery",
		"gastown/refinery-batch",
		"otherrig/refinery",
	}
	for _, role := range busy {
		if !isRefineryGateRole(role) {
			t.Errorf("%q holds a merge gate and must count as busy", role)
		}
	}

	notBusy := []string{
		"gastown/main-branch-test",
		"gastown/my-refinery-watcher",
		"gastown/refinery-helper",
		"gastown/jasper",
		"gastown/refiner",
		"",
	}
	for _, role := range notBusy {
		if isRefineryGateRole(role) {
			t.Errorf("%q is not a merge gate and must not count as busy", role)
		}
	}
}

// TestRefineryGateHolder drives the decision over the pool states it has to tell
// apart: a free pool runs, a refinery hold skips and names the holder, and a
// hold by anyone else (a polecat, this runner's own role, a slot with no
// readable owner) runs.
func TestRefineryGateHolder(t *testing.T) {
	cases := []struct {
		name string
		rep  slot.Report
		want string
	}{
		{"free pool", slot.Report{Total: 4}, ""},
		{"refinery holds slot 0", poolHeldBy("gastown/refinery"), "gastown/refinery"},
		{"batch gate holds slot 0", poolHeldBy("gastown/refinery-batch"), "gastown/refinery-batch"},
		{"another rig's refinery", poolHeldBy("otherrig/refinery"), "otherrig/refinery"},
		{"polecat holds a slot", poolHeldBy("gastown/jasper"), ""},
		{"our own role holds a slot", poolHeldBy("gastown/main-branch-test"), ""},
		{"held slot with no owner file", slot.Report{
			Held: true, HeldCount: 1, Total: 4,
			Slots: []slot.SlotState{{Index: 0, Held: true}},
		}, ""},
		{"refinery behind a polecat's slot", poolHeldBy("gastown/jasper", "gastown/refinery"), "gastown/refinery"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refineryGateHolder(tc.rep); got != tc.want {
				t.Errorf("refineryGateHolder = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMainBranchTestSkipWhenGateBusyDefault is the "default true" half of the
// knob: it has to be on for an unconfigured town, and off only when a config
// says so explicitly. A gate that yields by default is the opposite polarity of
// min_cpu_idle_percent, so the default is its own documented decision and needs
// its own test.
func TestMainBranchTestSkipWhenGateBusyDefault(t *testing.T) {
	if !mainBranchTestSkipWhenGateBusy(nil) {
		t.Error("nil config must default to skipping for a busy gate")
	}
	if !mainBranchTestSkipWhenGateBusy(&DaemonPatrolConfig{}) {
		t.Error("a config with no patrols must default to skipping for a busy gate")
	}
	unset := &DaemonPatrolConfig{Patrols: &PatrolsConfig{MainBranchTest: &MainBranchTestConfig{Enabled: true}}}
	if !mainBranchTestSkipWhenGateBusy(unset) {
		t.Error("an unset skip_when_gate_busy must default to true")
	}

	no := false
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{MainBranchTest: &MainBranchTestConfig{SkipWhenGateBusy: &no}}}
	if mainBranchTestSkipWhenGateBusy(off) {
		t.Error("skip_when_gate_busy=false must stop the yield")
	}
}

// TestMainBranchTestGateBusyStarveAfter covers the bound's three readings: the
// default when unset, a configured duration, and a value that cannot be met —
// which disables the bound rather than silently substituting the default, so a
// typo reads as "no bound" instead of as agreement.
func TestMainBranchTestGateBusyStarveAfter(t *testing.T) {
	if got := mainBranchTestGateBusyStarveAfter(nil); got != defaultGateBusyStarveAfter {
		t.Errorf("nil config: got %v, want the default %v", got, defaultGateBusyStarveAfter)
	}
	if got := mainBranchTestGateBusyStarveAfter(&DaemonPatrolConfig{}); got != defaultGateBusyStarveAfter {
		t.Errorf("empty config: got %v, want the default %v", got, defaultGateBusyStarveAfter)
	}

	cfg := func(s string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			MainBranchTest: &MainBranchTestConfig{GateBusyStarveAfterStr: s},
		}}
	}
	if got := mainBranchTestGateBusyStarveAfter(cfg("2h")); got != 2*time.Hour {
		t.Errorf("configured bound: got %v, want 2h", got)
	}
	for _, s := range []string{"0", "-1h", "not-a-duration"} {
		if got := mainBranchTestGateBusyStarveAfter(cfg(s)); got != 0 {
			t.Errorf("bound %q: got %v, want 0 (disabled)", s, got)
		}
	}
}

// TestNoteGateBusySkip_MeasuresTheUnbrokenRun is the arithmetic the starvation
// bound is measured on: the clock starts at the first skip of a run, keeps
// running across later skips, and restarts only once the rig has been reached
// again. Without the restart, one busy hour would leave the rig permanently
// "starved" and the alert would fire on a single later skip.
func TestNoteGateBusySkip_MeasuresTheUnbrokenRun(t *testing.T) {
	d := &Daemon{}
	start := time.Date(2026, 9, 23, 6, 0, 0, 0, time.UTC)

	if got := d.noteGateBusySkip("gastown", start); got != 0 {
		t.Errorf("first skip of a run must start the clock, got %v", got)
	}
	if got := d.noteGateBusySkip("gastown", start.Add(7*time.Hour)); got != 7*time.Hour {
		t.Errorf("second skip must report the run, got %v", got)
	}
	// A different rig keeps its own clock.
	if got := d.noteGateBusySkip("otherrig", start.Add(1*time.Hour)); got != 0 {
		t.Errorf("another rig's first skip must start its own clock, got %v", got)
	}
	if !d.clearGateBusySkip("gastown") {
		t.Fatal("clearGateBusySkip must report the run it ended")
	}
	if d.clearGateBusySkip("gastown") {
		t.Error("clearing a rig with no run must report that there was nothing to clear")
	}
	if got := d.noteGateBusySkip("gastown", start.Add(14*time.Hour)); got != 0 {
		t.Errorf("a reached rig must start a fresh run, got %v", got)
	}
}

// TestTestRigMainBranch_SkipsBeforeSetupWhenRefineryHoldsSlot is the acceptance
// case from gt-lf2r: a pool whose slot 0 is held by gastown/refinery must make
// the rig skip — naming that holder — before the fetch and worktree-add, not
// after waiting 60m for a slot the merge gate needs.
func TestTestRigMainBranch_SkipsBeforeSetupWhenRefineryHoldsSlot(t *testing.T) {
	townRoot := t.TempDir()
	writeTestTownRig(t, townRoot, "gastown", true)
	stubGatePool(t, poolHeldBy("gastown/refinery"))

	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})

	err := d.testRigMainBranch("gastown", filepath.Join(townRoot, "gastown"), time.Minute)

	if !errors.Is(err, errMainBranchTestGateBusy) {
		t.Fatalf("want a gate-busy skip, got %v", err)
	}
	if got := err.Error(); got != "skipped: gate busy: gastown/refinery" {
		t.Errorf("skip must name the holder, got %q", got)
	}
	// The rig has no .repo.git, so reaching setup would have produced "bare repo
	// not found" instead. Asserting on the error above already proves the return
	// came earlier; this pins it to the fetch, which is the step that must not
	// run.
	if out := logged.String(); strings.Contains(out, "git fetch") || strings.Contains(out, "bare repo not found") {
		t.Errorf("the rig was skipped after setup had started:\n%s", out)
	}
}

// TestTestRigMainBranch_FreePoolIsNotSkipped is the guard's other half: with
// nothing holding the pool the rig must be tested normally. The rig has no bare
// repo, so "tested normally" surfaces as a setup failure — any error other than
// the gate-busy sentinel is the proof that the skip decision let it through.
func TestTestRigMainBranch_FreePoolIsNotSkipped(t *testing.T) {
	townRoot := t.TempDir()
	writeTestTownRig(t, townRoot, "gastown", true)
	stubGatePool(t, poolHeldBy())

	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})

	err := d.testRigMainBranch("gastown", filepath.Join(townRoot, "gastown"), time.Minute)

	if errors.Is(err, errMainBranchTestGateBusy) {
		t.Fatalf("a free pool must not skip the rig: %v", err)
	}
	if err == nil {
		t.Fatal("expected the bare-repo setup to fail on a town with no .repo.git")
	}
	if !strings.Contains(err.Error(), "bare repo not found") {
		t.Errorf("expected the rig to reach setup, got %v", err)
	}
}

// TestTestRigMainBranch_GateBusyCheckCanBeDisabled is the knob's own test: with
// skip_when_gate_busy=false a refinery hold must not stop the rig, which is what
// lets a town that would rather compete for the slot opt out.
func TestTestRigMainBranch_GateBusyCheckCanBeDisabled(t *testing.T) {
	townRoot := t.TempDir()
	writeTestTownRig(t, townRoot, "gastown", true)
	stubGatePool(t, poolHeldBy("gastown/refinery"))

	no := false
	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true, SkipWhenGateBusy: &no})

	err := d.testRigMainBranch("gastown", filepath.Join(townRoot, "gastown"), time.Minute)

	if errors.Is(err, errMainBranchTestGateBusy) {
		t.Fatalf("skip_when_gate_busy=false must not skip: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "bare repo not found") {
		t.Errorf("expected the disabled gate to run the rig to setup, got %v", err)
	}
}

// TestTestRigMainBranch_UnreadablePoolDoesNotSkip is the fail-open case: a pool
// status this daemon cannot read is not evidence of a merge gate, and treating
// it as one would let a broken lock directory stop the patrol that catches
// regressions in main.
func TestTestRigMainBranch_UnreadablePoolDoesNotSkip(t *testing.T) {
	townRoot := t.TempDir()
	writeTestTownRig(t, townRoot, "gastown", true)
	stubGatePoolError(t, errors.New("lock dir unreadable"))

	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})

	err := d.testRigMainBranch("gastown", filepath.Join(townRoot, "gastown"), time.Minute)

	if errors.Is(err, errMainBranchTestGateBusy) {
		t.Fatalf("an unreadable pool must not skip the rig: %v", err)
	}
	if out := logged.String(); !strings.Contains(out, "could not read container-gate pool") {
		t.Errorf("expected the unreadable pool to be logged rather than swallowed:\n%s", out)
	}
}

// TestRunMainBranchTests_AllSkippedCycleCountsSkips is the summary half of
// gt-lf2r: a cycle that skipped every rig must say so. Reporting "1 tested, 0
// failed" over a rig nothing looked at is the reading that clears the failure
// alert on an unverified main.
func TestRunMainBranchTests_AllSkippedCycleCountsSkips(t *testing.T) {
	townRoot := t.TempDir()
	writeTestTownRig(t, townRoot, "gastown", true)
	stubGatePool(t, poolHeldBy("gastown/refinery"))

	var logged, escalated bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{Enabled: true})
	captureMainBranchTestEscalations(t, &escalated)

	d.runMainBranchTests()

	out := logged.String()
	if !strings.Contains(out, "skipped: gate busy: gastown/refinery") {
		t.Errorf("expected the skip to name the holder:\n%s", out)
	}
	if !strings.Contains(out, "(0 tested, 0 failed, 1 skipped)") {
		t.Errorf("expected the summary to count the skip separately from the tests:\n%s", out)
	}
	if strings.Contains(out, "FAILED") {
		t.Errorf("a skipped rig is not a failure:\n%s", out)
	}
	// tested == 0 means the cycle must not clear the failure alert: nothing was
	// verified. clearAlerts shells out to gt and only logs when it fails, so a
	// clearAlerts line here is exactly the over-clear this guards.
	if strings.Contains(out, "clearAlerts(") {
		t.Errorf("an all-skipped cycle must not clear the failure alert:\n%s", out)
	}
	// The skip is not yet a starvation: the bound is measured in hours and this
	// is the first skip of the run.
	if escalated.Len() > 0 {
		t.Errorf("a single skip must not escalate a starvation:\n%s", escalated.String())
	}
}

// TestRunMainBranchTests_CountsSkipsAcrossRigs pins the tally over more than
// one rig: the summary has to count every skip, not just the first. The rig
// order a cycle walks is map order out of mayor/rigs.json, so a per-rig
// stub would be flaky — the pool state is therefore shared, and both rigs
// skipping is the case both orders produce.
func TestRunMainBranchTests_CountsSkipsAcrossRigs(t *testing.T) {
	townRoot := t.TempDir()
	writeTestTownRig(t, townRoot, "gastown", true)
	addTestTownRig(t, townRoot, "otherrig", true)
	stubGatePool(t, poolHeldBy("gastown/refinery"))

	var logged bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{
		Enabled: true,
		// The bound is off so the tally is the only thing this test reports on.
		GateBusyStarveAfterStr: "0",
	})
	captureMainBranchTestEscalations(t, &bytes.Buffer{})

	d.runMainBranchTests()

	if out := logged.String(); !strings.Contains(out, "(0 tested, 0 failed, 2 skipped)") {
		t.Errorf("expected both rigs counted as skipped:\n%s", out)
	}
}

// TestRunMainBranchTests_StarvedRigEscalates is the alarming branch of the
// starvation bound: a rig whose skips have run unbroken past the configured
// bound must be escalated by name, so a yielding patrol that has stopped
// testing a rig is reported rather than inferred from a missing cycle.
//
// It takes two cycles, and that is the point rather than a test artifact: a
// single skip has waited out nothing, so the clock the bound is measured on has
// not moved yet. The bound is 1ns so the second cycle crosses it without the
// test waiting out the default 6h.
func TestRunMainBranchTests_StarvedRigEscalates(t *testing.T) {
	townRoot := t.TempDir()
	writeTestTownRig(t, townRoot, "gastown", true)
	stubGatePool(t, poolHeldBy("gastown/refinery"))

	var logged, escalated bytes.Buffer
	d := newMainBranchTestDaemon(townRoot, &logged, &MainBranchTestConfig{
		Enabled:                true,
		GateBusyStarveAfterStr: "1ns",
	})
	captureMainBranchTestEscalations(t, &escalated)

	d.runMainBranchTests()
	if escalated.Len() > 0 {
		t.Fatalf("the first skip cannot have waited out a bound, but it escalated:\n%s", escalated.String())
	}

	d.runMainBranchTests()

	out := logged.String()
	if !strings.Contains(out, "STARVED") {
		t.Errorf("expected the starvation to be logged loudly:\n%s", out)
	}
	body := escalated.String()
	if !strings.Contains(body, mainBranchTestGateBusyAlertKey("gastown")) {
		t.Errorf("expected an escalation under this rig's own alert key, got:\n%s", body)
	}
	if !strings.Contains(body, "gastown") {
		t.Errorf("expected the escalation to name the rig, got:\n%s", body)
	}
	// The rig still must not have been tested: starving a merge gate is the
	// outcome the yield exists to prevent, so the bound buys visibility, not a
	// forced run.
	if !strings.Contains(out, "(0 tested, 0 failed, 1 skipped)") {
		t.Errorf("a starved rig is still skipped, not tested:\n%s", out)
	}
}

// captureMainBranchTestEscalations redirects the starvation escalation into buf
// for the duration of the calling test. Seamed for the same reason
// maintenanceEscalateFn is: the escalation is the only signal that a yielding
// patrol has stopped testing a rig, so it needs a test that drives it — and one
// that asserts the escalation does not fire, on the cycles that must not.
func captureMainBranchTestEscalations(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	prev := mainBranchTestEscalateFn
	mainBranchTestEscalateFn = func(_ *Daemon, key, source, message string) {
		fmt.Fprintf(buf, "key=%s source=%s\n%s\n", key, source, message)
	}
	t.Cleanup(func() { mainBranchTestEscalateFn = prev })
}

// addTestTownRig adds a second rig to a town laid out by writeTestTownRig.
func addTestTownRig(t *testing.T, townRoot, rigName string, withGateConfig bool) {
	t.Helper()
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	data, err := os.ReadFile(rigsPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Rigs map[string]interface{} `json:"rigs"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	parsed.Rigs[rigName] = map[string]interface{}{}
	out, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigsPath, out, 0644); err != nil {
		t.Fatal(err)
	}

	rigDir := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigDir, 0755); err != nil {
		t.Fatal(err)
	}
	if !withGateConfig {
		return
	}
	cfg, err := json.Marshal(map[string]interface{}{
		"type": "rig", "version": 1, "name": rigName,
		"merge_queue": map[string]interface{}{"test_command": "go test ./..."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), cfg, 0644); err != nil {
		t.Fatal(err)
	}
}
