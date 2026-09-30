//go:build integration

package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIntegrationSourceHistory runs the sync guard against a real git repo:
// sourceHistory reads every blob a plugin file has held, relative to a
// sources directory below the repo root, so a runtime copy of an older
// commit's content is replaceable and a hand edit is not.
func TestIntegrationSourceHistory(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	srcDir := filepath.Join(repo, "plugins")
	dstDir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	createTestPlugin(t, srcDir, "p", "+++\nname = \"p\"\n+++\nv1", nil)
	git("add", "-A")
	git("commit", "-q", "-m", "v1")
	createTestPlugin(t, srcDir, "p", "+++\nname = \"p\"\n+++\nv2", nil)
	git("add", "-A")
	git("commit", "-q", "-m", "v2")

	hist, prefix := sourceHistory(srcDir)
	if prefix != "plugins" {
		t.Errorf("prefix = %q, want plugins", prefix)
	}
	if n := len(hist["plugins/p/plugin.md"]); n != 2 {
		t.Errorf("history of plugins/p/plugin.md holds %d blobs, want 2: %v", n, hist)
	}

	// An older copy is replaced; a hand edit is protected.
	createTestPlugin(t, dstDir, "p", "+++\nname = \"p\"\n+++\nv1", nil)
	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 1 {
		t.Errorf("older copy: Copied = %v, Protected = %v; want p copied", result.Copied, result.Protected)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "p", "plugin.md"), []byte("hand edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err = SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Protected["p"]; !ok {
		t.Errorf("hand edit: Protected = %v, want p protected", result.Protected)
	}
}
