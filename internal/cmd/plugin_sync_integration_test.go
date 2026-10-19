//go:build integration

package cmd

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitInTest runs git in dir with a fixed identity and no signing.
func gitInTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// The same dry run with --clean must say what it would remove, not just name
// the plugin. The source once shipped seat-refill, so the real run removes the
// runtime copy; without that history it would protect it as a runtime edit.
func TestIntegrationRunPluginSync_DryRunCleanSaysWouldRemove(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	markTestTown(t, townRoot)
	sourcePlugins := filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins")
	writeTestPlugin(t, sourcePlugins, "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "seat-refill", "retired")
	writeTestPlugin(t, sourcePlugins, "seat-refill", "retired")
	gitInTest(t, sourcePlugins, "init", "-q")
	gitInTest(t, sourcePlugins, "add", "-A")
	gitInTest(t, sourcePlugins, "commit", "-q", "-m", "ship seat-refill")
	if err := os.RemoveAll(filepath.Join(sourcePlugins, "seat-refill")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	r := pluginSyncRun{townRoot: townRoot, dryRun: true, clean: true, out: &out, errOut: io.Discard}
	if err := r.run(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(out.String(), "seat-refill (would be removed)") {
		t.Errorf("--clean dry run did not name what it would remove; output:\n%s", out.String())
	}
}
