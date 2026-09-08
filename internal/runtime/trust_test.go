package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func claudeRC() *config.RuntimeConfig {
	return &config.RuntimeConfig{Command: "claude"}
}

func readTrustConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return cfg
}

func trustAccepted(t *testing.T, cfg map[string]any, dir string) bool {
	t.Helper()
	projects, ok := cfg["projects"].(map[string]any)
	if !ok {
		return false
	}
	entry, ok := projects[dir].(map[string]any)
	if !ok {
		return false
	}
	accepted, _ := entry["hasTrustDialogAccepted"].(bool)
	return accepted
}

func TestEnsureWorkspaceTrust_CreatesMissingConfig(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()

	if err := EnsureWorkspaceTrust(workDir, configDir, claudeRC()); err != nil {
		t.Fatalf("EnsureWorkspaceTrust: %v", err)
	}

	cfg := readTrustConfig(t, filepath.Join(configDir, ".claude.json"))
	if !trustAccepted(t, cfg, workDir) {
		t.Errorf("expected trust entry for %s in %+v", workDir, cfg)
	}
}

func TestEnsureWorkspaceTrust_PreservesExistingConfig(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()
	path := filepath.Join(configDir, ".claude.json")

	existing := `{
  "numStartups": 42,
  "hasCompletedOnboarding": true,
  "projects": {
    "/some/other/dir": {"hasTrustDialogAccepted": true, "allowedTools": ["Bash"]}
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureWorkspaceTrust(workDir, configDir, claudeRC()); err != nil {
		t.Fatalf("EnsureWorkspaceTrust: %v", err)
	}

	cfg := readTrustConfig(t, path)
	if !trustAccepted(t, cfg, workDir) {
		t.Errorf("expected trust entry for %s", workDir)
	}
	if !trustAccepted(t, cfg, "/some/other/dir") {
		t.Errorf("existing project trust entry was lost")
	}
	if n, ok := cfg["numStartups"].(float64); !ok || n != 42 {
		t.Errorf("numStartups not preserved: %v", cfg["numStartups"])
	}
	if v, ok := cfg["hasCompletedOnboarding"].(bool); !ok || !v {
		t.Errorf("hasCompletedOnboarding not preserved: %v", cfg["hasCompletedOnboarding"])
	}
	projects := cfg["projects"].(map[string]any)
	other := projects["/some/other/dir"].(map[string]any)
	if tools, ok := other["allowedTools"].([]any); !ok || len(tools) != 1 {
		t.Errorf("allowedTools not preserved on existing entry: %v", other["allowedTools"])
	}
}

func TestEnsureWorkspaceTrust_PreservesExistingProjectFields(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()
	path := filepath.Join(configDir, ".claude.json")

	existing := `{"projects": {"` + workDir + `": {"lastCost": 1.5}}}`
	if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureWorkspaceTrust(workDir, configDir, claudeRC()); err != nil {
		t.Fatalf("EnsureWorkspaceTrust: %v", err)
	}

	cfg := readTrustConfig(t, path)
	if !trustAccepted(t, cfg, workDir) {
		t.Errorf("expected trust entry for %s", workDir)
	}
	entry := cfg["projects"].(map[string]any)[workDir].(map[string]any)
	if cost, ok := entry["lastCost"].(float64); !ok || cost != 1.5 {
		t.Errorf("existing project fields not preserved: %v", entry)
	}
}

func TestEnsureWorkspaceTrust_NoRewriteWhenAlreadyTrusted(t *testing.T) {
	configDir := t.TempDir()
	workDir := configDir // real dir with no symlink alias in the temp tree
	path := filepath.Join(configDir, ".claude.json")

	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	existing := map[string]any{
		"projects": map[string]any{
			workDir:  map[string]any{"hasTrustDialogAccepted": true},
			resolved: map[string]any{"hasTrustDialogAccepted": true},
		},
	}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := EnsureWorkspaceTrust(workDir, configDir, claudeRC()); err != nil {
		t.Fatalf("EnsureWorkspaceTrust: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Errorf("config was rewritten despite trust already present")
	}
}

func TestEnsureWorkspaceTrust_SeedsSymlinkResolvedPath(t *testing.T) {
	configDir := t.TempDir()
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := EnsureWorkspaceTrust(linkDir, configDir, claudeRC()); err != nil {
		t.Fatalf("EnsureWorkspaceTrust: %v", err)
	}

	cfg := readTrustConfig(t, filepath.Join(configDir, ".claude.json"))
	if !trustAccepted(t, cfg, linkDir) {
		t.Errorf("expected trust entry for literal path %s", linkDir)
	}
	resolved, err := filepath.EvalSymlinks(linkDir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != linkDir && !trustAccepted(t, cfg, resolved) {
		t.Errorf("expected trust entry for resolved path %s", resolved)
	}
}

func TestEnsureWorkspaceTrust_NonClaudeRuntimeIsNoop(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()

	for _, rc := range []*config.RuntimeConfig{
		nil,
		{Command: "gemini"},
		{Command: "/usr/local/bin/codex"},
		{Command: ""},
	} {
		if err := EnsureWorkspaceTrust(workDir, configDir, rc); err != nil {
			t.Fatalf("EnsureWorkspaceTrust(%+v): %v", rc, err)
		}
	}

	if _, err := os.Stat(filepath.Join(configDir, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf(".claude.json should not be created for non-claude runtimes")
	}
}

func TestEnsureWorkspaceTrust_ClaudePathVariants(t *testing.T) {
	for _, cmd := range []string{
		"claude",
		"/Users/x/.claude/local/claude",
		"claude.exe",
	} {
		if !isClaudeRuntime(&config.RuntimeConfig{Command: cmd}) {
			t.Errorf("isClaudeRuntime(%q) = false, want true", cmd)
		}
	}
	if isClaudeRuntime(&config.RuntimeConfig{Command: "claudette"}) {
		t.Errorf("isClaudeRuntime(claudette) = true, want false")
	}
}

func TestEnsureWorkspaceTrust_CorruptConfigErrors(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()
	path := filepath.Join(configDir, ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureWorkspaceTrust(workDir, configDir, claudeRC()); err == nil {
		t.Fatal("expected error for corrupt config, got nil")
	}

	// The corrupt file must be left untouched, not clobbered.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{not json" {
		t.Errorf("corrupt config was modified: %q", data)
	}
}

func TestEnsureWorkspaceTrust_EmptyWorkDirIsNoop(t *testing.T) {
	configDir := t.TempDir()
	if err := EnsureWorkspaceTrust("", configDir, claudeRC()); err != nil {
		t.Fatalf("EnsureWorkspaceTrust: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configDir, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf(".claude.json should not be created for empty workDir")
	}
}

func TestEnsureWorkspaceTrust_PreservesFileMode(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()
	path := filepath.Join(configDir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{}`), 0640); err != nil {
		t.Fatal(err)
	}

	if err := EnsureWorkspaceTrust(workDir, configDir, claudeRC()); err != nil {
		t.Fatalf("EnsureWorkspaceTrust: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Errorf("file mode = %o, want 0640", info.Mode().Perm())
	}
}

func TestSeedWorkspaceTrust_SeedsNeverTrustedPath(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()

	SeedWorkspaceTrust(workDir, configDir, claudeRC())

	cfg := readTrustConfig(t, filepath.Join(configDir, ".claude.json"))
	if !trustAccepted(t, cfg, workDir) {
		t.Errorf("expected trust entry for %s in %+v", workDir, cfg)
	}
}

func TestSeedWorkspaceTrust_SwallowsErrors(t *testing.T) {
	configDir := t.TempDir()
	workDir := t.TempDir()
	path := filepath.Join(configDir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0600); err != nil {
		t.Fatal(err)
	}

	// Must warn, not panic or overwrite: spawn paths call this best-effort.
	SeedWorkspaceTrust(workDir, configDir, claudeRC())

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{not json` {
		t.Errorf("corrupt config was rewritten: %q", data)
	}
}
