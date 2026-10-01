package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestInstallForRole_ConcurrentPolecatSpawnsProduceValidJSON covers gh#3500
// on the JSON merge path every Claude role takes (SyncManagedClaudeSettings,
// a read-modify-write over the settings.json every polecat in a rig shares;
// gt-8stz, gt-4k3fj.8.3): N concurrent polecat installs leave a valid
// settings.json that carries the PermissionRequest guard, because the atomic
// rename serializes the writes.
func TestInstallForRole_ConcurrentPolecatSpawnsProduceValidJSON(t *testing.T) {
	t.Parallel()
	home := configHome{home: t.TempDir()}

	rigRoot := t.TempDir()
	settingsDir := filepath.Join(rigRoot, "gastown", "polecats")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatalf("mkdir settingsDir: %v", err)
	}
	target := filepath.Join(settingsDir, ".claude", "settings.json")

	const concurrency = 64
	start := make(chan struct{})
	var ready, wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		ready.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			if err := home.installForRole("claude", settingsDir, settingsDir, "polecat", ".claude", "settings.json", "claude", true); err != nil {
				errs <- err
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("InstallForRole: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}

	settings, err := LoadSettings(target)
	if err != nil {
		t.Fatalf("settings.json is not valid JSON after concurrent writes: %v\n--- file contents (%d bytes) ---\n%s", err, len(data), string(data))
	}

	foundGuard := false
	for _, entry := range settings.Hooks.PermissionRequest {
		for _, h := range entry.Hooks {
			if strings.Contains(h.Command, "tap guard permission-request") {
				foundGuard = true
			}
		}
	}
	if !foundGuard {
		t.Fatalf("concurrent polecat install did not carry the PermissionRequest guard, got: %+v", settings.Hooks.PermissionRequest)
	}
}

// TestInstallForRole_AtomicWriteErrorPropagates: when the settings file
// cannot be read or written (here: the target path is a non-empty
// directory), the installer surfaces the error rather than swallowing it
// (gh#3500).
func TestInstallForRole_AtomicWriteErrorPropagates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "occupied"), nil, 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	err := HomeAt(t.TempDir()).InstallForRole("claude", dir, dir, "mayor", ".claude", "settings.json", "claude", true)
	if err == nil {
		t.Fatal("expected error from a directory in place of settings.json, got nil")
	}
	if !strings.Contains(err.Error(), "installing managed claude settings") {
		t.Errorf("expected wrapped 'installing managed claude settings' error, got: %v", err)
	}
}

// TestSyncForRole_AtomicWriteErrorPropagates is the SyncForRole counterpart
// to TestInstallForRole_AtomicWriteErrorPropagates — covers the second
// atomic-write call site introduced in gh#3500.
func TestSyncForRole_AtomicWriteErrorPropagates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The target path is a non-empty directory, so the atomic write's final
	// rename fails.
	target := filepath.Join(dir, ".opencode", "plugins", "gastown.js")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "occupied"), nil, 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	_, err := SyncForRole("opencode", dir, dir, "polecat", ".opencode/plugins", "gastown.js", "opencode", false)
	if err == nil {
		t.Fatal("expected error from read-only directory, got nil")
	}
	if !strings.Contains(err.Error(), "writing hooks file") {
		t.Errorf("expected wrapped 'writing hooks file' error, got: %v", err)
	}
}
