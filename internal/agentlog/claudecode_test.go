package agentlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeProjectDirFor(t *testing.T) {
	// The config dir is resolved from CLAUDE_CONFIG_DIR when set, so the test
	// owns it rather than inheriting whatever the environment carries.
	configDir := filepath.Join(t.TempDir(), "claude-town")
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	// Every non-alphanumeric character becomes '-', so the leading slash does,
	// and so do the dots and underscores Claude Code encodes that way.
	// e.g., /some/work/.dir_name → <config>/projects/-some-work--dir-name
	input := "/some/work/.dir_name"
	wantSuffix := "-some-work--dir-name"
	wantDir := filepath.Join(configDir, claudeProjectsSubdir, wantSuffix)

	got, err := claudeProjectDirFor(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantDir {
		t.Errorf("claudeProjectDirFor(%q) = %q, want %q", input, got, wantDir)
	}
}

// TestClaudeProjectHash pins the encoding against directory names observed on
// disk, including the dot-bearing one Gas Town itself creates (.repo.git).
func TestClaudeProjectHash(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/Users/me/gt", "-Users-me-gt"},
		{"/Users/me/.claude", "-Users-me--claude"},
		{"/Users/me/gt/gastown/.repo.git", "-Users-me-gt-gastown--repo-git"},
		{"/Users/me/gt/gastown/polecats/topaz/gastown", "-Users-me-gt-gastown-polecats-topaz-gastown"},
	}
	for _, tt := range tests {
		if got := claudeProjectHash(tt.path); got != tt.want {
			t.Errorf("claudeProjectHash(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// TestLatestTranscript_ResolvesAgainstConfigDir is the gt-xb27 regression:
// with CLAUDE_CONFIG_DIR set (as Gas Town sets it town-wide), the transcript
// must be found there rather than in ~/.claude, or every live agent looks
// undated.
func TestLatestTranscript_ResolvesAgainstConfigDir(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	// Hermetic: the legacy root is searched too, so it must hold nothing here.
	t.Setenv("HOME", t.TempDir())

	workDir := filepath.Join(t.TempDir(), "polecats", "topaz", "gastown")
	projectDir := filepath.Join(configDir, claudeProjectsSubdir, claudeProjectHash(filepath.ToSlash(workDir)))
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("creating project dir: %v", err)
	}

	older := filepath.Join(projectDir, "aaaaaaaa-0000-0000-0000-000000000000.jsonl")
	newer := filepath.Join(projectDir, "bbbbbbbb-1111-1111-1111-111111111111.jsonl")
	for _, path := range []string{older, newer} {
		if err := os.WriteFile(path, []byte("{}\n"), 0644); err != nil {
			t.Fatalf("writing transcript: %v", err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatalf("aging transcript: %v", err)
	}

	got, err := LatestTranscript(workDir)
	if err != nil {
		t.Fatalf("LatestTranscript: %v", err)
	}
	if got != newer {
		t.Errorf("LatestTranscript() = %q, want the newest transcript %q", got, newer)
	}
}

// TestLatestTranscript_NoTranscriptIsEmptyNotAnError keeps the absent case
// distinguishable from a failure: callers report "last activity unknown"
// rather than acting on it.
func TestLatestTranscript_NoTranscriptIsEmptyNotAnError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	got, err := LatestTranscript(filepath.Join(t.TempDir(), "nothing-here"))
	if err != nil {
		t.Fatalf("LatestTranscript: %v", err)
	}
	if got != "" {
		t.Errorf("LatestTranscript() = %q, want empty", got)
	}
}

// TestClaudeProjectDirsFor covers both roots, and their collapse to one when
// CLAUDE_CONFIG_DIR is unset (where the "configured" dir IS ~/.claude).
func TestClaudeProjectDirsFor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workDir := "/some/work/dir"
	hash := "-some-work-dir"

	t.Run("config dir set: configured root first, legacy root second", func(t *testing.T) {
		configDir := filepath.Join(t.TempDir(), "claude-town")
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)

		got, err := claudeProjectDirsFor(workDir)
		if err != nil {
			t.Fatalf("claudeProjectDirsFor: %v", err)
		}
		want := []string{
			filepath.Join(configDir, claudeProjectsSubdir, hash),
			filepath.Join(home, claudeProjectsDir, hash),
		}
		if len(got) != len(want) {
			t.Fatalf("claudeProjectDirsFor = %q, want %q", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("dirs[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("config dir unset: one root, not the same path twice", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "")

		got, err := claudeProjectDirsFor(workDir)
		if err != nil {
			t.Fatalf("claudeProjectDirsFor: %v", err)
		}
		want := []string{filepath.Join(home, claudeProjectsDir, hash)}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("claudeProjectDirsFor = %q, want %q", got, want)
		}
	})
}

// TestLatestTranscript_SearchesLegacyRoot is the gt-jxfe regression.
//
// A seat can log to ~/.claude (the claude-sonnet preset keeps its credentials
// there) while the town's CLAUDE_CONFIG_DIR root holds only a previous
// session's transcript. Picking the configured root's newest — which is what a
// single-root lookup does — hands the caller a transcript that predates the
// live session, and the session is reported as having no dated activity.
func TestLatestTranscript_SearchesLegacyRoot(t *testing.T) {
	configDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("HOME", home)

	workDir := filepath.Join(t.TempDir(), "polecats", "emerald", "gastown")
	hash := claudeProjectHash(filepath.ToSlash(workDir))

	staleDir := filepath.Join(configDir, claudeProjectsSubdir, hash)
	liveDir := filepath.Join(home, claudeProjectsDir, hash)
	for _, dir := range []string{staleDir, liveDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("creating project dir: %v", err)
		}
	}

	stale := filepath.Join(staleDir, "9b478215-0000-0000-0000-000000000000.jsonl")
	live := filepath.Join(liveDir, "a5aef6ba-1111-1111-1111-111111111111.jsonl")
	writeTranscript(t, stale, time.Now().Add(-time.Hour))
	writeTranscript(t, live, time.Now())

	got, err := LatestTranscript(workDir)
	if err != nil {
		t.Fatalf("LatestTranscript: %v", err)
	}
	if got != live {
		t.Errorf("LatestTranscript() = %q, want the live transcript in the legacy root %q", got, live)
	}
}

// TestLatestTranscript_NewestWinsAcrossRoots guards the direction of the
// multi-root pick: the configured root must not be preferred when the legacy
// root holds only an older transcript.
func TestLatestTranscript_NewestWinsAcrossRoots(t *testing.T) {
	configDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("HOME", home)

	workDir := filepath.Join(t.TempDir(), "polecats", "quartz", "gastown")
	hash := claudeProjectHash(filepath.ToSlash(workDir))

	configuredDir := filepath.Join(configDir, claudeProjectsSubdir, hash)
	legacyDir := filepath.Join(home, claudeProjectsDir, hash)
	for _, dir := range []string{configuredDir, legacyDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("creating project dir: %v", err)
		}
	}

	live := filepath.Join(configuredDir, "bbbbbbbb-1111-1111-1111-111111111111.jsonl")
	stale := filepath.Join(legacyDir, "aaaaaaaa-0000-0000-0000-000000000000.jsonl")
	writeTranscript(t, stale, time.Now().Add(-time.Hour))
	writeTranscript(t, live, time.Now())

	got, err := LatestTranscript(workDir)
	if err != nil {
		t.Fatalf("LatestTranscript: %v", err)
	}
	if got != live {
		t.Errorf("LatestTranscript() = %q, want the newest transcript across roots %q", got, live)
	}
}

// writeTranscript creates a transcript file with an explicit modification time.
func writeTranscript(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}\n"), 0644); err != nil {
		t.Fatalf("writing transcript: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("aging transcript: %v", err)
	}
}

func TestParseClaudeCodeLine_Text(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello world"}]},"timestamp":"2026-02-23T10:00:00Z"}`
	events := parseClaudeCodeLine(line, "hq-mayor", "claudecode", "test-uuid")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != "text" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "text")
	}
	if ev.Role != "assistant" {
		t.Errorf("Role = %q, want %q", ev.Role, "assistant")
	}
	if ev.Content != "Hello world" {
		t.Errorf("Content = %q, want %q", ev.Content, "Hello world")
	}
	if ev.SessionID != "hq-mayor" {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, "hq-mayor")
	}
	if ev.AgentType != "claudecode" {
		t.Errorf("AgentType = %q, want %q", ev.AgentType, "claudecode")
	}
}

func TestParseClaudeCodeLine_ToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`
	events := parseClaudeCodeLine(line, "s1", "claudecode", "test-uuid")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != "tool_use" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "tool_use")
	}
	if ev.Content == "" {
		t.Error("Content should not be empty for tool_use")
	}
	// Content should contain the tool name
	if len(ev.Content) < 4 || ev.Content[:4] != "Bash" {
		t.Errorf("Content %q should start with tool name 'Bash'", ev.Content)
	}
}

func TestParseClaudeCodeLine_SkipsUnknownTypes(t *testing.T) {
	line := `{"type":"summary","content":"some summary"}`
	events := parseClaudeCodeLine(line, "s1", "claudecode", "test-uuid")
	if len(events) != 0 {
		t.Errorf("expected 0 events for summary type, got %d", len(events))
	}
}

func TestParseClaudeCodeLine_InvalidJSON(t *testing.T) {
	events := parseClaudeCodeLine("not json", "s1", "claudecode", "test-uuid")
	if len(events) != 0 {
		t.Errorf("expected 0 events for invalid JSON, got %d", len(events))
	}
}

func TestNewAdapter(t *testing.T) {
	tests := []struct {
		name      string
		agentType string
		wantNil   bool
		wantType  string
	}{
		{"claudecode", "claudecode", false, "claudecode"},
		{"empty defaults to claudecode", "", false, "claudecode"},
		{"opencode", "opencode", false, "opencode"},
		{"unknown", "kiro", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAdapter(tt.agentType)
			if tt.wantNil {
				if a != nil {
					t.Errorf("expected nil adapter for %q", tt.agentType)
				}
				return
			}
			if a == nil {
				t.Fatalf("expected non-nil adapter for %q", tt.agentType)
			}
			if a.AgentType() != tt.wantType {
				t.Errorf("AgentType() = %q, want %q", a.AgentType(), tt.wantType)
			}
		})
	}
}
