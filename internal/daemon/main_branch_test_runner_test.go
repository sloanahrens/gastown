package daemon

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	logPath, err := writeMainBranchTestLog(townRoot, "gastown", goTestFixtureOneFailingPackage)
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

func TestDefaultLifecycleConfigIncludesMainBranchTest(t *testing.T) {
	config := DefaultLifecycleConfig()
	if config.Patrols.MainBranchTest == nil {
		t.Fatal("expected MainBranchTest in default lifecycle config")
	}
	if !config.Patrols.MainBranchTest.Enabled {
		t.Error("expected MainBranchTest.Enabled=true")
	}
	if config.Patrols.MainBranchTest.IntervalStr != "30m" {
		t.Errorf("expected interval '30m', got %q", config.Patrols.MainBranchTest.IntervalStr)
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
