package polecat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SessionHeartbeatStaleThreshold is the age at which a polecat session heartbeat
// is considered stale, indicating the agent process is likely dead.
// Configurable via operational.polecat.heartbeat_stale_threshold in settings/config.json.
const SessionHeartbeatStaleThreshold = 3 * time.Minute

// HeartbeatState represents the agent-reported state in a heartbeat v2 (gt-3vr5).
// Agents report their own state; the witness makes exactly one inference:
// "is the heartbeat fresh?" Everything else is agent-reported.
type HeartbeatState string

const (
	// HeartbeatWorking means the agent is actively processing.
	HeartbeatWorking HeartbeatState = "working"
	// HeartbeatIdle means the agent is waiting for input.
	HeartbeatIdle HeartbeatState = "idle"
	// HeartbeatExiting means the agent is in the gt done flow.
	HeartbeatExiting HeartbeatState = "exiting"
	// HeartbeatStuck means the agent self-reports being stuck.
	HeartbeatStuck HeartbeatState = "stuck"
)

// SessionHeartbeat represents a polecat session's heartbeat file.
// v1: timestamp only. v2 (gt-3vr5): adds agent-reported state, context, and bead.
type SessionHeartbeat struct {
	Timestamp time.Time      `json:"timestamp"`
	State     HeartbeatState `json:"state,omitempty"`   // v2: agent-reported state
	Context   string         `json:"context,omitempty"` // v2: what the agent is doing
	Bead      string         `json:"bead,omitempty"`    // v2: current hook bead ID
}

// EffectiveState returns the agent-reported state, defaulting to HeartbeatWorking
// for v1 heartbeats without a state field (backwards compatibility). See gt-3vr5.
func (h *SessionHeartbeat) EffectiveState() HeartbeatState {
	if h.State == "" {
		return HeartbeatWorking
	}
	return h.State
}

// IsV2 returns true if this heartbeat carries a state field (heartbeat v2).
// Used by the witness to decide whether to use agent-reported state or fall
// through to legacy timer-based detection.
func (h *SessionHeartbeat) IsV2() bool {
	return h.State != ""
}

// heartbeatsDir returns the directory for polecat session heartbeat files.
// Heartbeats live under <townRoot>/.runtime/heartbeats/, parallel to .runtime/pids/.
func heartbeatsDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "heartbeats")
}

// heartbeatFile returns the path to a heartbeat file for a given session.
func heartbeatFile(townRoot, sessionName string) string {
	return filepath.Join(heartbeatsDir(townRoot), sessionName+".json")
}

// HeartbeatKeepAliveInterval is how often a long-running gt done stage renews
// the session heartbeat (gt-azmw).
//
// Derived from SessionHeartbeatStaleThreshold (3m) — the shortest freshness
// window any consumer applies — rather than picked as a round number. The
// witness treats a stale state="exiting" heartbeat as a dead agent and falls
// through to its done-intent restart (internal/witness/handlers.go), and the
// daemon's idle-reaper treats a stale heartbeat as an abandoned session and
// kills it (internal/daemon/daemon.go). Renewing six times per stale window
// keeps "this polecat is alive and inside gt done" unambiguous in both.
const HeartbeatKeepAliveInterval = SessionHeartbeatStaleThreshold / 6

// StartExitingHeartbeatKeepAlive renews sessionName's heartbeat with
// state="exiting" every HeartbeatKeepAliveInterval until the returned stop
// function is called (safe to call more than once). It returns a no-op stop
// when the town root or session name is unknown.
//
// Why this exists (gt-azmw): gt done writes state="exiting" once at its start
// and then runs its gate stages, silent the whole way — the container-gate slot
// wait (20m) plus the changed-package test run (10m) of gt-h9kf's default
// test-verify, or up to five 10m --pre-verified gates — with
// no intervening gt subcommand to re-touch the heartbeat through
// persistentPreRun. Every consumer's staleness threshold is far shorter than
// that, so a healthy polecat sitting in a gate looked abandoned: on
// 2026-09-16 the daemon idle-reaper killed gastown/amethyst 19m into a gt done
// whose gate was still running, taking the process — and the MR it had not yet
// created — with it.
//
// The keep-alive is deliberately scoped by the CALLER to one bounded stage,
// never to all of gt done: a gt done wedged outside a bounded child (a hung
// bd/Dolt call, say) must still age out and stay reapable, which is the whole
// reason the idle-reaper exists. A caller that holds this across an unbounded
// wait would make a wedged session immortal.
//
// The first renewal is written synchronously, so the session is fresh from the
// instant the stage starts rather than one interval later.
func StartExitingHeartbeatKeepAlive(townRoot, sessionName, context, bead string) func() {
	return startHeartbeatKeepAlive(townRoot, sessionName, context, bead, HeartbeatKeepAliveInterval)
}

// startHeartbeatKeepAlive is StartExitingHeartbeatKeepAlive with an injectable
// interval, so tests can observe renewal without waiting out the real one.
func startHeartbeatKeepAlive(townRoot, sessionName, context, bead string, interval time.Duration) func() {
	if townRoot == "" || sessionName == "" {
		return func() {}
	}

	TouchSessionHeartbeatWithState(townRoot, sessionName, HeartbeatExiting, context, bead)

	done := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	go func() {
		// closed last, after the ticker is stopped, so a stop that waits on it
		// is waiting on a goroutine that can no longer renew the heartbeat.
		defer close(exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				TouchSessionHeartbeatWithState(townRoot, sessionName, HeartbeatExiting, context, bead)
			}
		}
	}()
	// Stop is a barrier, not a signal: it returns only once the renewal
	// goroutine has actually exited, so no write can land after it returns.
	// Closing done alone left one write in flight — a ticker tick already past
	// the select but still inside TouchSessionHeartbeatWithState (mkdir,
	// marshal, write) — which lands after stop and makes the heartbeat look
	// live again to a caller that reads it immediately afterwards. That is the
	// "kept renewing after stop" flake (gt-nyh8): under host load the write
	// straddled the caller's first read, so the test saw the pre-write value as
	// "frozen" and the post-write value as a renewal. Waiting for exited makes
	// the last renewal the caller can observe the last one that exists, which
	// is what every reader of a stopped keep-alive already assumes.
	return func() {
		once.Do(func() {
			close(done)
			<-exited
		})
	}
}

// TouchSessionHeartbeat writes or updates the heartbeat file for a polecat session.
// Writes state="working" by default (heartbeat v2, gt-3vr5).
// This is best-effort: errors are silently ignored because heartbeat signals
// are non-critical and should not interrupt gt commands.
func TouchSessionHeartbeat(townRoot, sessionName string) {
	TouchSessionHeartbeatWithState(townRoot, sessionName, HeartbeatWorking, "", "")
}

// TouchSessionHeartbeatWithState writes a heartbeat with explicit state information.
// Used by gt done (state="exiting") and gt heartbeat (state="stuck"). See gt-3vr5.
// This is best-effort: errors are silently ignored.
func TouchSessionHeartbeatWithState(townRoot, sessionName string, state HeartbeatState, context, bead string) {
	dir := heartbeatsDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	hb := SessionHeartbeat{
		Timestamp: time.Now().UTC(),
		State:     state,
		Context:   context,
		Bead:      bead,
	}

	data, err := json.Marshal(hb)
	if err != nil {
		return
	}

	_ = os.WriteFile(heartbeatFile(townRoot, sessionName), data, 0644)
}

// ReadSessionHeartbeat reads the heartbeat for a polecat session.
// Returns nil if the file doesn't exist or can't be read.
func ReadSessionHeartbeat(townRoot, sessionName string) *SessionHeartbeat {
	data, err := os.ReadFile(heartbeatFile(townRoot, sessionName))
	if err != nil {
		return nil
	}

	var hb SessionHeartbeat
	if err := json.Unmarshal(data, &hb); err != nil {
		return nil
	}

	return &hb
}

// IsSessionHeartbeatStale returns true if the session's heartbeat is older than
// the stale threshold, or if no heartbeat file exists.
//
// When no heartbeat file exists, this returns false to avoid false positives
// during the rollout period where sessions may not yet be touching heartbeats.
// The caller should fall back to other liveness checks in that case.
func IsSessionHeartbeatStale(townRoot, sessionName string) (stale bool, exists bool) {
	hb := ReadSessionHeartbeat(townRoot, sessionName)
	if hb == nil {
		return false, false
	}
	return time.Since(hb.Timestamp) >= SessionHeartbeatStaleThreshold, true
}

// RemoveSessionHeartbeat removes the heartbeat file for a session.
// Called during session cleanup.
func RemoveSessionHeartbeat(townRoot, sessionName string) {
	_ = os.Remove(heartbeatFile(townRoot, sessionName))
}
