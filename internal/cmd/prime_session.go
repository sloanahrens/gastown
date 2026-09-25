package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/runtime"
	"github.com/steveyegge/gastown/internal/workspace"
)

// primeHookEventName is the hook_event_name from the runtime's stdin JSON
// (SessionStart, PreCompact, ...). PreCompact output is not model context, so
// prime prints nothing for it.
var primeHookEventName string

// primeHookInputSeen records whether the runtime piped a hook payload on stdin
// for this run — one of the two signals that a `gt prime --hook` run came from
// the runtime rather than from the agent itself. See isRuntimeHookInvocation.
var primeHookInputSeen bool

// hookInput represents the JSON input from LLM runtime hooks.
// Claude Code sends this on stdin for SessionStart hooks.
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Source         string `json:"source"` // startup, resume, clear, compact
	HookEventName  string `json:"hook_event_name"`
}

// readHookSessionID reads session ID from available sources in hook mode.
//
// Priority (env vars first so non-Claude runtimes skip the stdin read entirely):
//  1. GT_SESSION_ID / CLAUDE_SESSION_ID env var  — set by the hook command
//  2. Stdin JSON (Claude Code format)            — Claude sends {"session_id":…,"source":…}
//  3. Persisted .runtime/session_id              — written by a prior SessionStart hook
//  4. Auto-generate UUID
//
// Source is resolved from GT_HOOK_SOURCE env, stdin JSON, or empty.
// Non-Claude runtimes (Gemini CLI, etc.) should set GT_SESSION_ID and
// GT_HOOK_SOURCE in their hook commands to get full --hook behavior with
// zero stdin delay. Example:
//
//	SessionStart: "export GT_SESSION_ID=$(uuidgen) GT_HOOK_SOURCE=startup && gt prime --hook"
//	PreCompress:  "export GT_HOOK_SOURCE=compact && gt prime --hook"
func readHookSessionID() (sessionID, source string) {
	primeStructuredSessionStartOutput = false
	primeHookEventName = ""
	primeHookInputSeen = false
	// Source can come from env (any runtime) or stdin JSON (Claude only).
	// Check env first so it's available even when stdin provides the session ID.
	source = os.Getenv("GT_HOOK_SOURCE")

	// 1. Environment variables (fast path — skips stdin read entirely)
	if id := os.Getenv("GT_SESSION_ID"); id != "" {
		return id, source
	}
	if id := os.Getenv("CLAUDE_SESSION_ID"); id != "" {
		return id, source
	}
	// 2. Try reading stdin JSON (Claude Code format).
	//    Checked before persisted file so a fresh Claude session always wins
	//    over a potentially stale .runtime/session_id from a previous session.
	if input := readStdinJSON(); input != nil {
		primeHookInputSeen = true
		primeStructuredSessionStartOutput = input.HookEventName == "SessionStart"
		primeHookEventName = input.HookEventName
		if input.SessionID != "" {
			// Stdin source overrides env source when both are present
			if input.Source != "" {
				source = input.Source
			}
			return input.SessionID, source
		}
	}

	// 3. Persisted session ID from a prior hook invocation (e.g., PreCompress
	//    reusing the session ID that SessionStart wrote to .runtime/session_id)
	if id := ReadPersistedSessionID(); id != "" {
		return id, source
	}

	// 4. Auto-generate
	return uuid.New().String(), source
}

// stdinReadTimeout is how long readStdinJSON waits for data before giving up.
// This is a safety net for runtimes that pipe stdin without sending data AND
// don't set GT_SESSION_ID. Claude Code sends JSON on the first tick, so 500ms
// is generous. Non-Claude runtimes should set GT_SESSION_ID to skip this entirely.
const stdinReadTimeout = 500 * time.Millisecond

// readStdinJSON attempts to read and parse JSON from stdin.
// Returns nil if stdin is empty, not a pipe, invalid JSON, or no data
// arrives within stdinReadTimeout.
func readStdinJSON() *hookInput {
	// Check if stdin has data (non-blocking)
	stat, err := os.Stdin.Stat()
	if err != nil {
		return nil
	}

	// Only read if stdin is a pipe or has data
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		// stdin is a terminal, not a pipe - no data to read
		return nil
	}

	// Read with timeout: some LLM runtimes pipe stdin without sending data,
	// which would block ReadString forever. Use a goroutine + timer so we
	// fall through to env-var / auto-generate paths after stdinReadTimeout.
	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		ch <- readResult{line, err}
	}()

	var line string
	select {
	case r := <-ch:
		if r.err != nil && r.line == "" {
			return nil
		}
		line = r.line
	case <-time.After(stdinReadTimeout):
		// The goroutine above is still blocked on ReadString and will leak.
		// This is intentional — gt prime is a short-lived CLI command that
		// exits shortly after, so the goroutine is cleaned up by process exit.
		return nil
	}

	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	var input hookInput
	if err := json.Unmarshal([]byte(line), &input); err != nil {
		return nil
	}

	return &input
}

// persistSessionID writes the session ID to .runtime/session_id
// This allows subsequent gt prime calls to find the session ID.
func persistSessionID(dir, sessionID string) {
	writeRuntimeStateFile(dir, constants.FileSessionID, sessionID)
}

// ReadPersistedSessionID reads a previously persisted session ID.
// Checks cwd first, then town root.
// Returns empty string if not found.
func ReadPersistedSessionID() string {
	return readRuntimeStateFileSearched(constants.FileSessionID)
}

// writeRuntimeStateFile writes one line of per-session state to
// <dir>/.runtime/<name>, stamped with when it was written.
//
// Failing to write is not fatal: the reader falls back to the behavior the state
// existed to suppress.
func writeRuntimeStateFile(dir, name, value string) {
	runtimeDir := filepath.Join(dir, constants.DirRuntime)
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return // Non-fatal
	}

	content := fmt.Sprintf("%s\n%s\n", value, time.Now().Format(time.RFC3339))
	_ = os.WriteFile(filepath.Join(runtimeDir, name), []byte(content), 0644) // Non-fatal
}

// readRuntimeStateFile returns the first line of <dir>/.runtime/<name>,
// or "" when the file is missing or empty.
func readRuntimeStateFile(dir, name string) string {
	data, err := os.ReadFile(filepath.Join(dir, constants.DirRuntime, name))
	if err != nil {
		return ""
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0])
	}
	return ""
}

// readRuntimeStateFileSearched reads <name> from the cwd first, then from the
// town root — the two places a hook invocation writes its session state, so
// that a prime run from a nested directory still finds what its session wrote.
func readRuntimeStateFileSearched(name string) string {
	// Try cwd first
	cwd, err := os.Getwd()
	if err == nil {
		if value := readRuntimeStateFile(cwd, name); value != "" {
			return value
		}
	}

	// Try town root
	townRoot, err := workspace.FindFromCwd()
	if err == nil && townRoot != "" {
		if value := readRuntimeStateFile(townRoot, name); value != "" {
			return value
		}
	}

	return ""
}

// resolveSessionIDForPrime finds the session ID from available sources.
// Priority: GT_SESSION_ID env, CLAUDE_SESSION_ID env, persisted file, fallback.
func resolveSessionIDForPrime(actor string) string {
	// 1. Try runtime's session ID lookup (checks GT_SESSION_ID_ENV, then CLAUDE_SESSION_ID)
	if id := runtime.SessionIDFromEnv(); id != "" {
		return id
	}

	// 2. Persisted session file (from gt prime --hook)
	if id := ReadPersistedSessionID(); id != "" {
		return id
	}

	// 3. Fallback to generated identifier
	return fmt.Sprintf("%s-%d", actor, os.Getpid())
}

// isRuntimeHookInvocation reports whether the *runtime* invoked this run as a
// hook — SessionStart or PreCompact — rather than the agent running
// `gt prime --hook` by hand.
//
// Only the runtime produces either signal: a hook payload on stdin, or
// GT_HOOK_SOURCE named by the hook command. Attribution cannot stand in for
// them, because a spawner's GT_SESSION_START_REASON lives on the *session* env:
// the agent's own re-prime inherits it and reports it too.
func isRuntimeHookInvocation() bool {
	return primeHookInputSeen || primeHookSource != ""
}

// sessionStartAlreadyEmitted reports whether sessionID's session_start is
// already on the record for this session.
func sessionStartAlreadyEmitted(sessionID string) bool {
	return readRuntimeStateFileSearched(constants.FileSessionStartEmitted) == sessionID
}

// recordSessionStartEmitted notes that sessionID's session_start is on the
// record. Written wherever the session ID is persisted — the cwd, and the town
// root when different — so a later run finds it the way it finds the ID.
func recordSessionStartEmitted(workDir, townRoot, sessionID string) {
	writeRuntimeStateFile(workDir, constants.FileSessionStartEmitted, sessionID)
	if townRoot != "" && townRoot != workDir {
		writeRuntimeStateFile(townRoot, constants.FileSessionStartEmitted, sessionID)
	}
}

// shouldEmitSessionStart reports whether this `gt prime --hook` run records a
// session_start for sessionID. (gt-da73)
//
// The startup beacon tells every agent to run `gt prime --hook` for context, so
// a session's hook recorded a start and the agent's own re-prime recorded a
// second one for the same ID, which it resolves from the .runtime/session_id
// the hook wrote. One session records one start.
//
// A later run still records when the runtime invoked it — compaction, resume,
// clear — so a long session keeps its lifecycle events (the field gt-uj9k added
// to tell them apart). A first run records even unattributed, so a runtime whose
// hook names no source is not silenced.
func shouldEmitSessionStart(sessionID string) bool {
	if !sessionStartAlreadyEmitted(sessionID) {
		return true
	}
	return isRuntimeHookInvocation()
}

// emitSessionEvent emits a session_start event for seance discovery.
// The event is written to ~/gt/.events.jsonl and can be queried via gt seance.
// Session ID resolution order: GT_SESSION_ID, CLAUDE_SESSION_ID, persisted file, fallback.
func emitSessionEvent(ctx RoleContext) {
	if ctx.Role == RoleUnknown {
		return
	}

	// Get agent identity for the actor field
	actor := getAgentIdentity(ctx)
	if actor == "" {
		return
	}

	// Get session ID from multiple sources
	sessionID := resolveSessionIDForPrime(actor)

	// One session_start per session (gt-da73).
	if !shouldEmitSessionStart(sessionID) {
		return
	}

	// Determine topic from hook state or default
	topic := ""
	if ctx.Role == RoleWitness || ctx.Role == RoleRefinery || ctx.Role == RoleDeacon {
		topic = "patrol"
	}

	// Emit the event, attributed with why and at whose request (gt-uj9k).
	payload := events.SessionPayload(events.SessionStartInfo{
		SessionID: sessionID,
		Role:      actor,
		Topic:     topic,
		Cwd:       ctx.WorkDir,
		Reason:    sessionStartReason(),
		Caller:    sessionStartCaller(),
	})
	if err := events.LogFeed(events.TypeSessionStart, actor, payload); err != nil {
		return
	}
	// Record the start only once it is logged: if the write failed, the session
	// stays unrecorded and a later run can still put it on the record.
	recordSessionStartEmitted(ctx.WorkDir, ctx.TownRoot, sessionID)
}

// sessionStartReason reports why this session started.
//
// A spawner that knows its own intent wins (GT_SESSION_START_REASON, e.g.
// "daemon-heartbeat" or "install"): that is the only signal that can tell a
// deliberate restart apart from a burst. Otherwise fall back to the runtime's
// own hook source — startup/resume/clear/compact, resolved into primeHookSource
// by prime's source handling — and finally to the hook event name.
//
// "unknown" is what remains for an emitted start that names none of those: a
// hook that piped a session ID without naming its event, or a session's first
// recorded start when no hook named its source. It is diagnostic — a start that
// came through neither an instrumented spawner nor a named runtime hook — and
// it is no longer the label the agent's own re-prime gets, because that run is
// not a session start and does not emit (shouldEmitSessionStart).
func sessionStartReason() string {
	if r := os.Getenv(constants.EnvSessionStartReason); r != "" {
		return r
	}
	if primeHookSource != "" {
		return primeHookSource
	}
	if primeHookEventName != "" {
		return primeHookEventName
	}
	return "unknown"
}

// sessionStartCaller reports who requested this session start, or "unknown"
// when the start did not come through an instrumented spawner. Callers are
// expected to also set Reason; this identifies the actor, that one the intent.
func sessionStartCaller() string {
	if c := os.Getenv(constants.EnvSessionStartCaller); c != "" {
		return c
	}
	return "unknown"
}

// outputSessionMetadata prints a structured metadata line for seance discovery.
// Format: [GAS TOWN] role:<role> pid:<pid> session:<session_id>
// This enables gt seance to discover sessions from gt prime output.
func outputSessionMetadata(ctx RoleContext) {
	if ctx.Role == RoleUnknown {
		return
	}

	// Get agent identity for the role field
	actor := getAgentIdentity(ctx)
	if actor == "" {
		return
	}

	// Get session ID from multiple sources
	sessionID := resolveSessionIDForPrime(actor)

	// Output structured metadata line
	fmt.Println(formatSessionMetadataLine(actor, sessionID))
}

// formatSessionMetadataLine keeps the bracketed "[GAS TOWN]" banner for normal
// human-facing output, but removes the leading brackets during structured
// SessionStart hooks because Codex will see '[' and try to parse the line as
// JSON instead of treating it as plain text session metadata.
func formatSessionMetadataLine(actor, sessionID string) string {
	if primeStructuredSessionStartOutput {
		return fmt.Sprintf("GAS TOWN role:%s pid:%d session:%s", actor, os.Getpid(), sessionID)
	}
	return fmt.Sprintf("[GAS TOWN] role:%s pid:%d session:%s", actor, os.Getpid(), sessionID)
}

// formatPrimeStatusLine applies the same convention as
// formatSessionMetadataLine to prime's other bracketed status lines (e.g. the
// deacon patrol-counter reset, gt-wdv9): the "[prime]" prefix is for normal
// human-facing output only. Structured SessionStart output drops the leading
// bracket so a runtime that sniffs a line's first character for JSON (Codex)
// does not misparse it.
func formatPrimeStatusLine(msg string) string {
	if primeStructuredSessionStartOutput {
		return "prime: " + msg
	}
	return "[prime] " + msg
}

// --- Session state detection (merged from prime_state.go) ---

// SessionState represents the detected session state for observability.
type SessionState struct {
	State         string `json:"state"`                    // normal, post-handoff, crash-recovery, autonomous
	Role          Role   `json:"role"`                     // detected role
	PrevSession   string `json:"prev_session,omitempty"`   // for post-handoff
	CheckpointAge string `json:"checkpoint_age,omitempty"` // for crash-recovery
	HookedBead    string `json:"hooked_bead,omitempty"`    // for autonomous
}

// detectSessionState returns the current session state without side effects.
func detectSessionState(ctx RoleContext) SessionState {
	state := SessionState{
		State: "normal",
		Role:  ctx.Role,
	}

	// Check for handoff marker (post-handoff state)
	markerPath := filepath.Join(ctx.WorkDir, constants.DirRuntime, constants.FileHandoffMarker)
	if data, err := os.ReadFile(markerPath); err == nil {
		state.State = "post-handoff"
		state.PrevSession = strings.TrimSpace(string(data))
		return state
	}

	// Check for checkpoint (crash-recovery state) - only for polecat/crew
	if ctx.Role == RolePolecat || ctx.Role == RoleCrew {
		if cp, err := checkpoint.Read(ctx.WorkDir); err == nil && cp != nil && !cp.IsStale(24*time.Hour) {
			state.State = "crash-recovery"
			state.CheckpointAge = cp.Age().Round(time.Minute).String()
			return state
		}
	}

	// Check for hooked work (autonomous state).
	// Primary: read hook_bead from the agent bead's DB column (same strategy as gt hook).
	// Fallback: query hooked/in_progress beads by assignee.
	agentID := getAgentIdentity(ctx)
	if agentID != "" {
		// Use rig beads directory, not polecat worktree. Polecats don't have their
		// own .beads — the rig's beads dir is the authoritative source. (GH#2503)
		beadsDir := ctx.WorkDir
		if ctx.Rig != "" && ctx.TownRoot != "" {
			rigDir := filepath.Join(ctx.TownRoot, ctx.Rig)
			if _, err := os.Stat(filepath.Join(rigDir, ".beads")); err == nil {
				beadsDir = rigDir
			}
		}
		b := beads.New(beadsDir)
		// Primary: agent bead's hook_bead field (authoritative, set by bd slot set during sling)
		agentBeadID := buildAgentBeadID(agentID, ctx.Role, ctx.TownRoot)
		if agentBeadID != "" {
			agentBeadDir := beads.ResolveHookDir(ctx.TownRoot, agentBeadID, ctx.WorkDir)
			ab := beads.New(agentBeadDir)
			if agentBead, err := ab.Show(agentBeadID); err == nil && agentBead != nil && agentBead.HookBead != "" {
				// Resolve and verify the target bead exists with active status
				// (mirrors molecule_status.go and signal_stop.go patterns)
				hookBeadDir := beads.ResolveHookDir(ctx.TownRoot, agentBead.HookBead, ctx.WorkDir)
				hb := beads.New(hookBeadDir)
				if hookBead, err := hb.Show(agentBead.HookBead); err == nil && hookBead != nil &&
					(hookBead.Status == beads.StatusHooked || hookBead.Status == "in_progress") {
					state.State = "autonomous"
					state.HookedBead = agentBead.HookBead
					return state
				}
			}
		}

		// Fallback: query active assigned work across durable issues and wisps.
		hookedBeads, err := listAssignedActiveWork(b, agentID)
		if err == nil && len(hookedBeads) > 0 {
			state.State = "autonomous"
			state.HookedBead = hookedBeads[0].ID
			return state
		}
		// Town-level fallback: rig-level agents may have hooked HQ beads
		// stored in townRoot/.beads. Matches prime.go and molecule_status.go. (gt-dtq7)
		if !isTownLevelRole(agentID) && ctx.TownRoot != "" {
			townB := beads.New(filepath.Join(ctx.TownRoot, ".beads"))
			if townWork, err := listAssignedActiveWork(townB, agentID); err == nil && len(townWork) > 0 {
				state.State = "autonomous"
				state.HookedBead = townWork[0].ID
				return state
			}
		}
	}

	return state
}

// checkHandoffMarker checks for a handoff marker file and outputs a warning if found.
// This prevents the "handoff loop" bug where a new session sees /handoff in context
// and incorrectly runs it again. The marker tells the new session: "handoff is DONE,
// the /handoff you see in context was from YOUR PREDECESSOR, not a request for you."
//
// The marker format is: "session_id\nreason" (reason is optional, on second line).
// When present, the reason is stored in primeHandoffReason for compact/resume detection.
// This enables compaction-triggered handoff cycles to route through the lighter
// compact/resume path instead of full re-initialization. (GH#1965)
func checkHandoffMarker(workDir string) {
	markerPath := filepath.Join(workDir, constants.DirRuntime, constants.FileHandoffMarker)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		// No marker = not post-handoff, normal startup
		return
	}

	// Parse marker: first line is session ID, optional second line is reason
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	prevSession := strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		primeHandoffReason = strings.TrimSpace(lines[1])
	}

	// Remove the marker FIRST so we don't warn twice
	_ = os.Remove(markerPath)

	// Output prominent warning
	outputHandoffWarning(prevSession)
}

// checkHandoffMarkerDryRun checks for handoff marker without removing it (for --dry-run).
func checkHandoffMarkerDryRun(workDir string) {
	markerPath := filepath.Join(workDir, constants.DirRuntime, constants.FileHandoffMarker)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		// No marker = not post-handoff, normal startup
		explain(true, "Post-handoff: no handoff marker found")
		return
	}

	// Parse marker: first line is session ID, optional second line is reason
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	prevSession := strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		primeHandoffReason = strings.TrimSpace(lines[1])
	}

	explain(true, fmt.Sprintf("Post-handoff: marker found (predecessor: %s, reason: %s), marker NOT removed in dry-run", prevSession, primeHandoffReason))

	// Output the warning but don't remove marker
	outputHandoffWarning(prevSession)
}
