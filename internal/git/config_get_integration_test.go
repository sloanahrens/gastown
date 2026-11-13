//go:build integration

package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIntegrationConfigGetReadsRealGit: against a real repository, a missing key
// reads as empty, and a config file git cannot parse is an error, not an unset key.
func TestIntegrationConfigGetReadsRealGit(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	g := (&Git{workDir: dir}).
		WithEnv([]string{"HOME=" + t.TempDir()}).
		WithoutEnv("XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM")
	config := filepath.Join(dir, ".git", "config")

	if got, err := g.ConfigGet("core.hooksPath"); err != nil || got != "" {
		t.Fatalf("missing key = %q, %v; want empty and no error", got, err)
	}

	if err := os.WriteFile(config, []byte("[core\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.ConfigGet("core.hooksPath"); err == nil {
		t.Fatal("unreadable config: want an error, got an unset key")
	}
}
