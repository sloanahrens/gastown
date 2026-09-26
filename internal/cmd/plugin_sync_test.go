package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestPlugin creates <dir>/<name>/plugin.md.
func writeTestPlugin(t *testing.T, dir, name, body string) {
	t.Helper()
	pluginDir := filepath.Join(dir, name)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pluginDir, "plugin.md"), "+++\nname = \""+name+"\"\n+++\n"+body+"\n")
}

// markTestTown makes dir resolvable as a town root by workspace.Find.
func markTestTown(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "mayor", "town.json"), `{"name":"test-town"}`)
}

// resetPluginSyncFlags restores the sync subcommand's package-level flags.
func resetPluginSyncFlags(t *testing.T) {
	t.Helper()
	prevSource, prevClean, prevDryRun, prevForce := pluginSyncSource, pluginSyncClean, pluginSyncDryRun, pluginSyncForce
	t.Cleanup(func() {
		pluginSyncSource, pluginSyncClean, pluginSyncDryRun, pluginSyncForce = prevSource, prevClean, prevDryRun, prevForce
	})
	pluginSyncSource, pluginSyncClean, pluginSyncDryRun, pluginSyncForce = "", false, false, false
}

// gt-nc7q: a gastown checkout in the working directory must not supply the
// plugins. A dog ran `gt plugin sync` from its stale clone
// (deacon/dogs/alpha/gastown at a Sep-17 commit) and the clone's copy of the
// compactor-dog default won, overwriting the town's. The up-to-date run is
// the one that matters — a sync from the wrong source is silent there, so
// naming the directory it read is the only way to see it.
func TestRunPluginSync_ReadsTownCheckoutNotWorkingDirectoryClone(t *testing.T) {
	resetPluginSyncFlags(t)

	townRoot := t.TempDir()
	markTestTown(t, townRoot)
	writeTestPlugin(t, filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins"), "current-plugin", "current")
	// Runtime copy already matches the town's checkout: nothing to copy, and
	// the run must still report which source it read.
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "current-plugin", "current")

	// The stale clone, inside the town as a dog's is.
	clone := filepath.Join(townRoot, "deacon", "dogs", "alpha", "gastown")
	if err := os.MkdirAll(clone, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(clone, "go.mod"), "module github.com/steveyegge/gastown\n")
	writeTestPlugin(t, filepath.Join(clone, "plugins"), "stale-plugin", "stale")

	// Keep the fallback resolution on the fixture town if the cwd walk ever
	// stops finding it, rather than on the operator's live town.
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Chdir(clone)

	out := captureStdout(t, func() {
		if err := runPluginSync(nil, nil); err != nil {
			t.Errorf("runPluginSync() error = %v", err)
		}
	})

	canonical := filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins")
	if !strings.Contains(out, canonical) || !strings.Contains(out, "mayor rig checkout") {
		t.Errorf("sync did not report the town's checkout as its source; output:\n%s", out)
	}
	if strings.Contains(out, clone) {
		t.Errorf("sync reported the working directory's clone as its source; output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(townRoot, "plugins", "stale-plugin")); err == nil {
		t.Error("the cwd clone's plugin was copied into the runtime directory")
	}
}

// The explicit path stays available and is labelled as such, so a run against
// a directory outside the town is never mistaken for the town's own checkout.
func TestRunPluginSync_SourceFlagIsLabelledExplicit(t *testing.T) {
	resetPluginSyncFlags(t)

	townRoot := t.TempDir()
	markTestTown(t, townRoot)
	writeTestPlugin(t, filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins"), "current-plugin", "current")

	elsewhere := t.TempDir()
	writeTestPlugin(t, elsewhere, "handed-plugin", "handed")
	pluginSyncSource = elsewhere

	t.Chdir(townRoot)

	out := captureStdout(t, func() {
		if err := runPluginSync(nil, nil); err != nil {
			t.Errorf("runPluginSync() error = %v", err)
		}
	})

	if !strings.Contains(out, elsewhere) || !strings.Contains(out, "explicit --source") {
		t.Errorf("sync did not report the --source directory as its source; output:\n%s", out)
	}
}
