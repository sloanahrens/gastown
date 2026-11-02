// Package events provides event logging for the gt activity feed.
//
// Events are written to ~/gt/.events.jsonl (raw audit log).
package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/workspace"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// Event represents an activity event in Gas Town.
type Event struct {
	Timestamp  string                 `json:"ts"`
	Source     string                 `json:"source"`
	Type       string                 `json:"type"`
	Actor      string                 `json:"actor"`
	Payload    map[string]interface{} `json:"payload,omitempty"`
	Visibility string                 `json:"visibility"`
	// Workspace is the workspace id the town maps the event's rig to, omitted
	// on lines written before the field existed.
	Workspace string `json:"workspace,omitempty"`
}

// WorkspaceUnresolved is the workspace id stamped on an event whose town
// workspace map cannot be read, so the event is still written (gt-91ty4).
const WorkspaceUnresolved = "unresolved"

// Visibility levels for events.
const (
	VisibilityAudit = "audit" // Only in raw events log
	VisibilityFeed  = "feed"  // Appears in curated feed
	VisibilityBoth  = "both"  // Both audit and feed
)

// Common event types for gt commands.
const (
	TypeSling   = "sling"
	TypeHook    = "hook"
	TypeUnhook  = "unhook"
	TypeHandoff = "handoff"
	TypeDone    = "done"
	TypeMail    = "mail"
	TypeSpawn   = "spawn"
	TypeKill    = "kill"
	TypeNudge   = "nudge"
	TypeBoot    = "boot"
	TypeHalt    = "halt"

	// TypeWake records an explicit agent-session resume: `gt session start`
	// or `gt session restart` brought a seat back. It is the agent's own
	// session_start (TypeSessionStart), seen from the command that asked for
	// it — the record exists even when the session never gets far enough to
	// prime and emit its own (gt-tcrgb, gt-i057g).
	TypeWake = "wake"

	// TypeHandoffNoPersist records a handoff whose mail never reached Dolt.
	// A false handoff: the command failed and the successor will find an
	// empty hook, so it is distinct from TypeHandoff rather than a field on
	// one (gt-i057g).
	TypeHandoffNoPersist = "handoff_nopersist"

	// TypeOverrunStop records the overrun guard ending a bead's run: it
	// stopped the seat and handed the bead back, so the next dispatch of that
	// bead is a new run with its own clock (DispatchStart). It is not
	// TypeDone, which means the seat itself submitted, landed or closed the
	// work: a stop is the guard's act, and gt log reads the two differently.
	// Its payload may carry "at", the time the run ended when that is not the
	// time the record was written (a stop retried on later ticks), so a
	// repeated record does not move the boundary.
	TypeOverrunStop = "overrun_stop"

	// TypeBeadClosed records a work bead closed through gt bead close. It ends
	// the bead's run as a done event does, so a bead closed by hand and
	// dispatched again later starts its clock over instead of being dated from
	// the dispatch of the run that was closed (DispatchStart). A landed bead
	// needs no such record: the landing follows the seat's own gt done, whose
	// TypeDone ends the run first.
	TypeBeadClosed = "bead_closed"

	// Session lifecycle events
	TypeSessionStart = "session_start"
	TypeSessionEnd   = "session_end"

	// Session death events (for crash investigation)
	TypeSessionDeath = "session_death" // Feed-visible session termination
	TypeMassDeath    = "mass_death"    // Multiple sessions died in short window

	// TypeRefineryRestartDecision records why a refinery session that already
	// existed was kept or killed. Restarts used to be silent on the daemon-log
	// side, which is why the gt-uj9k respawn burst went unexplained.
	TypeRefineryRestartDecision = "refinery_restart_decision"

	// Witness patrol events
	TypePatrolStarted     = "patrol_started"
	TypePolecatChecked    = "polecat_checked"
	TypePolecatNudged     = "polecat_nudged"
	TypeEscalationSent    = "escalation_sent"
	TypeEscalationAcked   = "escalation_acked"
	TypeEscalationClosed  = "escalation_closed"
	TypeEscalationDropped = "escalation_dropped" // gt escalate call itself failed — the alert never reached a bead
	TypePatrolComplete    = "patrol_complete"
	TypeDogCycleOutcome   = "dog_cycle_outcome" // A daemon dog job cycle with a failed step, recorded so a failure reaches readers of the feed (gt-i3rpw)

	// Merge queue events (emitted by refinery)
	TypeMergeStarted = "merge_started"
	TypeMerged       = "merged"
	TypeMergeFailed  = "merge_failed"
	TypeMergeSkipped = "merge_skipped"

	// Destructive Dolt cleanup audit events (gt-87a). Intent is written before
	// the first DROP so a crash mid-cleanup still leaves a durable record.
	TypeDoltCleanupIntent = "dolt_cleanup_intent" // Forced cleanup about to remove databases
	TypeDoltCleanupDone   = "dolt_cleanup_done"   // Forced cleanup finished (even if 0 removed)

	// Scheduler events
	TypeSchedulerEnqueue        = "scheduler_enqueue"         // Bead scheduled for deferred dispatch
	TypeSchedulerDispatch       = "scheduler_dispatch"        // Bead dispatched from scheduler
	TypeSchedulerDispatchFailed = "scheduler_dispatch_failed" // Bead dispatch failed (requeued)
	TypeSchedulerCloseRetry     = "scheduler_close_retry"     // Context close needed last-resort attempt

	// Container-gate slot telemetry (gt-dc81): one slot_wait per grant, one
	// slot_hold per release, so the cost of serializing container-backed suites
	// townwide is measurable after the fact instead of inferred from panes.
	TypeSlotWait = "slot_wait"
	TypeSlotHold = "slot_hold"

	// TypeWorktreePrune records a destructive `git worktree remove` performed
	// by a bash-executed patrol step (e.g. the dead-dog-worktree cleanup in
	// mol-deacon-patrol.formula.toml) that has no Go call site of its own to
	// emit events.LogFeed directly. See "gt log prune-worktree".
	TypeWorktreePrune = "worktree_prune"

	// TypePolecatBranchRepairFailed records a session start whose worktree
	// repair (moving off the base branch) did not run. It exists for the
	// witness restart path, which discards the stderr warning that would
	// otherwise be the only report (gt-ns8t).
	TypePolecatBranchRepairFailed = "polecat_branch_repair_failed"
)

// EventsFile is the name of the raw events log.
const EventsFile = ".events.jsonl"

// Infrastructure actors: literal actor values for events with no owning
// agent role (internal/cmd.Role covers agent-originated actors instead —
// see internal/cmd.AllRoles and detectActor). Named here, rather than
// inlined at each call site, so gt doctor's test-leaks tolerance list
// (internal/doctor.BuiltinActorPrefixes) can be verified against the same
// values that actually get logged (gt-9pn).
const (
	ActorGt     = "gt"     // town-infrastructure events: boot, halt, spawn
	ActorDaemon = "daemon" // daemon-originated events, e.g. mass-death detection
)

// Payload "caller" stamps for the town processes that author session events
// from outside a test binary. Those events name the tmux session as their
// actor ("gt-opal", "hq-deacon"), a prefix the town does not know, so gt
// doctor's test-leaks check would read the town's own housekeeping as leaked
// test fixtures. The stamp is the whole test: a fixture actor logged with any
// other caller is still reported (gt-d9423, gt-rqajq).
const (
	CallerDaemon = "daemon"    // daemon crash detection and idle reap
	CallerDoctor = "gt doctor" // gt doctor fix zombie-sessions / orphan-processes / orphan-sessions
	CallerDown   = "gt down"   // gt down town-session shutdown
)

// townProcessSessionCallers is the set IsTownProcessSessionCaller accepts.
var townProcessSessionCallers = map[string]bool{
	CallerDaemon: true,
	CallerDoctor: true,
	CallerDown:   true,
}

// IsTownProcessSessionCaller reports whether caller is a stamp the test-leaks
// tripwire tolerates, the writer being the town rather than a test fixture
// that reached the live events log (gt-x9o).
func IsTownProcessSessionCaller(caller string) bool {
	return townProcessSessionCallers[caller]
}

// Log writes an event to the events log.
// The event is appended to <town-root>/.events.jsonl, with the town root
// resolved from the current working directory. Callers that already know
// their town root (e.g., the daemon via its config) should use LogTo instead.
// Returns nil if logging fails (events are best-effort).
func Log(eventType, actor string, payload map[string]interface{}, visibility string) error {
	return write(newEvent(eventType, actor, payload, visibility))
}

// LogTo is like Log but writes to an explicitly provided town root instead of
// resolving one from the current working directory.
func LogTo(townRoot, eventType, actor string, payload map[string]interface{}, visibility string) error {
	return writeTo(townRoot, newEvent(eventType, actor, payload, visibility))
}

// LogFeed is a convenience wrapper for feed-visible events.
func LogFeed(eventType, actor string, payload map[string]interface{}) error {
	return Log(eventType, actor, payload, VisibilityFeed)
}

// LogFeedTo is like LogFeed but writes to an explicitly provided town root.
func LogFeedTo(townRoot, eventType, actor string, payload map[string]interface{}) error {
	return LogTo(townRoot, eventType, actor, payload, VisibilityFeed)
}

// LogAudit is a convenience wrapper for audit-only events.
func LogAudit(eventType, actor string, payload map[string]interface{}) error {
	return Log(eventType, actor, payload, VisibilityAudit)
}

// LogAuditTo is like LogAudit but writes to an explicitly provided town root.
func LogAuditTo(townRoot, eventType, actor string, payload map[string]interface{}) error {
	return LogTo(townRoot, eventType, actor, payload, VisibilityAudit)
}

func newEvent(eventType, actor string, payload map[string]interface{}, visibility string) Event {
	return Event{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Source:     "gt",
		Type:       eventType,
		Actor:      actor,
		Payload:    payload,
		Visibility: visibility,
	}
}

// write appends an event to the events file of the town root resolved from
// the current working directory.
func write(event Event) error {
	return writeFromCwd(event, workspace.FindFromCwd)
}

// writeFromCwd is write with the cwd town-root resolver explicit. It keeps
// the process's own test-binary and hermetic checks, so a test can hand it a
// fixture town and prove the gt-x9o guard still refuses to write there.
func writeFromCwd(event Event, findRoot func() (string, error)) error {
	return writeVia(event, testing.Testing(), os.Getenv, findRoot)
}

// writeVia is write with its inputs explicit: whether this is a test binary,
// the environment, and the cwd town-root resolver.
func writeVia(event Event, underTest bool, getenv func(string) string, findRoot func() (string, error)) error {
	// Test binaries often run with cwd inside a real checkout under the
	// production town root; resolving from cwd would append fixture events
	// to the operator's live ~/gt/.events.jsonl (gt-x9o). Tests that want
	// event output must pass an explicit town root via the *To variants.
	//
	// GT_TEST_HERMETIC covers gt subprocesses spawned by tests: they are not
	// test binaries themselves, so testing.Testing() is false, but their cwd
	// may still resolve to the operator's live town (gt-lwi). The hermetic
	// test harness (internal/testutil) sets this variable.
	if underTest || getenv("GT_TEST_HERMETIC") == "1" {
		return nil
	}

	townRoot, err := findRoot()
	if err != nil || townRoot == "" {
		// Silently ignore - we're not in a Gas Town workspace
		return nil
	}
	return writeTo(townRoot, event)
}

// writeTo appends an event to the events file under the given town root.
// Uses flock for cross-process synchronization — sync.Mutex only protects
// intra-process goroutines, but multiple gt processes write concurrently.
func writeTo(townRoot string, event Event) error {
	if townRoot == "" {
		return nil
	}

	event.Workspace = workspaceOf(townRoot, event.Payload)

	eventsPath := filepath.Join(townRoot, EventsFile)

	// Marshal event to JSON
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshaling event: %w", err)
	}
	data = append(data, '\n')

	// Acquire cross-process file lock
	fl := flock.New(eventsPath + ".lock")
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquiring events file lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck // best-effort unlock

	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644) //nolint:gosec // G302: events file is non-sensitive operational data
	if err != nil {
		return fmt.Errorf("opening events file: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing event: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing events file: %w", err)
	}

	return nil
}

// workspaceOf is the workspace id an event written under townRoot carries: the
// payload's "rig" mapped through internal/workspaceid, or the town default
// when the payload names none. An unreadable or malformed map, and a rig that
// is present but not a usable prefix, are WorkspaceUnresolved rather than a
// dropped event or a guessed id (gt-91ty4).
func workspaceOf(townRoot string, payload map[string]interface{}) string {
	m, err := loadWorkspaceMap(townRoot)
	if err != nil {
		return WorkspaceUnresolved
	}
	rig, present := payload["rig"]
	if !present {
		return m.Default()
	}
	prefix, ok := rig.(string)
	if !ok || prefix == "" {
		return WorkspaceUnresolved
	}
	id, err := m.For(prefix)
	if err != nil {
		return WorkspaceUnresolved
	}
	return id
}

// workspaceMapRead is one parse of a town's workspace map: the file's contents
// and the map parsed from exactly those contents.
type workspaceMapRead struct {
	raw []byte
	m   *workspaceid.Map
}

// workspaceMapCache holds the last parse of each town's workspace map, keyed by
// town root.
type workspaceMapCache struct {
	mu    sync.Mutex
	towns map[string]workspaceMapRead
}

var cachedWorkspaceMaps workspaceMapCache

// loadWorkspaceMap returns the town's workspace map. Every event write asks for
// it. The file is read each time, and parsed again only when its contents differ
// from the last parse, so an edit is never missed: the key is the content itself,
// not a modification time or size that a rewrite can leave unchanged. A file that
// cannot be read is not cached: workspaceid.Load decides the result, as it does
// for an absent file.
func loadWorkspaceMap(townRoot string) (*workspaceid.Map, error) {
	if townRoot == "" {
		return workspaceid.Load(townRoot)
	}
	path := filepath.Join(townRoot, workspaceid.File)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the town root the caller named
	if err != nil {
		return workspaceid.Load(townRoot)
	}
	c := &cachedWorkspaceMaps
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.towns[townRoot]; ok && bytes.Equal(r.raw, data) {
		return r.m, nil
	}
	m, err := workspaceid.Parse(path, data)
	if err != nil {
		delete(c.towns, townRoot)
		return nil, err
	}
	if c.towns == nil {
		c.towns = make(map[string]workspaceMapRead)
	}
	c.towns[townRoot] = workspaceMapRead{raw: data, m: m}
	return m, nil
}

// Payload helpers for common event structures.

// SlingPayload creates a payload for sling events.
func SlingPayload(beadID, target string) map[string]interface{} {
	return map[string]interface{}{
		"bead":   beadID,
		"target": target,
	}
}

// HookPayload creates a payload for hook events.
func HookPayload(beadID string) map[string]interface{} {
	return map[string]interface{}{
		"bead": beadID,
	}
}

// HandoffFailedPayload creates a payload for TypeHandoffNoPersist: the handoff
// mail never reached Dolt, so there is no successor session to name and the
// persistence error is the record.
func HandoffFailedPayload(subject string, persistErr error) map[string]interface{} {
	p := map[string]interface{}{}
	if subject != "" {
		p["subject"] = subject
	}
	if persistErr != nil {
		p["error"] = persistErr.Error()
	}
	return p
}

// HandoffPayload creates a payload for handoff events.
func HandoffPayload(subject string, toSession bool) map[string]interface{} {
	p := map[string]interface{}{
		"to_session": toSession,
	}
	if subject != "" {
		p["subject"] = subject
	}
	return p
}

// Exit statuses a done event's payload records, the values gt done accepts in
// --status. They live here because the dispatch clock reads them and this
// package cannot import internal/done (which imports this one); a test in
// internal/done holds the two sets equal.
const (
	DoneStatusCompleted = "COMPLETED" // the seat submitted the work for landing
	DoneStatusEscalated = "ESCALATED" // the seat stopped on a blocker, bead left open
	DoneStatusDeferred  = "DEFERRED"  // the seat paused the work for a later turn
)

// DonePayload creates a payload for done events that do not record their exit
// status: the host's repaired event for a sandbox seat's submission, and events
// written before the status was logged.
func DonePayload(beadID, branch string) map[string]interface{} {
	return DonePayloadWithStatus(beadID, branch, "")
}

// DonePayloadWithStatus creates a payload for a done event that records how the
// seat's turn ended. status is the gt done exit type (DoneStatus*), "" to omit
// it. The dispatch clock reads it to tell a run that ended from a move of the
// bead between live seats (DispatchStart).
func DonePayloadWithStatus(beadID, branch, status string) map[string]interface{} {
	p := map[string]interface{}{
		"bead":   beadID,
		"branch": branch,
	}
	if status != "" {
		p["status"] = status
	}
	return p
}

// MailPayload creates a payload for mail events.
func MailPayload(to, subject string) map[string]interface{} {
	return map[string]interface{}{
		"to":      to,
		"subject": subject,
	}
}

// SpawnPayload creates a payload for spawn events.
func SpawnPayload(rig, polecat string) map[string]interface{} {
	return map[string]interface{}{
		"rig":     rig,
		"polecat": polecat,
	}
}

// BootPayload creates a payload for rig boot events.
func BootPayload(rig string, agents []string) map[string]interface{} {
	return map[string]interface{}{
		"rig":    rig,
		"agents": agents,
	}
}

// MergePayload creates a payload for merge queue events.
// mrID: merge request ID
// worker: polecat name that submitted the work
// branch: source branch being merged
// reason: failure reason (for merge_failed/merge_skipped events)
func MergePayload(mrID, worker, branch, reason string) map[string]interface{} {
	p := map[string]interface{}{
		"mr":     mrID,
		"worker": worker,
		"branch": branch,
	}
	if reason != "" {
		p["reason"] = reason
	}
	return p
}

// PatrolPayload creates a payload for patrol start/complete events.
func PatrolPayload(rig string, polecatCount int, message string) map[string]interface{} {
	p := map[string]interface{}{
		"rig":           rig,
		"polecat_count": polecatCount,
	}
	if message != "" {
		p["message"] = message
	}
	return p
}

// PolecatCheckPayload creates a payload for polecat check events.
func PolecatCheckPayload(rig, polecat, status, issue string) map[string]interface{} {
	p := map[string]interface{}{
		"rig":     rig,
		"polecat": polecat,
		"status":  status,
	}
	if issue != "" {
		p["issue"] = issue
	}
	return p
}

// NudgePayload creates a payload for nudge events.
func NudgePayload(rig, target, reason string) map[string]interface{} {
	return map[string]interface{}{
		"rig":    rig,
		"target": target,
		"reason": reason,
	}
}

// EscalationPayload creates a payload for escalation events.
func EscalationPayload(rig, target, to, reason string) map[string]interface{} {
	return map[string]interface{}{
		"rig":    rig,
		"target": target,
		"to":     to,
		"reason": reason,
	}
}

// UnhookPayload creates a payload for unhook events.
func UnhookPayload(beadID string) map[string]interface{} {
	return map[string]interface{}{
		"bead": beadID,
	}
}

// KillPayload creates a payload for kill events.
func KillPayload(rig, target, reason string) map[string]interface{} {
	return map[string]interface{}{
		"rig":    rig,
		"target": target,
		"reason": reason,
	}
}

// WakePayload creates a payload for wake events. context is what the resuming
// command knew: the hooked issue for `gt session start`, who requested it for
// `gt session restart`.
func WakePayload(rig, context string) map[string]interface{} {
	p := map[string]interface{}{
		"rig": rig,
	}
	if context != "" {
		p["context"] = context
	}
	return p
}

// HaltPayload creates a payload for halt events.
func HaltPayload(services []string) map[string]interface{} {
	return map[string]interface{}{
		"services": services,
	}
}

// SessionDeathPayload creates a payload for session death events.
// session: tmux session name that died
// agent: Gas Town agent identity (e.g., "gastown/polecats/Toast")
// reason: why the session was killed (e.g., "zombie cleanup", "user request", "doctor fix")
// caller: what initiated the kill (e.g., "daemon", "doctor", "gt down")
func SessionDeathPayload(session, agent, reason, caller string) map[string]interface{} {
	return map[string]interface{}{
		"session": session,
		"agent":   agent,
		"reason":  reason,
		"caller":  caller,
	}
}

// MassDeathPayload creates a payload for mass death events.
// count: number of sessions that died
// window: time window in which deaths occurred (e.g., "5s")
// sessions: list of session names that died
// possibleCause: suspected cause if known
func MassDeathPayload(count int, window string, sessions []string, possibleCause string) map[string]interface{} {
	p := map[string]interface{}{
		"count":    count,
		"window":   window,
		"sessions": sessions,
	}
	if possibleCause != "" {
		p["possible_cause"] = possibleCause
	}
	return p
}

// SessionStartInfo describes a session_start event: which session started,
// and — the part that was missing before gt-uj9k — why and at whose request.
type SessionStartInfo struct {
	// SessionID is the Claude Code session UUID.
	SessionID string
	// Role is the Gas Town role (e.g., "gastown/crew/joe", "deacon").
	Role string
	// Topic is what the session is working on, if known.
	Topic string
	// Cwd is the working directory.
	Cwd string
	// Reason is why this session started. Values come from the hook source
	// (startup/resume/clear/compact) or from GT_SESSION_START_REASON when a
	// spawner sets it explicitly (e.g. "daemon-heartbeat", "install").
	Reason string
	// Caller is who requested the start: the role or subsystem that spawned
	// this session, from GT_SESSION_START_CALLER. "unknown" when the start
	// did not come through an instrumented spawner.
	Caller string
}

// SessionPayload creates a payload for session start/end events.
//
// reason and caller are always emitted, even when empty-resolved, so a burst
// of session_starts can be attributed without cross-referencing process PIDs
// (gt-uj9k). "who requested it and why" is the field pair that was missing
// when 4-8 unlogged refinery respawns went unexplained.
func SessionPayload(info SessionStartInfo) map[string]interface{} {
	p := map[string]interface{}{
		"session_id": info.SessionID,
		"role":       info.Role,
		"actor_pid":  fmt.Sprintf("%s-%d", info.Role, os.Getpid()),
		"reason":     info.Reason,
		"caller":     info.Caller,
	}
	if info.Topic != "" {
		p["topic"] = info.Topic
	}
	if info.Cwd != "" {
		p["cwd"] = info.Cwd
	}
	return p
}

// SchedulerEnqueuePayload creates a payload for scheduler enqueue events.
func SchedulerEnqueuePayload(beadID, rig string) map[string]interface{} {
	return map[string]interface{}{
		"bead": beadID,
		"rig":  rig,
	}
}

// SchedulerDispatchPayload creates a payload for scheduler dispatch events.
func SchedulerDispatchPayload(beadID, rig, polecat string) map[string]interface{} {
	return map[string]interface{}{
		"bead":    beadID,
		"rig":     rig,
		"polecat": polecat,
	}
}

// SchedulerDispatchFailedPayload creates a payload for scheduler dispatch failure events.
func SchedulerDispatchFailedPayload(beadID, rig, errMsg string) map[string]interface{} {
	return map[string]interface{}{
		"bead":  beadID,
		"rig":   rig,
		"error": errMsg,
	}
}

// WorktreePrunePayload creates a payload for worktree prune events.
// kind: what kind of orphaned worktree this was (e.g. "dog")
// owner: the name of the entity that owned the worktree (e.g. dog name)
// path: filesystem path of the pruned worktree
func WorktreePrunePayload(kind, owner, path string) map[string]interface{} {
	return map[string]interface{}{
		"kind":  kind,
		"owner": owner,
		"path":  path,
	}
}
