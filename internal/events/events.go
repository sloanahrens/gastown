// Package events provides event logging for the gt activity feed.
//
// Events are written to ~/gt/.events.jsonl (raw audit log) and later
// curated by the feed daemon into ~/.feed.jsonl (user-facing).
package events

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Event represents an activity event in Gas Town.
type Event struct {
	Timestamp  string                 `json:"ts"`
	Source     string                 `json:"source"`
	Type       string                 `json:"type"`
	Actor      string                 `json:"actor"`
	Payload    map[string]interface{} `json:"payload,omitempty"`
	Visibility string                 `json:"visibility"`
}

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

	// Session events (for seance discovery)
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
	TypePatrolStarted    = "patrol_started"
	TypePolecatChecked   = "polecat_checked"
	TypePolecatNudged    = "polecat_nudged"
	TypeEscalationSent    = "escalation_sent"
	TypeEscalationAcked   = "escalation_acked"
	TypeEscalationClosed  = "escalation_closed"
	TypeEscalationDropped = "escalation_dropped" // gt escalate call itself failed — the alert never reached a bead
	TypePatrolComplete    = "patrol_complete"
	TypeDogCycleOutcome   = "dog_cycle_outcome" // A dog patrol cycle that did NOT end as a clean run (skipped/failed), recorded so a skipped cycle is never read as a clean one (gt-i3rpw)

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
// inlined at each call site, so the hermetic test tripwire's tolerance list
// (internal/testutil.BuiltinActorPrefixes) can be verified against the same
// values that actually get logged (gt-9pn).
const (
	ActorGt     = "gt"     // town-infrastructure events: boot, halt, spawn
	ActorDaemon = "daemon" // daemon-originated events, e.g. mass-death detection
)

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
	// Test binaries often run with cwd inside a real checkout under the
	// production town root; resolving from cwd would append fixture events
	// to the operator's live ~/gt/.events.jsonl (gt-x9o). Tests that want
	// event output must pass an explicit town root via the *To variants.
	//
	// GT_TEST_HERMETIC covers gt subprocesses spawned by tests: they are not
	// test binaries themselves, so testing.Testing() is false, but their cwd
	// may still resolve to the operator's live town (gt-lwi). The hermetic
	// test harness (internal/testutil) sets this variable.
	if testing.Testing() || os.Getenv("GT_TEST_HERMETIC") == "1" {
		return nil
	}

	townRoot, err := workspace.FindFromCwd()
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

// DonePayload creates a payload for done events.
func DonePayload(beadID, branch string) map[string]interface{} {
	return map[string]interface{}{
		"bead":   beadID,
		"branch": branch,
	}
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
