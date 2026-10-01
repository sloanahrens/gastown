package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/hooks"
)

func claudeRuntime() *config.RuntimeConfig {
	useSettingsDir := true
	return &config.RuntimeConfig{
		Command: "claude",
		Hooks: &config.RuntimeHooksConfig{
			Provider:       "claude",
			Dir:            ".claude",
			SettingsFile:   "settings.json",
			UseSettingsDir: &useSettingsDir,
		},
	}
}

// A session start writes the managed settings into the role's settings dir
// and reports hooks:present for the file the session loads (gt-4k3fj.8.3).
func TestSyncSessionSettings_Present(t *testing.T) {
	t.Parallel()
	home := hooks.HomeAt(t.TempDir())
	settingsDir := filepath.Join(t.TempDir(), "gastown", "crew")
	workDir := t.TempDir()

	s, err := syncSessionSettings(home, settingsDir, workDir, "crew", claudeRuntime())
	if err != nil {
		t.Fatalf("syncSessionSettings: %v", err)
	}
	want := filepath.Join(settingsDir, ".claude", "settings.json")
	if !s.Present || s.Path != want || s.EventType() != EventHooksPresent {
		t.Fatalf("status = %+v, want present at %s", s, want)
	}
	if err := home.CheckManagedClaudeSettings(hooks.Target{Path: want, Key: "gastown/crew"}); err != nil {
		t.Errorf("settings file is not the managed set for gastown/crew: %v", err)
	}
}

// A start replaces stale hooks: agents get new guards without an operator
// running gt hooks sync.
func TestSyncSessionSettings_ReplacesStaleHooks(t *testing.T) {
	t.Parallel()
	home := hooks.HomeAt(t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hooks":{"PreToolUse":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := syncSessionSettings(home, dir, dir, "mayor", claudeRuntime())
	if err != nil || !s.Present {
		t.Fatalf("status = %+v, err = %v; want present", s, err)
	}
}

// A settings file that cannot be written leaves the session without its
// guards: hooks:absent, with the write error as the reason.
func TestSyncSessionSettings_AbsentWhenUnwritable(t *testing.T) {
	t.Parallel()
	home := hooks.HomeAt(t.TempDir())
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Join(blocker, "polecats") // under a regular file

	s, err := syncSessionSettings(home, settingsDir, t.TempDir(), "polecat", claudeRuntime())
	if err == nil {
		t.Fatal("want the write error")
	}
	if s.Present || s.EventType() != EventHooksAbsent || s.Reason == "" {
		t.Fatalf("status = %+v, want absent with a reason", s)
	}
	if got := s.Payload("gt-gastown-p-opal"); got["reason"] != s.Reason || got["session"] != "gt-gastown-p-opal" || got["role"] != "polecat" {
		t.Errorf("payload = %v", got)
	}
}

// A runtime with no Claude hooks never loads the managed guards (gt-be0z).
func TestSyncSessionSettings_AbsentWithoutClaudeHooks(t *testing.T) {
	t.Parallel()
	home := hooks.HomeAt(t.TempDir())
	dir := t.TempDir()
	for name, rc := range map[string]*config.RuntimeConfig{
		"no hooks":   {Command: "claude"},
		"none":       {Command: "claude", Hooks: &config.RuntimeHooksConfig{Provider: "none"}},
		"non-claude": {Command: "other", Hooks: &config.RuntimeHooksConfig{Provider: "unknown"}},
	} {
		s, err := syncSessionSettings(home, dir, dir, "crew", rc)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if s.Present || s.Path != "" || s.Reason == "" {
			t.Errorf("%s: status = %+v, want absent with a reason", name, s)
		}
	}
}

func TestHooksStatusPayload_PresentHasNoReason(t *testing.T) {
	t.Parallel()
	s := HooksStatus{Present: true, Role: "mayor", Path: "/town/mayor/.claude/settings.json"}
	p := s.Payload("hq-mayor")
	if _, ok := p["reason"]; ok || p["path"] != s.Path {
		t.Errorf("payload = %v", p)
	}
}
