package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadHookSessionID_EnvTakesPriority verifies GT_SESSION_ID env var is
// returned without touching stdin or persisted files.
func TestReadHookSessionID_EnvTakesPriority(t *testing.T) {
	want := "env-session-abc123"
	t.Setenv("GT_SESSION_ID", want)
	t.Setenv("CLAUDE_SESSION_ID", "should-not-use-this")

	id, _ := readHookSessionID()
	if id != want {
		t.Errorf("readHookSessionID() = %q, want %q", id, want)
	}
}

// TestReadHookSessionID_ClaudeSessionIDFallback verifies CLAUDE_SESSION_ID
// is used when GT_SESSION_ID is unset.
func TestReadHookSessionID_ClaudeSessionIDFallback(t *testing.T) {
	want := "claude-session-xyz"
	t.Setenv("GT_SESSION_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", want)

	id, _ := readHookSessionID()
	if id != want {
		t.Errorf("readHookSessionID() = %q, want %q", id, want)
	}
}

// TestReadHookSessionID_PersistedFileFallback verifies the persisted
// .runtime/session_id file is used when env vars are unset.
func TestReadHookSessionID_PersistedFileFallback(t *testing.T) {
	want := "persisted-session-456"
	t.Setenv("GT_SESSION_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", "")

	// Write a persisted session file in cwd (ReadPersistedSessionID checks cwd first)
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := fmt.Sprintf("%s\n%s\n", want, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(runtimeDir, "session_id"), []byte(content), 0644); err != nil {
		t.Fatalf("write session_id: %v", err)
	}

	// Change to the temp dir so ReadPersistedSessionID finds it via cwd
	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(origDir) })

	id, _ := readHookSessionID()
	if id != want {
		t.Errorf("readHookSessionID() = %q, want %q", id, want)
	}
}

// TestReadHookSessionID_SourceFromEnv verifies GT_HOOK_SOURCE env var
// populates the source return value.
func TestReadHookSessionID_SourceFromEnv(t *testing.T) {
	t.Setenv("GT_SESSION_ID", "some-id")
	t.Setenv("GT_HOOK_SOURCE", "compact")

	_, source := readHookSessionID()
	if source != "compact" {
		t.Errorf("source = %q, want %q", source, "compact")
	}
}

// TestReadHookSessionID_AutoGeneratesFallback verifies a UUID is generated
// when no env vars, stdin, or persisted file are available.
func TestReadHookSessionID_AutoGeneratesFallback(t *testing.T) {
	t.Setenv("GT_SESSION_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", "")

	// Use a temp dir with no .runtime/session_id
	dir := t.TempDir()
	origDir, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(origDir) })

	id, _ := readHookSessionID()
	if id == "" {
		t.Error("readHookSessionID() returned empty string, want auto-generated UUID")
	}
	// Should look like a UUID (36 chars with hyphens)
	if len(id) != 36 {
		t.Errorf("auto-generated id %q doesn't look like a UUID (len=%d)", id, len(id))
	}
}

// --- gt-uj9k: session_start attribution -------------------------------------

// withPrimeHookVars sets the package-level hook state that sessionStartReason
// reads, restoring it afterwards so these tests cannot leak into each other.
func withPrimeHookVars(t *testing.T, source, eventName string) {
	t.Helper()
	prevSource, prevEvent := primeHookSource, primeHookEventName
	t.Cleanup(func() {
		primeHookSource, primeHookEventName = prevSource, prevEvent
	})
	primeHookSource, primeHookEventName = source, eventName
}

// TestSessionStartReason_SpawnerAttributionWins pins the contract that lets a
// burst be traced to a caller: an explicit spawner reason outranks the runtime's
// own hook source, because only the spawner knows a start was a deliberate
// restart rather than a fresh boot.
func TestSessionStartReason_SpawnerAttributionWins(t *testing.T) {
	t.Setenv("GT_SESSION_START_REASON", "daemon-heartbeat")
	withPrimeHookVars(t, "startup", "SessionStart")

	if got := sessionStartReason(); got != "daemon-heartbeat" {
		t.Errorf("sessionStartReason() = %q, want daemon-heartbeat", got)
	}
}

// TestSessionStartReason_FallsBackToHookSource covers the un-instrumented
// spawners: without a spawner reason, the runtime's hook source is the best
// available "why".
func TestSessionStartReason_FallsBackToHookSource(t *testing.T) {
	t.Setenv("GT_SESSION_START_REASON", "")
	withPrimeHookVars(t, "compact", "PreCompact")

	if got := sessionStartReason(); got != "compact" {
		t.Errorf("sessionStartReason() = %q, want compact", got)
	}
}

// TestSessionStartReason_FallsBackToHookEventName covers runtimes that report
// the event but no source.
func TestSessionStartReason_FallsBackToHookEventName(t *testing.T) {
	t.Setenv("GT_SESSION_START_REASON", "")
	withPrimeHookVars(t, "", "SessionStart")

	if got := sessionStartReason(); got != "SessionStart" {
		t.Errorf("sessionStartReason() = %q, want SessionStart", got)
	}
}

// TestSessionStartReason_UnknownIsExplicit pins that an unattributable start is
// labelled "unknown" rather than emitted with an empty reason. "unknown" is
// itself the finding: it marks a start that came through neither an
// instrumented spawner nor a runtime hook, which is exactly the case that went
// unexplained before gt-uj9k.
func TestSessionStartReason_UnknownIsExplicit(t *testing.T) {
	t.Setenv("GT_SESSION_START_REASON", "")
	withPrimeHookVars(t, "", "")

	if got := sessionStartReason(); got != "unknown" {
		t.Errorf("sessionStartReason() = %q, want unknown", got)
	}
}

// TestSessionStartCaller covers the "who requested it" half of the pair.
func TestSessionStartCaller(t *testing.T) {
	t.Setenv("GT_SESSION_START_CALLER", "daemon")
	if got := sessionStartCaller(); got != "daemon" {
		t.Errorf("sessionStartCaller() = %q, want daemon", got)
	}

	t.Setenv("GT_SESSION_START_CALLER", "")
	if got := sessionStartCaller(); got != "unknown" {
		t.Errorf("sessionStartCaller() = %q, want unknown when unset", got)
	}
}
