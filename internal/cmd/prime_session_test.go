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

// withPrimeHookInput pins the "the runtime piped a hook payload on stdin" half
// of the hook-invocation signal. withPrimeHookVars covers the other half
// (GT_HOOK_SOURCE, resolved into primeHookSource).
func withPrimeHookInput(t *testing.T, seen bool) {
	t.Helper()
	prev := primeHookInputSeen
	t.Cleanup(func() { primeHookInputSeen = prev })
	primeHookInputSeen = seen
}

// TestShouldEmitSessionStart pins the once-per-session rule (gt-da73): a
// session's start is recorded once, and only a run the runtime invoked as a
// hook may record a later one.
func TestShouldEmitSessionStart(t *testing.T) {
	const (
		sessionA = "1b3f4a37-0f3b-4a48-8a25-2f1d6a4b9c11" // the session that started
		sessionB = "9d0c1e2f-5567-4a1b-9a3c-8e7d6f5b4a29" // a session that has not
	)

	cases := []struct {
		name      string
		sessionID string // the ID this run resolves
		recorded  string // the ID already on the record ("" = none)
		inputSeen bool   // the runtime piped a hook payload on stdin
		source    string // GT_HOOK_SOURCE named by the hook command
		want      bool
	}{
		{
			name:      "SessionStart records the session's start",
			sessionID: sessionA, inputSeen: true, source: "startup", want: true,
		},
		{
			name:      "the agent's own re-prime is not a second start",
			sessionID: sessionA, recorded: sessionA, want: false,
		},
		{
			name:      "a session's first run records even with nothing naming it",
			sessionID: sessionA, want: true,
		},
		{
			name:      "a new session is not masked by the previous session's record",
			sessionID: sessionB, recorded: sessionA, inputSeen: true, source: "startup", want: true,
		},
		{
			name:      "a post-compaction SessionStart still records",
			sessionID: sessionA, recorded: sessionA, inputSeen: true, source: "compact", want: true,
		},
		{
			name:      "a resume SessionStart still records",
			sessionID: sessionA, recorded: sessionA, inputSeen: true, source: "resume", want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The worktree the session's runtime state lives in — where the hook
			// writes the session ID and the recorded start, and where a later
			// prime run in the same session looks for them.
			dir := t.TempDir()
			t.Chdir(dir)
			if tc.recorded != "" {
				recordSessionStartEmitted(dir, dir, tc.recorded)
			}
			withPrimeHookInput(t, tc.inputSeen)
			withPrimeHookVars(t, tc.source, "")

			if got := shouldEmitSessionStart(tc.sessionID); got != tc.want {
				t.Errorf("shouldEmitSessionStart(%s) = %v, want %v", tc.sessionID, got, tc.want)
			}
		})
	}
}

// TestShouldEmitSessionStart_SpawnerAttributionIsNotEnough pins the finding that
// ruled out a reason-based dedupe for gt-da73.
//
// A spawner's attribution is set on the *session* env (internal/refinery and
// internal/daemon do this), so the agent's own re-prime inherits it and reports
// the same reason as the hook's start: two records identical in every field but
// whether the runtime ran the command.
func TestShouldEmitSessionStart_SpawnerAttributionIsNotEnough(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	const sessionID = "ba6f760f-05e9-4b5d-a7d5-cabbd43c36e2"

	t.Setenv("GT_SESSION_START_REASON", "daemon-heartbeat")
	t.Setenv("GT_SESSION_START_CALLER", "daemon")
	recordSessionStartEmitted(dir, dir, sessionID)

	// The agent's re-prime, in a refinerys session env: the reason survives, the
	// runtime's own signal does not.
	withPrimeHookInput(t, false)
	withPrimeHookVars(t, "", "")

	if got := sessionStartReason(); got != "daemon-heartbeat" {
		t.Fatalf("precondition: re-prime reason = %q, want the inherited daemon-heartbeat", got)
	}
	if shouldEmitSessionStart(sessionID) {
		t.Error("re-prime of a recorded session recorded a second start; attribution alone cannot separate it from the hook's")
	}
}

// TestAgentRePrimeIsNotASecondSessionStart walks the gt-da73 duplicate end to
// end through both seams that produce it: the SessionStart hook records the
// session's start, then the agent follows the startup beacon it was launched
// with — "Run `gt prime --hook` and begin work on your hook" — and runs that
// same command itself.
//
// The two runs land on ONE session ID, because the agent's run finds the ID the
// hook persisted. That is why the duplicate read as a single session starting
// twice rather than as a stray start.
func TestAgentRePrimeIsNotASecondSessionStart(t *testing.T) {
	// A polecat worktree: the hook persists its runtime state here (cwd), and
	// the agent's own prime runs here too.
	worktree := t.TempDir()
	t.Chdir(worktree)
	t.Setenv("GT_SESSION_ID", "")
	t.Setenv("CLAUDE_SESSION_ID", "")

	// 1. SessionStart: the runtime invokes the hook, which names the source.
	//    (Claude pipes its own payload instead; either signal is the runtime's.)
	t.Setenv("GT_HOOK_SOURCE", "startup")
	hookID, hookSource := readHookSessionID()
	primeHookSource = hookSource // what handlePrimeHookMode leaves for the emitter
	if hookID == "" {
		t.Fatal("precondition: the hook run resolved no session ID")
	}
	if !shouldEmitSessionStart(hookID) {
		t.Fatal("the SessionStart hook must record the session start")
	}
	persistSessionID(worktree, hookID)
	recordSessionStartEmitted(worktree, worktree, hookID) // emitSessionEvent's bookkeeping

	// 2. The agent's own `gt prime --hook`, from the beacon prompt. Nothing here
	//    signals a hook: Claude Code gives a Bash call a character device on
	//    stdin rather than a payload, and the hook's GT_HOOK_SOURCE is not part
	//    of the session env.
	t.Setenv("GT_HOOK_SOURCE", "")
	agentID, agentSource := readHookSessionID()
	primeHookSource = agentSource
	if isRuntimeHookInvocation() {
		t.Fatalf("precondition: the agent's re-prime looks like a hook invocation "+
			"(stdin payload seen=%v, source=%q)", primeHookInputSeen, primeHookSource)
	}
	persistSessionID(worktree, agentID) // the hook-mode bookkeeping runs here too

	if agentID != hookID {
		t.Fatalf("precondition: the agent's re-prime must resolve the hook's session ID (%s), got %s", hookID, agentID)
	}
	if shouldEmitSessionStart(agentID) {
		t.Error("the agent's own `gt prime --hook` recorded a second session_start for one session")
	}
}
