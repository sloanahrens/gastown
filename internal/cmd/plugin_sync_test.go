package cmd

import (
	"bytes"
	"io"
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

// runPluginSyncFor runs gt plugin sync for townRoot from source ("" finds
// the town's checkout) and returns what it printed.
func runPluginSyncFor(t *testing.T, townRoot, source string) string {
	t.Helper()
	var out bytes.Buffer
	r := pluginSyncRun{townRoot: townRoot, source: source, out: &out, errOut: io.Discard}
	if err := r.run(); err != nil {
		t.Errorf("runPluginSync() error = %v", err)
	}
	return out.String()
}

// gt-nc7q: a gastown checkout in the working directory must not supply the
// plugins. A dog ran `gt plugin sync` from its stale clone
// (deacon/dogs/alpha/gastown at a Sep-17 commit) and the clone's copy of the
// compactor-dog default won, overwriting the town's. The up-to-date run is
// the one that matters — a sync from the wrong source is silent there, so
// naming the directory it read is the only way to see it.
func TestRunPluginSync_ReadsTownCheckoutNotWorkingDirectoryClone(t *testing.T) {
	t.Parallel()

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

	out := runPluginSyncFor(t, townRoot, "")

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
	t.Parallel()

	townRoot := t.TempDir()
	markTestTown(t, townRoot)
	writeTestPlugin(t, filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins"), "current-plugin", "current")

	elsewhere := t.TempDir()
	writeTestPlugin(t, elsewhere, "handed-plugin", "handed")

	out := runPluginSyncFor(t, townRoot, elsewhere)

	if !strings.Contains(out, elsewhere) || !strings.Contains(out, "explicit --source") {
		t.Errorf("sync did not report the --source directory as its source; output:\n%s", out)
	}
}

// gt-bw1wo: a dry run that would not remove an extra still names it. Printing
// nothing read as "nothing to see" while the plugin kept dispatching.
func TestRunPluginSync_DryRunNamesExtraWithoutClean(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	markTestTown(t, townRoot)
	sourcePlugins := filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins")
	writeTestPlugin(t, sourcePlugins, "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "seat-refill", "retired")

	var out bytes.Buffer
	r := pluginSyncRun{townRoot: townRoot, dryRun: true, out: &out, errOut: io.Discard}
	if err := r.run(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "seat-refill (in runtime, not in source)") {
		t.Errorf("dry run did not name the extra plugin; output:\n%s", got)
	}
	if strings.Contains(got, "up to date") {
		t.Errorf("dry run claimed up to date with an extra plugin present; output:\n%s", got)
	}
}

// The real sync without --clean leaves the extra in place, so it must say so
// on stderr: "already up to date" over a retired plugin is how seat-refill
// kept dispatching with nothing warning (gt-bw1wo).
func TestRunPluginSync_ReportsExtraItLeaves(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	markTestTown(t, townRoot)
	sourcePlugins := filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins")
	writeTestPlugin(t, sourcePlugins, "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "seat-refill", "retired")

	var out, errOut bytes.Buffer
	r := pluginSyncRun{townRoot: townRoot, out: &out, errOut: &errOut}
	if err := r.run(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(errOut.String(), "seat-refill") {
		t.Errorf("sync left seat-refill in place without naming it; stderr:\n%s", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(townRoot, "plugins", "seat-refill")); err != nil {
		t.Errorf("sync without --clean removed the extra: %v", err)
	}
}

// The same dry run with --clean must say what it would remove, not just name
// the plugin.
func TestRunPluginSync_DryRunCleanSaysWouldRemove(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	markTestTown(t, townRoot)
	sourcePlugins := filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins")
	writeTestPlugin(t, sourcePlugins, "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "current-plugin", "current")
	writeTestPlugin(t, filepath.Join(townRoot, "plugins"), "seat-refill", "retired")

	var out bytes.Buffer
	r := pluginSyncRun{townRoot: townRoot, dryRun: true, clean: true, out: &out, errOut: io.Discard}
	if err := r.run(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(out.String(), "seat-refill (would be removed)") {
		t.Errorf("--clean dry run did not name what it would remove; output:\n%s", out.String())
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
