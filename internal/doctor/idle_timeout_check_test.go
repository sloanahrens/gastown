package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdleTimeoutCheck_Run(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Create town beads with routes.jsonl
	townBeads := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown"}
{"prefix":"bd-","path":"beads"}
`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create gastown rig with correct idle-timeout
	gastownBeads := filepath.Join(townRoot, "gastown", ".beads")
	if err := os.MkdirAll(gastownBeads, 0755); err != nil {
		t.Fatal(err)
	}
	gastownConfig := `prefix: gt
issue-prefix: gt
dolt.idle-timeout: "0"
`
	if err := os.WriteFile(filepath.Join(gastownBeads, "config.yaml"), []byte(gastownConfig), 0644); err != nil {
		t.Fatal(err)
	}

	// Create beads rig WITHOUT idle-timeout
	beadsBeads := filepath.Join(townRoot, "beads", ".beads")
	if err := os.MkdirAll(beadsBeads, 0755); err != nil {
		t.Fatal(err)
	}
	beadsConfig := `prefix: bd
issue-prefix: bd
`
	if err := os.WriteFile(filepath.Join(beadsBeads, "config.yaml"), []byte(beadsConfig), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := &CheckContext{
		TownRoot: townRoot,
	}

	check := NewIdleTimeoutCheck()
	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning, got %v", result.Status)
	}
	if len(result.Details) != 1 {
		t.Errorf("expected 1 rig missing idle-timeout, got %d: %v", len(result.Details), result.Details)
	}
	if result.Details[0] != "beads" {
		t.Errorf("expected 'beads' in details, got %q", result.Details[0])
	}
}

func TestIdleTimeoutCheck_Run_AllCorrect(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Create town beads with routes.jsonl
	townBeads := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown"}
`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create gastown rig with correct idle-timeout
	gastownBeads := filepath.Join(townRoot, "gastown", ".beads")
	if err := os.MkdirAll(gastownBeads, 0755); err != nil {
		t.Fatal(err)
	}
	gastownConfig := `prefix: gt
issue-prefix: gt
dolt.idle-timeout: "0"
`
	if err := os.WriteFile(filepath.Join(gastownBeads, "config.yaml"), []byte(gastownConfig), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := &CheckContext{
		TownRoot: townRoot,
	}

	check := NewIdleTimeoutCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK, got %v: %s", result.Status, result.Message)
	}
}

func TestIdleTimeoutCheck_Fix(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Create town beads with routes.jsonl
	townBeads := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown"}
`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create gastown rig WITHOUT idle-timeout
	gastownBeads := filepath.Join(townRoot, "gastown", ".beads")
	if err := os.MkdirAll(gastownBeads, 0755); err != nil {
		t.Fatal(err)
	}
	gastownConfig := `prefix: gt
issue-prefix: gt
`
	if err := os.WriteFile(filepath.Join(gastownBeads, "config.yaml"), []byte(gastownConfig), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := &CheckContext{
		TownRoot: townRoot,
	}

	check := NewIdleTimeoutCheck()
	err := check.Fix(ctx)
	if err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify config was updated
	data, err := os.ReadFile(filepath.Join(gastownBeads, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, `dolt.idle-timeout: "0"`) {
		t.Errorf("config.yaml should contain dolt.idle-timeout: \"0\", got:\n%s", content)
	}
}

// TestIdleTimeoutCheck_Fix_PreservesPrefixes pins the fix for the bug where Fix
// called EnsureConfigYAML with an empty prefix, blanking prefix and
// issue-prefix in every rig config.yaml. A rig can carry YAML overrides that
// differ from its Dolt database prefix, and they must survive the fix exactly:
// the only change is the appended dolt.idle-timeout line.
func TestIdleTimeoutCheck_Fix_PreservesPrefixes(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	townBeads := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"xyz-","path":"riga"}
{"prefix":"abc-","path":"rigb"}
`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Each rig's config.yaml overrides the prefix away from its route prefix.
	fixtures := map[string]string{
		"riga": "prefix: xyz-override\nissue-prefix: xyz-override\n" +
			"# keep this comment\nexport.auto: \"false\"\ncustom.key: value\n",
		"rigb": "prefix: abc-override\nissue-prefix: abc-override\n",
	}

	for rig, content := range fixtures {
		rigBeads := filepath.Join(townRoot, rig, ".beads")
		if err := os.MkdirAll(rigBeads, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rigBeads, "config.yaml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	ctx := &CheckContext{TownRoot: townRoot}
	if err := NewIdleTimeoutCheck().Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	for rig, before := range fixtures {
		data, err := os.ReadFile(filepath.Join(townRoot, rig, ".beads", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		// The writer trims the trailing newline, appends the new line, and
		// restores it, so the original bytes are a prefix of the result.
		want := before + "dolt.idle-timeout: \"0\"\n"
		if string(data) != want {
			t.Errorf("%s: config.yaml not preserved byte for byte\n got:\n%s\nwant:\n%s", rig, data, want)
		}
	}
}

// TestIdleTimeoutCheck_Fix_CreatesMissingConfig covers the rig the check
// reports as "(config.yaml missing)". Only the idle-timeout key is written and
// no prefix is guessed, so bd falls back to the Dolt database prefix.
func TestIdleTimeoutCheck_Fix_CreatesMissingConfig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	townBeads := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeads, 0755); err != nil {
		t.Fatal(err)
	}
	routesContent := `{"prefix":"gt-","path":"gastown"}
`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	rigBeads := filepath.Join(townRoot, "gastown", ".beads")
	if err := os.MkdirAll(rigBeads, 0755); err != nil {
		t.Fatal(err)
	}

	ctx := &CheckContext{TownRoot: townRoot}
	if err := NewIdleTimeoutCheck().Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(rigBeads, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "dolt.idle-timeout: \"0\"\n" {
		t.Errorf("missing config.yaml should be created with only the idle-timeout key, got:\n%s", data)
	}
}
