package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestComputeCPUIdlePercent(t *testing.T) {
	cases := []struct {
		name     string
		load1    float64
		numCPU   int
		wantIdle float64
	}{
		{"fully idle", 0, 4, 100},
		{"fully saturated", 4, 4, 0},
		{"half loaded", 2, 4, 50},
		{"over-saturated clamps to 0", 8, 4, 0},
		{"negative load clamps to 100", -1, 4, 100},
		{"zero cores never blocks", 5, 0, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := computeCPUIdlePercent(c.load1, c.numCPU); got != c.wantIdle {
				t.Errorf("computeCPUIdlePercent(%v, %v) = %v, want %v", c.load1, c.numCPU, got, c.wantIdle)
			}
		})
	}
}

func TestFilterTestFailureOutput(t *testing.T) {
	t.Run("extracts FAIL lines and their assertion line, dropping ok noise", func(t *testing.T) {
		output := strings.Join([]string{
			"ok  	github.com/foo/bar	0.010s",
			"=== RUN   TestBaz",
			"--- FAIL: TestBaz (0.00s)",
			"    baz_test.go:42: expected 1, got 2",
			"ok  	github.com/foo/quux	0.020s",
			"FAIL",
			"FAIL	github.com/foo/bar	0.123s",
		}, "\n")

		got := filterTestFailureOutput(output)

		if strings.Contains(got, "0.010s") || strings.Contains(got, "0.020s") {
			t.Errorf("expected passing 'ok' lines to be filtered out, got:\n%s", got)
		}
		if !strings.Contains(got, "--- FAIL: TestBaz") {
			t.Errorf("expected --- FAIL line preserved, got:\n%s", got)
		}
		if !strings.Contains(got, "baz_test.go:42: expected 1, got 2") {
			t.Errorf("expected assertion line following --- FAIL preserved, got:\n%s", got)
		}
		if !strings.Contains(got, "FAIL\nFAIL\tgithub.com/foo/bar\t0.123s") {
			t.Errorf("expected FAIL summary lines preserved, got:\n%s", got)
		}
	})

	t.Run("falls back to full output when no FAIL markers found", func(t *testing.T) {
		output := "make: *** [build] Error 2\nsome compiler error"
		got := filterTestFailureOutput(output)
		if got != output {
			t.Errorf("expected fallback to unfiltered output, got:\n%s", got)
		}
	})

	t.Run("caps filtered output at maxEscalationOutputLines", func(t *testing.T) {
		var lines []string
		for i := 0; i < maxEscalationOutputLines+50; i++ {
			lines = append(lines, "--- FAIL: TestN")
		}
		got := filterTestFailureOutput(strings.Join(lines, "\n"))
		gotLines := strings.Split(got, "\n")
		if len(gotLines) != maxEscalationOutputLines {
			t.Errorf("expected %d lines after cap, got %d", maxEscalationOutputLines, len(gotLines))
		}
	})
}

func TestPersistMainBranchTestLog(t *testing.T) {
	townRoot := t.TempDir()
	output := "full test output\nline two\nline three"

	logPath, err := persistMainBranchTestLog(townRoot, "gastown", "abc123", output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := filepath.Join(townRoot, "logs", "main_branch_test", "gastown-abc123.log")
	if logPath != wantPath {
		t.Errorf("expected log path %q, got %q", wantPath, logPath)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}
	if string(data) != output {
		t.Errorf("expected full output persisted, got %q", string(data))
	}
}

func TestPersistMainBranchTestLogUnknownSha(t *testing.T) {
	townRoot := t.TempDir()
	logPath, err := persistMainBranchTestLog(townRoot, "gastown", "", "output")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantPath := filepath.Join(townRoot, "logs", "main_branch_test", "gastown-unknown.log")
	if logPath != wantPath {
		t.Errorf("expected log path %q, got %q", wantPath, logPath)
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
