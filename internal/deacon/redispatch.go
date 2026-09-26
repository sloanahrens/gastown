// Package deacon provides the Deacon agent infrastructure.
package deacon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/util"
)

// Default parameters for re-dispatch rate-limiting.
// Configurable via operational.deacon.max_redispatches and
// operational.deacon.redispatch_cooldown in settings/config.json.
const (
	// DefaultMaxRedispatches is the number of times a bead can be re-dispatched
	// before escalating to Mayor instead of re-slinging.
	DefaultMaxRedispatches = 3

	// DefaultRedispatchCooldown is the minimum time between re-dispatches of
	// the same bead. Prevents thrashing when a bead keeps killing polecats.
	DefaultRedispatchCooldown = 5 * time.Minute

	// DefaultMaxDeferrals is the number of consecutive deferrals — a
	// persistent pool-full refusal, a standing operator hold, or a per-rig
	// ESTOP — tolerated before the deacon escalates to Mayor instead of
	// deferring forever. A deferral deliberately never advances AttemptCount
	// (gt-xdaq: the bead was queued, not failed), so a PERSISTENT deferral
	// condition would otherwise never reach the --max-attempts escalation
	// either — the bead just defers every cycle, silently, forever (gt-oo494).
	DefaultMaxDeferrals = 3
)

// RedispatchState tracks re-dispatch attempts for recovered beads.
// Persisted to deacon/redispatch-state.json.
type RedispatchState struct {
	// Beads maps bead ID to their re-dispatch tracking state.
	Beads map[string]*BeadRedispatchState `json:"beads"`

	// LastUpdated is when this state was last written.
	LastUpdated time.Time `json:"last_updated"`
}

// BeadRedispatchState tracks the re-dispatch history for a single bead.
type BeadRedispatchState struct {
	// BeadID is the bead identifier.
	BeadID string `json:"bead_id"`

	// AttemptCount is total number of re-dispatch attempts for this bead.
	AttemptCount int `json:"attempt_count"`

	// LastAttemptTime is when the last re-dispatch was attempted.
	LastAttemptTime time.Time `json:"last_attempt_time,omitempty"`

	// LastRig is the rig where the last re-dispatch was sent.
	LastRig string `json:"last_rig,omitempty"`

	// LastAgent is the agent alias used for the last re-dispatch (empty = rig default).
	LastAgent string `json:"last_agent,omitempty"`

	// Escalated is true if this bead has been escalated to Mayor.
	Escalated bool `json:"escalated,omitempty"`

	// EscalatedAt is when the bead was escalated.
	EscalatedAt time.Time `json:"escalated_at,omitempty"`

	// DeferralCount is the number of consecutive deferrals (pool-full
	// refusal, operator hold, or rig ESTOP) since the last actual dispatch
	// attempt. RecordAttempt resets it to 0, so it only measures an
	// unbroken streak — the condition that matters for DefaultMaxDeferrals
	// is whether the bead is stuck right now, not how often it has ever
	// deferred.
	DeferralCount int `json:"deferral_count,omitempty"`

	// LastDeferralReason is the hold/refusal text from the most recent
	// deferral, carried into the Mayor escalation mail once DeferralCount
	// crosses DefaultMaxDeferrals.
	LastDeferralReason string `json:"last_deferral_reason,omitempty"`

	// LastReceipt is the om editorial ReceiptSummary from the most recent
	// resubmit attempt, when this bead's re-dispatches are gated by
	// editorial convergence rather than a plain attempt count. Nil for
	// beads that have never had an editorial rejection (e.g. build/test
	// failure recoveries), and for the first editorial rejection.
	LastReceipt *ReceiptSummary `json:"last_receipt,omitempty"`
}

// ReceiptSummary is the minimal signal from an om editorial receipt the
// deacon needs to decide whether a redispatched resubmit is converging.
type ReceiptSummary struct {
	Score      float64  `json:"score"`
	Unresolved []string `json:"unresolved,omitempty"`
}

// Converging reports whether attempt cur trends toward a mergeable state
// relative to the prior attempt prev: no finding id carried forward from an
// earlier rejection is still open, AND the review score strictly increased.
// When false, reason names which condition failed, for use directly in
// escalation logs ("not converging: <reason> — escalating").
//
// cur.Unresolved is not this bead's raw finding list for the current
// attempt — it is om's own carry-forward classification (PriorFindings.
// Unresolved), computed upstream from the bead's full accumulated rejection
// history (every MERGE REJECTION note written so far), not just from prev.
// That is why this function reads prev.Score but never prev.Unresolved: the
// prev-vs-cur finding comparison the name implies already happened before
// this call, inside om.
//
// An identical finding set across two attempts is the canonical
// non-convergence case: cur.Unresolved is non-empty (those exact ids), so
// this returns false before the score is even compared.
func Converging(prev, cur ReceiptSummary) (ok bool, reason string) {
	if len(cur.Unresolved) > 0 {
		return false, "unresolved " + strings.Join(cur.Unresolved, ", ")
	}
	if cur.Score <= prev.Score {
		return false, "score did not rise"
	}
	return true, ""
}

// NeedsHumanLabel is applied to a bead when the deacon stops redispatching
// an om editorial resubmit — because it is not converging or the
// merge_queue.editorial.max_attempts cap was reached — instead of another
// automatic resubmit.
const NeedsHumanLabel = "needs_human"

// DefaultEditorialMaxAttempts mirrors config.EditorialConfig.WithDefaults'
// MaxAttempts default. Kept as a local constant (rather than importing
// internal/config here) to avoid a dependency edge from deacon to config;
// callers should prefer the resolved merge_queue.editorial.max_attempts.
const DefaultEditorialMaxAttempts = 5

// decideEditorialRedispatch is the pure decision gate behind RedispatchEditorial:
// given the resubmit's position (attemptCount so far, out of maxAttempts)
// and, when available, the receipt from the prior attempt, it decides
// whether to stop redispatching. Convergence is checked before the attempt
// cap so the escalation log names the actual defect (non-convergence) when
// both conditions hold at once, matching the spec's "Not converging, or
// attempt = max_attempts -> stop" (an OR, not solely a count check).
func decideEditorialRedispatch(attemptCount, maxAttempts int, lastReceipt *ReceiptSummary, cur ReceiptSummary) (stop bool, reason string) {
	if lastReceipt != nil {
		if ok, why := Converging(*lastReceipt, cur); !ok {
			return true, "not converging: " + why + " — escalating"
		}
	}
	if attemptCount >= maxAttempts {
		return true, fmt.Sprintf("max attempts (%d) reached — escalating", maxAttempts)
	}
	return false, ""
}

// ModelEscalationRule defines a single agent promotion rule.
type ModelEscalationRule struct {
	// FromAgent is the agent alias that triggers this rule (e.g., "claude-sonnet").
	FromAgent string `json:"from_agent"`

	// ToAgent is the agent alias to promote to (e.g., "claude" for Opus).
	ToAgent string `json:"to_agent"`

	// PromoteAfterFailures is the number of total failures (initial + re-dispatches)
	// required before promoting to ToAgent.
	PromoteAfterFailures int `json:"promote_after_failures"`

	// Comment is an optional human-readable description of this rule.
	Comment string `json:"comment,omitempty"`
}

// ModelEscalationConfig defines per-rig agent promotion rules for re-dispatch.
// Loaded from <rig>/refinery/rig/.gastown/model-escalation.json.
type ModelEscalationConfig struct {
	Type             string                `json:"type"`
	Version          int                   `json:"version"`
	Enabled          bool                  `json:"enabled"`
	Description      string                `json:"description,omitempty"`
	Rules            []ModelEscalationRule `json:"rules"`
	MaxTotalAttempts int                   `json:"max_total_attempts,omitempty"`
	Fallback         string                `json:"fallback,omitempty"`
}

// ModelEscalationConfigPath is the path within a rig project directory where
// the model escalation config is stored.
const ModelEscalationConfigPath = ".gastown/model-escalation.json"

// RedispatchResult describes the outcome of a re-dispatch attempt.
type RedispatchResult struct {
	BeadID    string `json:"bead_id"`
	Action    string `json:"action"` // "redispatched", "cooldown", "deferred", "escalated", "already-escalated", "skipped", "error"
	TargetRig string `json:"target_rig,omitempty"`
	Attempts  int    `json:"attempts"`
	Message   string `json:"message,omitempty"`
	Error     error  `json:"error,omitempty"`
}

// RedispatchStateFile returns the path to the re-dispatch state file.
func RedispatchStateFile(townRoot string) string {
	return filepath.Join(townRoot, "deacon", "redispatch-state.json")
}

// LoadRedispatchState loads the re-dispatch state from disk.
// Returns empty state if file doesn't exist.
func LoadRedispatchState(townRoot string) (*RedispatchState, error) {
	stateFile := RedispatchStateFile(townRoot)

	data, err := os.ReadFile(stateFile) //nolint:gosec // G304: path is constructed from trusted townRoot
	if err != nil {
		if os.IsNotExist(err) {
			return &RedispatchState{
				Beads: make(map[string]*BeadRedispatchState),
			}, nil
		}
		return nil, fmt.Errorf("reading redispatch state: %w", err)
	}

	var state RedispatchState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing redispatch state: %w", err)
	}

	if state.Beads == nil {
		state.Beads = make(map[string]*BeadRedispatchState)
	}

	return &state, nil
}

// SaveRedispatchState saves the re-dispatch state to disk.
func SaveRedispatchState(townRoot string, state *RedispatchState) error {
	stateFile := RedispatchStateFile(townRoot)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		return fmt.Errorf("creating deacon directory: %w", err)
	}

	state.LastUpdated = time.Now().UTC()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling redispatch state: %w", err)
	}

	return os.WriteFile(stateFile, data, 0600)
}

// GetBeadState returns the re-dispatch state for a bead, creating if needed.
func (s *RedispatchState) GetBeadState(beadID string) *BeadRedispatchState {
	if s.Beads == nil {
		s.Beads = make(map[string]*BeadRedispatchState)
	}

	state, ok := s.Beads[beadID]
	if !ok {
		state = &BeadRedispatchState{BeadID: beadID}
		s.Beads[beadID] = state
	}
	return state
}

// IsInCooldown returns true if the bead was recently re-dispatched.
func (s *BeadRedispatchState) IsInCooldown(cooldown time.Duration) bool {
	if s.LastAttemptTime.IsZero() {
		return false
	}
	return time.Since(s.LastAttemptTime) < cooldown
}

// CooldownRemaining returns how long until cooldown expires.
func (s *BeadRedispatchState) CooldownRemaining(cooldown time.Duration) time.Duration {
	if s.LastAttemptTime.IsZero() {
		return 0
	}
	remaining := cooldown - time.Since(s.LastAttemptTime)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ShouldEscalate returns true if the bead has exceeded the max re-dispatch attempts.
func (s *BeadRedispatchState) ShouldEscalate(maxAttempts int) bool {
	return s.AttemptCount >= maxAttempts
}

// RecordAttempt records a re-dispatch attempt for the bead. It also resets
// DeferralCount: an actual attempt (successful or failed) breaks whatever
// consecutive-deferral streak preceded it, so the next deferral starts a
// fresh count rather than immediately re-triggering the escalation this
// attempt already got past.
func (s *BeadRedispatchState) RecordAttempt(rig string) {
	s.AttemptCount++
	s.LastAttemptTime = time.Now().UTC()
	s.LastRig = rig
	s.DeferralCount = 0
}

// RecordDeferral records one more consecutive deferral and returns the
// updated streak length, for the caller to compare against
// DefaultMaxDeferrals.
func (s *BeadRedispatchState) RecordDeferral(reason string) int {
	s.DeferralCount++
	s.LastDeferralReason = reason
	return s.DeferralCount
}

// RecordEscalation records that the bead was escalated to Mayor.
func (s *BeadRedispatchState) RecordEscalation() {
	s.Escalated = true
	s.EscalatedAt = time.Now().UTC()
}

// RecoveredBeadRecord is what the RECOVERED_BEAD handler reads of a bead's own
// record before it decides what to do with the bead. It is a struct rather
// than a reader interface because both reads are made once, up front, by the
// caller that holds a beads store — the deacon package reaches beads only
// through the bd subprocess, and the dispatch-hold rule lives in convoy
// behind a store.
type RecoveredBeadRecord struct {
	// Notes is the bead's notes field. refinery.formatMergeRejectionNote
	// appends one MERGE REJECTION block per attempt there, ending in the
	// Score:/Unresolved: pair of an om review receipt when the rejection
	// came from an actual om verdict (om-gate T10). Kept verbatim because
	// RedispatchEditorial also attaches it to its escalation mail as the
	// finding history.
	Notes string

	// Hold is the reason this bead's own record keeps it off the dispatch
	// path (convoy.DispatchHoldReason): a deferred or pinned status, a
	// needs-sonnet or needs-mayor-review label, a MAYOR DESIGN DECISION or
	// do-not-redispatch in the bead's prose, or a record that could not be
	// read at all. Empty when the bead may be re-dispatched (gt-tq6l).
	Hold string
}

// RedispatchRecoveredBead is the deacon's RECOVERED_BEAD handler and the one
// decision point between a recovered bead and a fresh sling. It routes an
// editorial rejection — notes carrying an om review receipt — through
// RedispatchEditorial, whose convergence gate stops a resubmit that keeps
// failing the same findings, and everything else through the plain
// attempt-count Redispatch. A build/test rejection, a manual `gt mq reject`
// with no verdict behind it, and a bead with no rejection history at all all
// carry no receipt and take the plain path. Without this fork the convergence
// stop condition exists only on paper: the editorial path is reachable from
// nowhere else (gt-wuqn).
//
// Both engines gate on rec.Hold, so this only has to pick between them.
//
// Parameters are Redispatch's; maxAttempts is also the editorial cap when the
// bead routes editorially, which RedispatchEditorial resolves to
// DefaultEditorialMaxAttempts when it is 0.
func RedispatchRecoveredBead(rec RecoveredBeadRecord, townRoot, beadID, sourceRig string, maxAttempts int, cooldown time.Duration) *RedispatchResult {
	if cur, ok := ParseEditorialReceiptFromNotes(rec.Notes); ok {
		return RedispatchEditorial(rec, townRoot, beadID, sourceRig, maxAttempts, cooldown, cur)
	}
	return Redispatch(rec, townRoot, beadID, sourceRig, maxAttempts, cooldown)
}

// heldBeadResult returns the result for a bead its own record keeps off the
// dispatch path, or nil when the record holds nothing.
//
// Both entry points call it before anything else, because a hold outranks
// every other gate — the cooldown, the attempt count, and the editorial
// convergence rule. The deacon is also the one dispatcher that overrides a
// hold by design, re-slinging with --force, so it is the one place a hold
// recorded on the bead's own record would otherwise be overridden by the very
// loop the hold was written to stop (gt-tq6l).
func heldBeadResult(rec RecoveredBeadRecord, beadID string) *RedispatchResult {
	reason := strings.TrimSpace(rec.Hold)
	if reason == "" {
		return nil
	}
	return &RedispatchResult{
		BeadID:  beadID,
		Action:  "skipped",
		Message: "not re-dispatched: " + reason,
	}
}

// Redispatch handles a RECOVERED_BEAD message by re-slinging the bead to an
// available polecat, or escalating to Mayor if the bead has failed too many times.
//
// Parameters:
//   - rec: what the bead's own record said — a hold here skips the dispatch
//   - townRoot: the Gas Town workspace root
//   - beadID: the recovered bead to re-dispatch
//   - sourceRig: the rig from which the bead was recovered (empty = auto-detect from prefix)
//   - maxAttempts: max re-dispatches before escalating (0 = use default)
//   - cooldown: min time between re-dispatches (0 = use default)
func Redispatch(rec RecoveredBeadRecord, townRoot, beadID, sourceRig string, maxAttempts int, cooldown time.Duration) *RedispatchResult {
	if held := heldBeadResult(rec, beadID); held != nil {
		return held
	}

	result := &RedispatchResult{BeadID: beadID}

	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxRedispatches
	}
	if cooldown <= 0 {
		cooldown = DefaultRedispatchCooldown
	}

	// Load state
	state, err := LoadRedispatchState(townRoot)
	if err != nil {
		result.Action = "error"
		result.Error = fmt.Errorf("loading redispatch state: %w", err)
		return result
	}

	beadState := state.GetBeadState(beadID)
	result.Attempts = beadState.AttemptCount

	// Check if already escalated
	if beadState.Escalated {
		result.Action = "already-escalated"
		result.Message = fmt.Sprintf("bead already escalated to Mayor at %s", beadState.EscalatedAt.Format(time.RFC3339))
		return result
	}

	// Check cooldown
	if beadState.IsInCooldown(cooldown) {
		remaining := beadState.CooldownRemaining(cooldown)
		result.Action = "cooldown"
		result.Message = fmt.Sprintf("in cooldown (remaining: %s)", remaining.Round(time.Second))
		return result
	}

	// Check if we should escalate instead of re-dispatching
	if beadState.ShouldEscalate(maxAttempts) {
		result.Action = "escalated"
		result.Attempts = beadState.AttemptCount

		// Escalate to Mayor
		err := escalateToMayor(townRoot, beadID, beadState)
		if err != nil {
			result.Error = fmt.Errorf("escalating to mayor: %w", err)
			result.Message = fmt.Sprintf("failed to escalate after %d attempts: %v", beadState.AttemptCount, err)
		} else {
			beadState.RecordEscalation()
			result.Message = fmt.Sprintf("escalated to Mayor after %d failed re-dispatches", beadState.AttemptCount)
		}

		// Save state regardless of escalation success
		if saveErr := SaveRedispatchState(townRoot, state); saveErr != nil {
			// Log but don't fail - escalation mail was already sent
			result.Message += fmt.Sprintf(" (warning: state save failed: %v)", saveErr)
		}

		return result
	}

	return redispatchAttempt(townRoot, beadID, sourceRig, maxAttempts, state, beadState, nil)
}

// redispatchAttempt is the re-dispatch tail shared by Redispatch and
// RedispatchEditorial once the escalation/cooldown gate has already cleared:
// resolve the target rig, verify the bead is still open, resolve any
// model-escalation agent override, sling the bead, and persist state.
//
// onDispatched, when non-nil, is called only after a SUCCESSFUL sling — it
// exists so RedispatchEditorial can record this attempt's receipt as
// beadState.LastReceipt for the next call's convergence check. Calling it on
// a FAILED sling too was the bug: a failed sling makes no new attempt with a
// new outcome, so the next retry would still be judging the same rejection —
// but with LastReceipt now equal to cur, Converging compares that rejection
// to itself (cur.Score <= prev.Score, since they're equal) and reads it as
// non-converging, forcing an immediate false escalation on a transient sling
// failure (gt-j6ez).
func redispatchAttempt(townRoot, beadID, sourceRig string, maxAttempts int, state *RedispatchState, beadState *BeadRedispatchState, onDispatched func()) *RedispatchResult {
	result := &RedispatchResult{BeadID: beadID, Attempts: beadState.AttemptCount}

	// The operator's town-wide hold outranks the re-sling: this path passes
	// --force, so nothing downstream would stop it (gt-ifijm, where gt-elvf4
	// was force-slung during a hold). It is checked here, at the one tail both
	// engines share, so an escalation the attempt count already earned still
	// reaches the mayor — escalating is not dispatching. No attempt is
	// recorded: the bead did not fail, the town is paused, and it stays open
	// for whoever dispatches after the hold lifts.
	if reason := dispatch.OperatorHold(townRoot); reason != "" {
		return deferBead(townRoot, beadID, state, beadState, "not re-dispatched: "+reason, reason)
	}

	// Determine target rig
	targetRig := sourceRig
	if targetRig == "" {
		targetRig = resolveRigFromBead(townRoot, beadID)
	}
	if targetRig == "" {
		result.Action = "error"
		result.Error = fmt.Errorf("cannot determine target rig for bead %s", beadID)
		return result
	}
	result.TargetRig = targetRig

	// A per-rig ESTOP on the target rig defers the same way the town hold
	// does: no attempt, no cooldown (gt-ifijm).
	if reason := dispatch.RigHold(townRoot, targetRig); reason != "" {
		return deferBead(townRoot, beadID, state, beadState, "not re-dispatched: "+reason, reason)
	}

	// Verify bead is still open (not already claimed or closed).
	// Only proceed when status is explicitly "open". Empty status (query
	// failure) is treated as "not open" to avoid re-dispatching closed
	// beads when bd show fails. (gt-sy8)
	beadStatus := getBeadStatusForRedispatch(townRoot, beadID)
	if beadStatus != "open" {
		result.Action = "skipped"
		if beadStatus == "" {
			result.Message = "could not determine bead status (treating as not open)"
		} else {
			result.Message = fmt.Sprintf("bead status is %q (expected open)", beadStatus)
		}
		return result
	}

	// Determine agent override from model escalation config (if any).
	escalationAgent := resolveAgentForRedispatch(townRoot, targetRig, beadState)

	// Re-dispatch via gt sling
	if refusal, err := slingBead(townRoot, beadID, targetRig, escalationAgent); err != nil {
		// A capacity refusal (full polecat pool) is the town queueing, not
		// the bead failing: no attempt is recorded and no cooldown starts, so
		// the retry budget and the REDISPATCH_FAILED escalation stay reserved
		// for real failures. The bead is left open and ready (gt-xdaq).
		if refusal != "" {
			return deferBead(townRoot, beadID, state, beadState, "not re-dispatched, retry later: "+refusal, refusal)
		}
		result.Action = "error"
		result.Error = fmt.Errorf("slinging bead to %s: %w", targetRig, err)

		// Record the failed attempt — but not onDispatched; see doc comment.
		beadState.LastAgent = escalationAgent
		beadState.RecordAttempt(targetRig)
		_ = SaveRedispatchState(townRoot, state)

		return result
	}

	// Record successful dispatch
	beadState.LastAgent = escalationAgent
	beadState.RecordAttempt(targetRig)
	if onDispatched != nil {
		onDispatched()
	}
	result.Action = "redispatched"
	result.Attempts = beadState.AttemptCount
	if escalationAgent != "" {
		result.Message = fmt.Sprintf("re-dispatched to %s with agent %q (attempt %d/%d)", targetRig, escalationAgent, beadState.AttemptCount, maxAttempts)
	} else {
		result.Message = fmt.Sprintf("re-dispatched to %s (attempt %d/%d)", targetRig, beadState.AttemptCount, maxAttempts)
	}

	// Save state
	if saveErr := SaveRedispatchState(townRoot, state); saveErr != nil {
		result.Message += fmt.Sprintf(" (warning: state save failed: %v)", saveErr)
	}

	return result
}

// deferBead is the outcome for all three deferral sites in redispatchAttempt
// (operator hold, rig hold, pool-full sling refusal). It records the
// consecutive-deferral streak and, once that streak reaches
// DefaultMaxDeferrals, escalates to Mayor instead of deferring again —
// otherwise a PERSISTENT deferral condition (a saturated pool, a standing
// hold) would defer this bead every cycle forever, since a deferral never
// advances AttemptCount and so never reaches the --max-attempts escalation
// either (gt-oo494).
//
// message is the plain-deferral wording the caller already built (it varies
// per site: "not re-dispatched: <hold>" vs "not re-dispatched, retry later:
// <refusal>"); reason is the same fact bare, for the escalation mail body.
func deferBead(townRoot, beadID string, state *RedispatchState, beadState *BeadRedispatchState, message, reason string) *RedispatchResult {
	result := &RedispatchResult{BeadID: beadID, Attempts: beadState.AttemptCount}

	count := beadState.RecordDeferral(reason)
	if count < DefaultMaxDeferrals {
		result.Action = "deferred"
		result.Message = message
		if saveErr := SaveRedispatchState(townRoot, state); saveErr != nil {
			result.Message += fmt.Sprintf(" (warning: state save failed: %v)", saveErr)
		}
		return result
	}

	result.Action = "escalated"
	result.Message = fmt.Sprintf("%s (escalating after %d consecutive deferrals)", message, count)
	if err := escalateDeferralToMayor(townRoot, beadID, reason, count, beadState); err != nil {
		result.Error = fmt.Errorf("escalating to mayor: %w", err)
		result.Message += fmt.Sprintf(" (warning: escalation mail failed: %v)", err)
	} else {
		beadState.RecordEscalation()
	}
	if saveErr := SaveRedispatchState(townRoot, state); saveErr != nil {
		result.Message += fmt.Sprintf(" (warning: state save failed: %v)", saveErr)
	}
	return result
}

// escalateDeferralToMayor sends the REDISPATCH_FAILED mail for a bead stuck
// behind DefaultMaxDeferrals consecutive deferrals. It deliberately does not
// reuse escalateToMayor's wording ("keeps failing") — the bead here was never
// actually slung, so the message names the real blocker: a recurring
// deferral condition, not a bead defect.
func escalateDeferralToMayor(townRoot, beadID, reason string, deferralCount int, beadState *BeadRedispatchState) error {
	subject := fmt.Sprintf("REDISPATCH_FAILED: %s (%d consecutive deferrals)", beadID, deferralCount)
	body := fmt.Sprintf(`Bead %s has been deferred %d times in a row without ever being re-dispatched.

Bead: %s
Consecutive deferrals: %d
Redispatch attempts so far: %d
Last deferral reason: %s

This is not a bead failure — the bead never got a seat. The recurring
deferral reason above (a saturated pool or a standing dispatch hold) is the
actual blocker, and it will keep recurring until that condition clears.
Please investigate and either:
1. Free capacity (raise polecat_pool.max_local/max_overflow) or lift the hold
2. Re-sling the bead manually once the condition clears
3. Close/deprioritize the bead if it's no longer actionable`,
		beadID, deferralCount,
		beadID, deferralCount, beadState.AttemptCount, reason,
	)

	cmd := exec.Command("gt", "mail", "send", "mayor/", "-s", subject, "-m", body)
	cmd.Dir = townRoot
	cmd.Env = deaconMutationRoutingEnv(townRoot)
	util.SetDetachedProcessGroup(cmd)
	return cmd.Run()
}

// RedispatchEditorial handles a RECOVERED_BEAD message whose rejection came
// from an om editorial review, gating on convergence in addition to the
// plain attempt count that Redispatch enforces (spec: "Convergence (decided
// by the deacon from the receipt alone) ... Not converging, or attempt =
// max_attempts -> stop, label needs_human, escalate with the finding
// history").
//
//   - rec: what the bead's own record said. rec.Notes is the full finding
//     history (every MERGE REJECTION block the bead accumulated) attached to
//     the needs_human escalation mail so a human picks up with full context;
//     a hold in rec skips the dispatch before the convergence rule runs.
//   - maxAttempts: the rig's resolved merge_queue.editorial.max_attempts
//     (0 = DefaultEditorialMaxAttempts).
//   - cur: the receipt summary (score, unresolved finding ids) from the
//     rejection that triggered this call.
func RedispatchEditorial(rec RecoveredBeadRecord, townRoot, beadID, sourceRig string, maxAttempts int, cooldown time.Duration, cur ReceiptSummary) *RedispatchResult {
	if held := heldBeadResult(rec, beadID); held != nil {
		return held
	}

	findingHistory := rec.Notes
	result := &RedispatchResult{BeadID: beadID}

	if maxAttempts <= 0 {
		maxAttempts = DefaultEditorialMaxAttempts
	}
	if cooldown <= 0 {
		cooldown = DefaultRedispatchCooldown
	}

	state, err := LoadRedispatchState(townRoot)
	if err != nil {
		result.Action = "error"
		result.Error = fmt.Errorf("loading redispatch state: %w", err)
		return result
	}

	beadState := state.GetBeadState(beadID)
	result.Attempts = beadState.AttemptCount

	if beadState.Escalated {
		result.Action = "already-escalated"
		result.Message = fmt.Sprintf("bead already escalated to Mayor at %s", beadState.EscalatedAt.Format(time.RFC3339))
		return result
	}
	if beadState.IsInCooldown(cooldown) {
		remaining := beadState.CooldownRemaining(cooldown)
		result.Action = "cooldown"
		result.Message = fmt.Sprintf("in cooldown (remaining: %s)", remaining.Round(time.Second))
		return result
	}

	if stop, reason := decideEditorialRedispatch(beadState.AttemptCount, maxAttempts, beadState.LastReceipt, cur); stop {
		result.Action = "escalated"
		result.Message = reason

		if err := labelNeedsHuman(townRoot, beadID); err != nil {
			result.Message += fmt.Sprintf(" (warning: failed to label %s: %v)", NeedsHumanLabel, err)
		}
		if err := escalateEditorialToMayor(townRoot, beadID, reason, findingHistory, beadState); err != nil {
			result.Error = fmt.Errorf("escalating to mayor: %w", err)
			result.Message += fmt.Sprintf(" (warning: escalation mail failed: %v)", err)
		} else {
			beadState.RecordEscalation()
		}

		if saveErr := SaveRedispatchState(townRoot, state); saveErr != nil {
			result.Message += fmt.Sprintf(" (warning: state save failed: %v)", saveErr)
		}
		return result
	}

	// Converging (or no prior receipt yet) and under the attempt cap:
	// proceed exactly like the generic Redispatch, then remember this
	// attempt's receipt — but only once the sling actually succeeds, so a
	// transient sling failure doesn't make the next retry compare this
	// rejection's receipt against itself (see redispatchAttempt's doc
	// comment).
	receipt := cur
	return redispatchAttempt(townRoot, beadID, sourceRig, maxAttempts, state, beadState, func() {
		beadState.LastReceipt = &receipt
	})
}

// labelNeedsHuman applies NeedsHumanLabel to a bead so it surfaces for
// human triage instead of further automatic resubmits.
func labelNeedsHuman(townRoot, beadID string) error {
	cmd := beads.Command(townRoot, townBeadsDir(townRoot), beads.MutationRouting, "update", beadID, "--add-label", NeedsHumanLabel)
	cmd.Dir = townRoot
	return cmd.Run()
}

// escalateEditorialToMayor sends the needs_human escalation mail, including
// the full finding history so a human doesn't have to reconstruct it from
// bead notes.
func escalateEditorialToMayor(townRoot, beadID, reason, findingHistory string, beadState *BeadRedispatchState) error {
	subject := fmt.Sprintf("REDISPATCH_FAILED: %s (needs_human)", beadID)
	body := fmt.Sprintf(`Bead %s stopped resubmitting under om editorial review: %s

Bead: %s
Attempts: %d
Last Rig: %s

Finding history:
%s

This bead has been labeled %s. Please investigate and either:
1. Fix the underlying issue and re-sling manually
2. Close/deprioritize the bead if it's not actionable
3. Adjust merge_queue.editorial.max_attempts if attempts were legitimately still improving`,
		beadID, reason,
		beadID, beadState.AttemptCount, beadState.LastRig,
		findingHistory,
		NeedsHumanLabel,
	)

	cmd := exec.Command("gt", "mail", "send", "mayor/", "-s", subject, "-m", body)
	cmd.Dir = townRoot
	cmd.Env = deaconMutationRoutingEnv(townRoot)
	util.SetDetachedProcessGroup(cmd)
	return cmd.Run()
}

// PruneRedispatchState removes entries for beads that are no longer open.
// Call periodically to prevent unbounded state growth.
func PruneRedispatchState(townRoot string) (int, error) {
	state, err := LoadRedispatchState(townRoot)
	if err != nil {
		return 0, err
	}

	pruned := 0
	for beadID := range state.Beads {
		status := getBeadStatusForRedispatch(townRoot, beadID)
		// Remove entries for beads that are closed, or that we can't find
		if status == "closed" || status == "" {
			delete(state.Beads, beadID)
			pruned++
		}
	}

	if pruned > 0 {
		if err := SaveRedispatchState(townRoot, state); err != nil {
			return pruned, err
		}
	}

	return pruned, nil
}

// resolveRigFromBead determines the rig that owns a bead based on its prefix.
func resolveRigFromBead(townRoot, beadID string) string {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return ""
	}
	return beads.GetRigNameForPrefix(townRoot, prefix)
}

// getBeadStatusForRedispatch returns the current status of a bead.
func getBeadStatusForRedispatch(townRoot, beadID string) string {
	cmd := beads.Command(townRoot, townBeadsDir(townRoot), beads.ReadOnlyRouting, "show", beadID, "--json")

	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	var issues []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &issues); err != nil || len(issues) == 0 {
		return ""
	}
	return issues[0].Status
}

// GetBeadNotesForRedispatch fetches a bead's notes field, for callers (`gt
// deacon redispatch`) that need to recover the om editorial receipt
// formatMergeRejectionNote wrote onto it (om-gate T10), so
// ParseEditorialReceiptFromNotes can hand RedispatchEditorial's
// convergence rule real data instead of the deacon inferring it from mail
// prose by hand. Returns "" on any lookup failure (bd unreachable, bead not
// found) — the caller falls back to the plain Redispatch in that case.
func GetBeadNotesForRedispatch(townRoot, beadID string) string {
	cmd := beads.Command(townRoot, townBeadsDir(townRoot), beads.ReadOnlyRouting, "show", beadID, "--json")

	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	var issues []struct {
		Notes string `json:"notes"`
	}
	if err := json.Unmarshal(output, &issues); err != nil || len(issues) == 0 {
		return ""
	}
	return issues[0].Notes
}

// editorialScoreLineRE and editorialUnresolvedLineRE match the "Score:" and
// "Unresolved:" lines formatMergeRejectionNote appends to a source bead's
// notes for an editorial rejection carrying an EditorialReceipt
// (refinery.deadWorkerRecoveryRequest.Receipt).
var (
	editorialScoreLineRE      = regexp.MustCompile(`(?m)^Score:\s*(\S+)\s*$`)
	editorialUnresolvedLineRE = regexp.MustCompile(`(?m)^Unresolved:\s*(.+?)\s*$`)
)

// ParseEditorialReceiptFromNotes recovers the om review receipt (score,
// carried-forward unresolved finding ids) that an editorial merge rejection
// wrote onto the source bead's notes, so `gt deacon redispatch` can call
// RedispatchEditorial with real data instead of falling back to the plain
// attempt-count Redispatch. ok is false when notes carries no Score line —
// a build/test rejection, a manual `gt mq reject` with no om verdict behind
// it, or a bead with no rejection history at all.
//
// Notes accumulate one MERGE REJECTION block per attempt (append-only), so
// this takes the LAST Score/Unresolved line in the text — the most recent
// rejection, which is the one that triggered this redispatch.
func ParseEditorialReceiptFromNotes(notes string) (cur ReceiptSummary, ok bool) {
	scoreMatches := editorialScoreLineRE.FindAllStringSubmatch(notes, -1)
	if len(scoreMatches) == 0 {
		return ReceiptSummary{}, false
	}
	last := scoreMatches[len(scoreMatches)-1]
	score, err := strconv.ParseFloat(last[1], 64)
	if err != nil {
		return ReceiptSummary{}, false
	}
	cur.Score = score

	if unresolvedMatches := editorialUnresolvedLineRE.FindAllStringSubmatch(notes, -1); len(unresolvedMatches) > 0 {
		last := unresolvedMatches[len(unresolvedMatches)-1]
		for _, id := range strings.Split(last[1], ",") {
			if id = strings.TrimSpace(id); id != "" {
				cur.Unresolved = append(cur.Unresolved, id)
			}
		}
	}
	return cur, true
}

// LoadModelEscalationConfig loads model escalation rules from a rig project directory.
// Returns nil, nil if the file does not exist (no escalation configured).
func LoadModelEscalationConfig(rigProjectDir string) (*ModelEscalationConfig, error) {
	path := filepath.Join(rigProjectDir, ModelEscalationConfigPath)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed from trusted rigProjectDir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading model escalation config: %w", err)
	}

	var cfg ModelEscalationConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing model escalation config %s: %w", path, err)
	}
	return &cfg, nil
}

// resolveAgentForRedispatch determines which agent alias to use when re-dispatching
// a bead, based on the rig's model escalation config.
//
// The "total failures" seen by this bead is beadState.AttemptCount (prior re-dispatches)
// plus 1 (the original dispatch failure that triggered this re-dispatch).
// Rules fire when totalFailures >= rule.PromoteAfterFailures.
//
// Returns "" (empty) if no escalation is configured or no rule applies — the caller
// should omit the --agent flag and let the rig use its default.
func resolveAgentForRedispatch(townRoot, targetRig string, beadState *BeadRedispatchState) string {
	// Rig project dir is <townRoot>/<rig>/refinery/rig
	rigProjectDir := filepath.Join(townRoot, targetRig, "refinery", "rig")

	cfg, err := LoadModelEscalationConfig(rigProjectDir)
	if err != nil || cfg == nil || !cfg.Enabled || len(cfg.Rules) == 0 {
		return ""
	}

	// Total failures = prior re-dispatch attempts + 1 (initial failure that triggered
	// this re-dispatch call). AttemptCount is recorded AFTER each sling, so on entry
	// it reflects the number of completed re-dispatches, not the current one.
	totalFailures := beadState.AttemptCount + 1

	for _, rule := range cfg.Rules {
		if totalFailures >= rule.PromoteAfterFailures {
			return rule.ToAgent
		}
	}
	return ""
}

// slingBead dispatches a bead to a rig via gt sling.
// If agent is non-empty, passes --agent <agent> to override the rig's default.
//
// When the sling fails with a capacity refusal (dispatch.SlingRefusalMarker),
// refusal carries the guard's sentence so the caller can defer rather than
// count a failure; it is "" for every other outcome. Stderr is still streamed
// to the deacon's own stderr as before.
func slingBead(townRoot, beadID, rig, agent string) (refusal string, err error) {
	args := []string{"sling", beadID, rig, "--force", "--no-convoy"}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	cmd := exec.Command("gt", args...)
	cmd.Dir = townRoot
	cmd.Env = deaconMutationRoutingEnv(townRoot)
	util.SetDetachedProcessGroup(cmd)
	cmd.Stdout = os.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		reason, _ := dispatch.SlingRefusalReason(stderr.String())
		return reason, err
	}
	return "", nil
}

// escalateToMayor sends an escalation mail to the Mayor about a repeatedly-failing bead.
func escalateToMayor(townRoot, beadID string, beadState *BeadRedispatchState) error {
	subject := fmt.Sprintf("REDISPATCH_FAILED: %s (%d attempts)", beadID, beadState.AttemptCount)
	body := fmt.Sprintf(`Bead %s has been recovered and re-dispatched %d times but keeps failing.

Bead: %s
Attempts: %d
Last Rig: %s
Last Attempt: %s

This bead may have a systemic issue (e.g., causes polecat crashes).
Please investigate and either:
1. Fix the underlying issue and re-sling manually
2. Close/deprioritize the bead if it's not actionable
3. Increase the re-dispatch limit if the failures were transient`,
		beadID,
		beadState.AttemptCount,
		beadID,
		beadState.AttemptCount,
		beadState.LastRig,
		beadState.LastAttemptTime.Format(time.RFC3339),
	)

	cmd := exec.Command("gt", "mail", "send", "mayor/", "-s", subject, "-m", body)
	cmd.Dir = townRoot
	cmd.Env = deaconMutationRoutingEnv(townRoot)
	util.SetDetachedProcessGroup(cmd)
	return cmd.Run()
}

// ParseRecoveredBeadSubject extracts the bead ID from a RECOVERED_BEAD mail subject.
// Expected format: "RECOVERED_BEAD <bead-id>"
func ParseRecoveredBeadSubject(subject string) (beadID string, ok bool) {
	const prefix = "RECOVERED_BEAD "
	if !strings.HasPrefix(subject, prefix) {
		return "", false
	}
	beadID = strings.TrimSpace(strings.TrimPrefix(subject, prefix))
	if beadID == "" {
		return "", false
	}
	return beadID, true
}

// ParseRecoveredBeadBody extracts the source rig from a RECOVERED_BEAD mail body.
// Looks for "Polecat: <rig>/<name>" line.
func ParseRecoveredBeadBody(body string) (rig string) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Polecat:") {
			polecatAddr := strings.TrimSpace(strings.TrimPrefix(line, "Polecat:"))
			parts := strings.SplitN(polecatAddr, "/", 2)
			if len(parts) >= 1 {
				return parts[0]
			}
		}
	}
	return ""
}
