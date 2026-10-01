package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testPrimeHookEnv is a hook-mode process in dir with the given environment,
// no stdin payload, no town, and a fixed fresh session ID.
func testPrimeHookEnv(dir string, env map[string]string) primeHookEnv {
	return primeHookEnv{
		getenv:   envMap(env),
		stdin:    func() *hookInput { return nil },
		getwd:    func() (string, error) { return dir, nil },
		findTown: func() (string, error) { return "", errors.New("not in a town") },
		newID:    func() string { return "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" },
	}
}

// TestReadHookSessionID_EnvTakesPriority verifies GT_SESSION_ID env var is
// returned without touching stdin or persisted files.
func TestReadHookSessionID_EnvTakesPriority(t *testing.T) {
	t.Parallel()
	e := testPrimeHookEnv(t.TempDir(), map[string]string{"GT_SESSION_ID": "env-session-abc123", "CLAUDE_SESSION_ID": "should-not-use-this"})
	e.stdin = func() *hookInput { t.Error("stdin read despite GT_SESSION_ID"); return nil }
	if got := e.readHookSession().sessionID; got != "env-session-abc123" {
		t.Errorf("session ID = %q, want the GT_SESSION_ID value", got)
	}
}

// TestReadHookSessionID_ClaudeSessionIDFallback verifies CLAUDE_SESSION_ID
// is used when GT_SESSION_ID is unset.
func TestReadHookSessionID_ClaudeSessionIDFallback(t *testing.T) {
	t.Parallel()
	e := testPrimeHookEnv(t.TempDir(), map[string]string{"CLAUDE_SESSION_ID": "claude-session-xyz"})
	if got := e.readHookSession().sessionID; got != "claude-session-xyz" {
		t.Errorf("session ID = %q, want the CLAUDE_SESSION_ID value", got)
	}
}

// TestReadHookSessionID_StdinPayload: Claude's hook payload names the session
// and its source, and marks the run as the runtime's.
func TestReadHookSessionID_StdinPayload(t *testing.T) {
	t.Parallel()
	e := testPrimeHookEnv(t.TempDir(), map[string]string{"GT_HOOK_SOURCE": "startup"})
	e.stdin = func() *hookInput {
		return &hookInput{SessionID: "stdin-id", Source: "compact", HookEventName: "SessionStart"}
	}
	r := e.readHookSession()
	if r.sessionID != "stdin-id" || r.source != "compact" || !r.inputSeen || !r.structuredSessionStart || r.eventName != "SessionStart" {
		t.Errorf("read = %+v, want the payload's session, source and event", r)
	}
}

// TestReadHookSessionID_PersistedFileFallback verifies the persisted
// .runtime/session_id file is used when env vars are unset.
func TestReadHookSessionID_PersistedFileFallback(t *testing.T) {
	t.Parallel()
	want := "persisted-session-456"
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := fmt.Sprintf("%s\n%s\n", want, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(runtimeDir, "session_id"), []byte(content), 0644); err != nil {
		t.Fatalf("write session_id: %v", err)
	}
	if got := testPrimeHookEnv(dir, nil).readHookSession().sessionID; got != want {
		t.Errorf("session ID = %q, want %q", got, want)
	}
}

// TestReadHookSessionID_SourceFromEnv verifies GT_HOOK_SOURCE env var
// populates the source return value.
func TestReadHookSessionID_SourceFromEnv(t *testing.T) {
	t.Parallel()
	e := testPrimeHookEnv(t.TempDir(), map[string]string{"GT_SESSION_ID": "some-id", "GT_HOOK_SOURCE": "compact"})
	if got := e.readHookSession().source; got != "compact" {
		t.Errorf("source = %q, want compact", got)
	}
}

// TestReadHookSessionID_AutoGeneratesFallback verifies a UUID is generated
// when no env vars, stdin, or persisted file are available.
func TestReadHookSessionID_AutoGeneratesFallback(t *testing.T) {
	t.Parallel()
	e := realPrimeHookEnv()
	e.getenv = envMap(nil)
	e.stdin = func() *hookInput { return nil }
	dir := t.TempDir()
	e.getwd = func() (string, error) { return dir, nil }
	e.findTown = func() (string, error) { return "", errors.New("not in a town") }
	id := e.readHookSession().sessionID
	if len(id) != 36 {
		t.Errorf("auto-generated id %q doesn't look like a UUID (len=%d)", id, len(id))
	}
}

// --- gt-uj9k: session_start attribution -------------------------------------

// TestSessionStartReason_SpawnerAttributionWins pins the contract that lets a
// burst be traced to a caller: an explicit spawner reason outranks the runtime's
// own hook source, because only the spawner knows a start was a deliberate
// restart rather than a fresh boot.
func TestSessionStartReason_SpawnerAttributionWins(t *testing.T) {
	t.Parallel()
	env := envMap(map[string]string{"GT_SESSION_START_REASON": "daemon-heartbeat"})
	if got := sessionStartReasonFrom(env, "startup", "SessionStart"); got != "daemon-heartbeat" {
		t.Errorf("sessionStartReason() = %q, want daemon-heartbeat", got)
	}
}

// TestSessionStartReason_FallsBackToHookSource covers the un-instrumented
// spawners: without a spawner reason, the runtime's hook source is the best
// available "why".
func TestSessionStartReason_FallsBackToHookSource(t *testing.T) {
	t.Parallel()
	if got := sessionStartReasonFrom(envMap(nil), "compact", "PreCompact"); got != "compact" {
		t.Errorf("sessionStartReason() = %q, want compact", got)
	}
}

// TestSessionStartReason_FallsBackToHookEventName covers runtimes that report
// the event but no source.
func TestSessionStartReason_FallsBackToHookEventName(t *testing.T) {
	t.Parallel()
	if got := sessionStartReasonFrom(envMap(nil), "", "SessionStart"); got != "SessionStart" {
		t.Errorf("sessionStartReason() = %q, want SessionStart", got)
	}
}

// TestSessionStartReason_UnknownIsExplicit pins that an unattributable start is
// labelled "unknown" rather than emitted with an empty reason. "unknown" is
// itself the finding: it marks a start that came through neither an
// instrumented spawner nor a runtime hook, which is exactly the case that went
// unexplained before gt-uj9k.
func TestSessionStartReason_UnknownIsExplicit(t *testing.T) {
	t.Parallel()
	if got := sessionStartReasonFrom(envMap(nil), "", ""); got != "unknown" {
		t.Errorf("sessionStartReason() = %q, want unknown", got)
	}
}

// TestSessionStartCaller covers the "who requested it" half of the pair.
func TestSessionStartCaller(t *testing.T) {
	t.Parallel()
	if got := sessionStartCallerFrom(envMap(map[string]string{"GT_SESSION_START_CALLER": "daemon"})); got != "daemon" {
		t.Errorf("sessionStartCaller() = %q, want daemon", got)
	}
	if got := sessionStartCallerFrom(envMap(nil)); got != "unknown" {
		t.Errorf("sessionStartCaller() = %q, want unknown when unset", got)
	}
}

// TestShouldEmitSessionStart pins the once-per-session rule (gt-da73): a
// session's start is recorded once, and only a run the runtime invoked as a
// hook may record a later one.
func TestShouldEmitSessionStart(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			// The worktree the session's runtime state lives in — where the hook
			// writes the session ID and the recorded start, and where a later
			// prime run in the same session looks for them.
			dir := t.TempDir()
			if tc.recorded != "" {
				recordSessionStartEmitted(dir, dir, tc.recorded)
			}
			e := testPrimeHookEnv(dir, nil)
			if got := e.shouldEmitSessionStart(tc.sessionID, isRuntimeHookInvocationFrom(tc.inputSeen, tc.source)); got != tc.want {
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
	t.Parallel()
	dir := t.TempDir()
	const sessionID = "ba6f760f-05e9-4b5d-a7d5-cabbd43c36e2"
	env := map[string]string{"GT_SESSION_START_REASON": "daemon-heartbeat", "GT_SESSION_START_CALLER": "daemon"}
	recordSessionStartEmitted(dir, dir, sessionID)

	// The agent's re-prime, in a refinery's session env: the reason survives,
	// the runtime's own signal does not.
	e := testPrimeHookEnv(dir, env)
	if got := sessionStartReasonFrom(e.getenv, "", ""); got != "daemon-heartbeat" {
		t.Fatalf("precondition: re-prime reason = %q, want the inherited daemon-heartbeat", got)
	}
	if e.shouldEmitSessionStart(sessionID, isRuntimeHookInvocationFrom(false, "")) {
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
	t.Parallel()
	// A polecat worktree: the hook persists its runtime state here (cwd), and
	// the agent's own prime runs here too.
	worktree := t.TempDir()

	// 1. SessionStart: the runtime invokes the hook, which names the source.
	//    (Claude pipes its own payload instead; either signal is the runtime's.)
	hook := testPrimeHookEnv(worktree, map[string]string{"GT_HOOK_SOURCE": "startup"}).readHookSession()
	if hook.sessionID == "" {
		t.Fatal("precondition: the hook run resolved no session ID")
	}
	e := testPrimeHookEnv(worktree, nil)
	if !e.shouldEmitSessionStart(hook.sessionID, isRuntimeHookInvocationFrom(hook.inputSeen, hook.source)) {
		t.Fatal("the SessionStart hook must record the session start")
	}
	persistSessionID(worktree, hook.sessionID)
	recordSessionStartEmitted(worktree, worktree, hook.sessionID) // emitSessionEvent's bookkeeping

	// 2. The agent's own `gt prime --hook`, from the beacon prompt. Nothing here
	//    signals a hook: Claude Code gives a Bash call a character device on
	//    stdin rather than a payload, and the hook's GT_HOOK_SOURCE is not part
	//    of the session env.
	agent := e.readHookSession()
	agentIsHook := isRuntimeHookInvocationFrom(agent.inputSeen, agent.source)
	if agentIsHook {
		t.Fatalf("precondition: the agent's re-prime looks like a hook invocation (%+v)", agent)
	}
	persistSessionID(worktree, agent.sessionID) // the hook-mode bookkeeping runs here too

	if agent.sessionID != hook.sessionID {
		t.Fatalf("precondition: the agent's re-prime must resolve the hook's session ID (%s), got %s", hook.sessionID, agent.sessionID)
	}
	if e.shouldEmitSessionStart(agent.sessionID, agentIsHook) {
		t.Error("the agent's own `gt prime --hook` recorded a second session_start for one session")
	}
}

// TestSessionIDForPrime pins the order prime resolves its session ID in: the
// ID this run's hook read wins (gt prime --hook no longer publishes it into
// the process environment), then the runtime's env, then the persisted file,
// then the actor-and-pid fallback.
func TestSessionIDForPrime(t *testing.T) {
	t.Parallel()
	none := func() string { return "" }
	is := func(id string) func() string { return func() string { return id } }
	tests := []struct {
		name               string
		hookID             string
		fromEnv, persisted func() string
		want               string
	}{
		{name: "hook ID wins over env and file", hookID: "hook-id", fromEnv: is("env-id"), persisted: is("file-id"), want: "hook-id"},
		{name: "env without a hook ID", fromEnv: is("env-id"), persisted: is("file-id"), want: "env-id"},
		{name: "persisted file without env", fromEnv: none, persisted: is("file-id"), want: "file-id"},
		{name: "fallback names actor and pid", fromEnv: none, persisted: none, want: "gastown/crew/den-42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sessionIDForPrime(tt.hookID, tt.fromEnv, tt.persisted, "gastown/crew/den", 42); got != tt.want {
				t.Fatalf("sessionIDForPrime = %q, want %q", got, tt.want)
			}
		})
	}
}
