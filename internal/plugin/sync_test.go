package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// helper to create a plugin directory with a plugin.md and optional extra files.
func createTestPlugin(t *testing.T, dir, name, content string, extras map[string]string) {
	t.Helper()
	pluginDir := filepath.Join(dir, name)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	for fname, fcontent := range extras {
		if err := os.WriteFile(filepath.Join(pluginDir, fname), []byte(fcontent), 0755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSyncPlugins_CopiesNew(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\ndo stuff", nil)

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 1 || result.Copied[0] != "my-plugin" {
		t.Errorf("expected 1 copied plugin, got %v", result.Copied)
	}

	// Verify file exists at target
	if _, err := os.Stat(filepath.Join(dstDir, "my-plugin", "plugin.md")); err != nil {
		t.Errorf("plugin.md not copied: %v", err)
	}
}

func TestSyncPlugins_SkipsUpToDate(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content := "+++\nname = \"my-plugin\"\n+++\ndo stuff"
	createTestPlugin(t, srcDir, "my-plugin", content, nil)
	createTestPlugin(t, dstDir, "my-plugin", content, nil)

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 {
		t.Errorf("expected 1 skipped plugin, got %v", result.Skipped)
	}
	if len(result.Copied) != 0 {
		t.Errorf("expected 0 copied, got %v", result.Copied)
	}
}

func TestSyncPlugins_UpdatesChanged(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nv2 instructions", nil)
	createTestPlugin(t, dstDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nv1 instructions", nil)

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 1 {
		t.Errorf("expected 1 copied plugin, got %v", result.Copied)
	}

	// Verify target has new content
	data, err := os.ReadFile(filepath.Join(dstDir, "my-plugin", "plugin.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "+++\nname = \"my-plugin\"\n+++\nv2 instructions" {
		t.Errorf("content not updated: %s", data)
	}
}

func TestSyncPlugins_CopiesExtraFiles(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nstuff",
		map[string]string{"run.sh": "#!/bin/bash\necho hi"})

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 1 {
		t.Errorf("expected 1 copied, got %v", result.Copied)
	}

	// Verify run.sh was copied
	data, err := os.ReadFile(filepath.Join(dstDir, "my-plugin", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "#!/bin/bash\necho hi" {
		t.Errorf("run.sh content wrong: %s", data)
	}

	// Verify executable permission preserved (skip on Windows where permission bits aren't meaningful)
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dstDir, "my-plugin", "run.sh"))
		if info.Mode()&0111 == 0 {
			t.Error("run.sh lost executable permission")
		}
	}
}

func TestSyncPlugins_CleanRemovesExtra(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "keep-me", "+++\nname = \"keep-me\"\n+++\nkeep", nil)
	createTestPlugin(t, dstDir, "keep-me", "+++\nname = \"keep-me\"\n+++\nkeep", nil)
	createTestPlugin(t, dstDir, "old-plugin", "+++\nname = \"old-plugin\"\n+++\nold", nil)

	result, err := SyncPlugins(srcDir, dstDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "old-plugin" {
		t.Errorf("expected old-plugin removed, got %v", result.Removed)
	}

	// Verify old plugin was removed
	if _, err := os.Stat(filepath.Join(dstDir, "old-plugin")); !os.IsNotExist(err) {
		t.Error("old-plugin should have been removed")
	}
}

func TestSyncPlugins_NoCleanKeepsExtra(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "new-plugin", "+++\nname = \"new-plugin\"\n+++\nnew", nil)
	createTestPlugin(t, dstDir, "old-plugin", "+++\nname = \"old-plugin\"\n+++\nold", nil)

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 0 {
		t.Errorf("expected 0 removed (clean=false), got %v", result.Removed)
	}

	// Verify old plugin still exists
	if _, err := os.Stat(filepath.Join(dstDir, "old-plugin", "plugin.md")); err != nil {
		t.Error("old-plugin should still exist when clean=false")
	}
}

func TestSyncPlugins_IgnoresNonPluginDirs(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Create a directory without plugin.md — should be ignored
	notPlugin := filepath.Join(srcDir, "not-a-plugin")
	if err := os.MkdirAll(notPlugin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notPlugin, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 0 {
		t.Errorf("expected 0 copied (no valid plugins), got %v", result.Copied)
	}
}

func TestDetectDrift_NoDrift(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content := "+++\nname = \"stable\"\n+++\nstuff"
	createTestPlugin(t, srcDir, "stable", content, nil)
	createTestPlugin(t, dstDir, "stable", content, nil)

	report, err := DetectDrift(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if report.HasDrift() {
		t.Error("expected no drift")
	}
}

func TestDetectDrift_ContentDiffers(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "changed", "+++\nname = \"changed\"\n+++\nv2", nil)
	createTestPlugin(t, dstDir, "changed", "+++\nname = \"changed\"\n+++\nv1", nil)

	report, err := DetectDrift(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasDrift() {
		t.Error("expected drift")
	}
	if len(report.Drifted) != 1 || report.Drifted[0].Name != "changed" {
		t.Errorf("expected changed in drifted, got %v", report.Drifted)
	}
}

func TestDetectDrift_MissingFromTarget(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, srcDir, "new-one", "+++\nname = \"new-one\"\n+++\nnew", nil)

	report, err := DetectDrift(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasDrift() {
		t.Error("expected drift")
	}
	if len(report.Missing) != 1 || report.Missing[0] != "new-one" {
		t.Errorf("expected new-one missing, got %v", report.Missing)
	}
}

func TestDetectDrift_ExtraInTarget(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	createTestPlugin(t, dstDir, "orphan", "+++\nname = \"orphan\"\n+++\nold", nil)

	report, err := DetectDrift(srcDir, dstDir)
	if err != nil {
		t.Fatal(err)
	}
	// Extra plugins are not drift (no HasDrift), but are reported
	if len(report.Extra) != 1 || report.Extra[0] != "orphan" {
		t.Errorf("expected orphan in extra, got %v", report.Extra)
	}
}

// writeRigsJSON writes a minimal mayor/rigs.json with a single rig entry.
func writeRigsJSON(t *testing.T, townRoot, rigName, localRepo string) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"version":1,"rigs":{%q:{"git_url":"https://example.com/%s.git","local_repo":%q,"added_at":"2026-01-01T00:00:00Z"}}}`,
		rigName, rigName, localRepo)
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestRigCheckoutRoot_UsesLocalRepoOverride(t *testing.T) {
	townRoot := t.TempDir()
	override := filepath.Join(t.TempDir(), "elsewhere", "gastown-checkout")
	writeRigsJSON(t, townRoot, "gastown", override)

	got := rigCheckoutRoot(townRoot, "gastown")
	if got != override {
		t.Errorf("rigCheckoutRoot() = %q, want LocalRepo override %q", got, override)
	}
}

func TestRigCheckoutRoot_DefaultsToTownRootRigName(t *testing.T) {
	townRoot := t.TempDir()
	// No rigs.json at all.
	got := rigCheckoutRoot(townRoot, "gastown")
	want := filepath.Join(townRoot, "gastown")
	if got != want {
		t.Errorf("rigCheckoutRoot() = %q, want default %q", got, want)
	}
}

func TestFindGastownSource_LocatesMayorRigPlugins(t *testing.T) {
	// Isolate from this test process's own repo checkout: without this, the
	// CWD-walk-up branch in FindGastownSource would find this repo's own
	// go.mod/plugins/ before ever consulting townRoot.
	t.Chdir(t.TempDir())

	townRoot := t.TempDir()
	pluginsDir := filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins")
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		t.Fatal(err)
	}
	createTestPlugin(t, pluginsDir, "some-plugin", "+++\nname = \"some-plugin\"\n+++\nbody", nil)

	got, err := FindGastownSource(townRoot)
	if err != nil {
		t.Fatalf("FindGastownSource() error = %v", err)
	}
	if got != pluginsDir {
		t.Errorf("FindGastownSource() = %q, want %q", got, pluginsDir)
	}
}

func TestFindGastownSource_FallsBackToLegacyLayout(t *testing.T) {
	t.Chdir(t.TempDir())

	townRoot := t.TempDir()
	// No mayor/rig/plugins — only the legacy crew/den/plugins layout exists.
	legacyDir := filepath.Join(townRoot, "gastown", "crew", "den", "plugins")
	if err := os.MkdirAll(legacyDir, 0755); err != nil {
		t.Fatal(err)
	}
	createTestPlugin(t, legacyDir, "old-plugin", "+++\nname = \"old-plugin\"\n+++\nbody", nil)

	got, err := FindGastownSource(townRoot)
	if err != nil {
		t.Fatalf("FindGastownSource() error = %v", err)
	}
	if got != legacyDir {
		t.Errorf("FindGastownSource() = %q, want legacy path %q", got, legacyDir)
	}
}

func TestFindGastownSource_NoneFoundReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())

	townRoot := t.TempDir() // Empty: no gastown checkout anywhere.
	if _, err := FindGastownSource(townRoot); err == nil {
		t.Error("FindGastownSource() error = nil, want error when no source exists")
	}
}
