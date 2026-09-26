package plugin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// gitCommitAll commits everything under dir (initialising a repo on first
// use), so a sync source has history: the guard treats runtime content that
// the repo once held at the same path as an older copy, safe to overwrite.
func gitCommitAll(t *testing.T, dir, msg string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		run("init", "-q")
	}
	run("add", "-A")
	run("commit", "-q", "--allow-empty", "-m", msg)
}

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

	// The runtime copy holds v1, which the source repo once held: an older
	// copy, so the sync may replace it.
	createTestPlugin(t, srcDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nv1 instructions", nil)
	gitCommitAll(t, srcDir, "v1")
	createTestPlugin(t, srcDir, "my-plugin", "+++\nname = \"my-plugin\"\n+++\nv2 instructions", nil)
	gitCommitAll(t, srcDir, "v2")
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

	// old-plugin was retired from the repo: its runtime copy matches what
	// the repo once held, so --clean may remove it.
	createTestPlugin(t, srcDir, "keep-me", "+++\nname = \"keep-me\"\n+++\nkeep", nil)
	createTestPlugin(t, srcDir, "old-plugin", "+++\nname = \"old-plugin\"\n+++\nold", nil)
	gitCommitAll(t, srcDir, "both")
	if err := os.RemoveAll(filepath.Join(srcDir, "old-plugin")); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, srcDir, "retire old-plugin")
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
	if got.Dir != pluginsDir {
		t.Errorf("FindGastownSource().Dir = %q, want %q", got.Dir, pluginsDir)
	}
	if got.Rule != "mayor rig checkout" {
		t.Errorf("FindGastownSource().Rule = %q, want %q", got.Rule, "mayor rig checkout")
	}
}

// gt-nc7q: a gastown checkout in the working directory must not become the
// plugin source. A dog resolved its sync source from CWD, so running from its
// stale clone (deacon/dogs/alpha/gastown at a Sep-17 commit) would have pushed
// that commit's plugin files over the town runtime copy.
func TestFindGastownSource_IgnoresWorkingDirectoryCheckout(t *testing.T) {
	cwdCheckout := t.TempDir()
	writeGastownCheckout(t, cwdCheckout, "stale-plugin")
	t.Chdir(cwdCheckout)

	townRoot := t.TempDir()
	canonical := filepath.Join(townRoot, "gastown", "mayor", "rig", "plugins")
	createTestPlugin(t, canonical, "current-plugin", "+++\nname = \"current-plugin\"\n+++\nbody", nil)

	got, err := FindGastownSource(townRoot)
	if err != nil {
		t.Fatalf("FindGastownSource() error = %v", err)
	}
	if got.Dir != canonical {
		t.Errorf("FindGastownSource().Dir = %q, want canonical %q (never the CWD checkout)", got.Dir, canonical)
	}
}

func TestFindGastownSource_FallsBackToLegacyLayout(t *testing.T) {
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
	if got.Dir != legacyDir {
		t.Errorf("FindGastownSource().Dir = %q, want legacy path %q", got.Dir, legacyDir)
	}
}

func TestFindGastownSource_NoneFoundReturnsError(t *testing.T) {
	townRoot := t.TempDir() // Empty: no gastown checkout anywhere.
	if _, err := FindGastownSource(townRoot); err == nil {
		t.Error("FindGastownSource() error = nil, want error when no source exists")
	}
}

// writeGastownCheckout lays out a directory that FindGastownSource's old
// CWD-walk-up recognized: a gastown go.mod beside a plugins/ tree.
func writeGastownCheckout(t *testing.T, dir, pluginName string) {
	t.Helper()
	goMod := "module github.com/steveyegge/gastown\n\ngo 1.24\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}
	createTestPlugin(t, filepath.Join(dir, "plugins"), pluginName, "+++\nname = \""+pluginName+"\"\n+++\nbody", nil)
}

// gt-o848l: a runtime edit the repo never held (2026-09-18: the mayor's
// CHECK_ONLY safety default in compactor-dog/run.sh) must survive a sync
// instead of being silently replaced by the destructive repo default.
func TestSyncPlugins_ProtectsRuntimeEdit(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	createTestPlugin(t, srcDir, "compactor", "+++\nname = \"compactor\"\n+++\n", map[string]string{"run.sh": "MODE=flatten\n"})
	gitCommitAll(t, srcDir, "repo default")
	createTestPlugin(t, dstDir, "compactor", "+++\nname = \"compactor\"\n+++\n", map[string]string{"run.sh": "MODE=check-only # hand edit\n"})

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 0 {
		t.Errorf("copied %v over a runtime edit", result.Copied)
	}
	if want := map[string][]string{"compactor": {"run.sh"}}; !reflect.DeepEqual(result.Protected, want) {
		t.Errorf("Protected = %v, want %v", result.Protected, want)
	}
	data, _ := os.ReadFile(filepath.Join(dstDir, "compactor", "run.sh"))
	if string(data) != "MODE=check-only # hand edit\n" {
		t.Errorf("runtime edit was overwritten: %q", data)
	}
}

// A file that exists only in the runtime copy (copyDir replaces the whole
// directory) is a runtime edit too.
func TestSyncPlugins_ProtectsRuntimeOnlyFile(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	createTestPlugin(t, srcDir, "p", "+++\nname = \"p\"\n+++\nv2", nil)
	gitCommitAll(t, srcDir, "v2")
	createTestPlugin(t, dstDir, "p", "+++\nname = \"p\"\n+++\nv2", map[string]string{"local.env": "X=1\n"})

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"p": {"local.env"}}; !reflect.DeepEqual(result.Protected, want) {
		t.Errorf("Protected = %v, want %v", result.Protected, want)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "p", "local.env")); err != nil {
		t.Errorf("runtime-only file was deleted: %v", err)
	}
}

func TestSyncPluginsWithOptions_ForceOverwritesRuntimeEdit(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	createTestPlugin(t, srcDir, "p", "+++\nname = \"p\"\n+++\nrepo", nil)
	gitCommitAll(t, srcDir, "repo")
	createTestPlugin(t, dstDir, "p", "+++\nname = \"p\"\n+++\nhand edit", nil)

	result, err := SyncPluginsWithOptions(srcDir, dstDir, SyncOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 1 || len(result.Protected) != 0 {
		t.Errorf("Copied = %v, Protected = %v; want p copied under --force", result.Copied, result.Protected)
	}
}

// Without git history the guard cannot tell an older copy from an edit, so
// it fails closed: drifted plugins are protected, new ones still copy.
func TestSyncPlugins_NoHistoryFailsClosed(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	createTestPlugin(t, srcDir, "drifted", "+++\nname = \"drifted\"\n+++\nv2", nil)
	createTestPlugin(t, srcDir, "fresh", "+++\nname = \"fresh\"\n+++\nnew", nil)
	createTestPlugin(t, dstDir, "drifted", "+++\nname = \"drifted\"\n+++\nv1", nil)

	result, err := SyncPlugins(srcDir, dstDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Protected["drifted"]; !ok {
		t.Errorf("drifted plugin not protected without history: %+v", result)
	}
	if !reflect.DeepEqual(result.Copied, []string{"fresh"}) {
		t.Errorf("Copied = %v, want [fresh]", result.Copied)
	}
}

// --clean must not delete a runtime-only plugin the repo never held.
func TestSyncPlugins_CleanProtectsRuntimeOnlyPlugin(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	createTestPlugin(t, srcDir, "keep-me", "+++\nname = \"keep-me\"\n+++\nkeep", nil)
	gitCommitAll(t, srcDir, "keep")
	createTestPlugin(t, dstDir, "keep-me", "+++\nname = \"keep-me\"\n+++\nkeep", nil)
	createTestPlugin(t, dstDir, "local-only", "+++\nname = \"local-only\"\n+++\nmine", nil)

	result, err := SyncPlugins(srcDir, dstDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 0 {
		t.Errorf("removed %v, a plugin the repo never held", result.Removed)
	}
	if _, ok := result.Protected["local-only"]; !ok {
		t.Errorf("local-only not protected: %+v", result)
	}
}
