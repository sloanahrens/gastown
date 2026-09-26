package witness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/channelevents"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/mayor"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/workspace"
)

// HungSessionThresholdMinutes is the number of minutes of tmux inactivity
// after which a live agent session is considered hung. Derived from
// constants.HungSessionThreshold (single source of truth).
var HungSessionThresholdMinutes = int(constants.HungSessionThreshold.Minutes())

// initRegistryFromWorkDir initializes the session prefix and agent registries
// from a work directory. This ensures session.PrefixFor(rigName) returns the
// correct rig prefix (e.g., "tr" for testrig) instead of the default "gt",
// and that user-configured agent overrides (e.g., custom process_names) are
// loaded for liveness checks.
func initRegistryFromWorkDir(workDir string) {
	if townRoot, err := workspace.Find(workDir); err == nil && townRoot != "" {
		initRegistryFromTownRoot(townRoot)
	}
}

// workDirToTownRoot resolves a workDir to the Gas Town root directory.
// Falls back to workDir itself if workspace.Find fails.
func workDirToTownRoot(workDir string) string {
	if townRoot, err := workspace.Find(workDir); err == nil && townRoot != "" {
		return townRoot
	}
	return workDir
}

// registryMu serializes calls to initRegistryFromTownRoot so that concurrent
// callers (including parallel tests) don't race on the global registries.
var registryMu sync.Mutex

// BdCli wraps bd CLI execution for dependency injection.
// Production code uses DefaultBdCli(); tests provide mock implementations
// to avoid spawning subprocesses and eliminate global mutable state.
type BdCli struct {
	Exec func(workDir string, args ...string) (string, error)
	Run  func(workDir string, args ...string) error
}

// DefaultBdCli returns a BdCli that shells out to the real bd binary.
func DefaultBdCli() *BdCli {
	return &BdCli{
		Exec: func(workDir string, args ...string) (string, error) {
			// bd v0.59+ requires --flat for list --json to produce JSON
			args = beads.InjectFlatForListJSON(args)
			return defaultBDExecWithOutput(workDir, args...)
		},
		Run: func(workDir string, args ...string) error {
			args = beads.InjectFlatForListJSON(args)
			return defaultBDRun(workDir, args...)
		},
	}
}

func defaultBDExecWithOutput(workDir string, args ...string) (string, error) {
	cmd := beads.Command(workDir, beads.ResolveBeadsDir(workDir), beads.SubprocessModeForArgs(args), args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return "", fmt.Errorf("%s", errMsg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func defaultBDRun(workDir string, args ...string) error {
	cmd := beads.Command(workDir, beads.ResolveBeadsDir(workDir), beads.SubprocessModeForArgs(args), args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
		return err
	}
	return nil
}

// initRegistryFromTownRoot initializes registries from a known town root,
// logging any errors so that misconfiguration is observable.
func initRegistryFromTownRoot(townRoot string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if err := session.InitRegistry(townRoot); err != nil {
		fmt.Fprintf(os.Stderr, "witness: failed to initialize town registry: %v\n", err)
	}
}

// HandlerResult tracks the result of handling a protocol message.
type HandlerResult struct {
	MessageID     string
	ProtocolType  ProtocolType
	Handled       bool
	Action        string
	CleanupStatus string // Observed cleanup_status (ZFC: report data, agent decides policy)
	WispCreated   string // ID of created wisp (if any)
	MailSent      string // Deprecated: was ID of sent mail. Notifications now use nudge.
	Error         error
}

// HandlePolecatDone processes a POLECAT_DONE message from a polecat.
// For PHASE_COMPLETE exits, recycles the polecat (session ends, worktree kept).
// For exits with pending MR, creates cleanup wisp and sends MERGE_READY to Refinery.
// For exits without MR, acknowledges completion (polecat stays in "done").
//
// When a pending MR exists, sends MERGE_READY to the Refinery to trigger
// immediate merge queue processing. This ensures work flows through the system
// without waiting for the daemon's heartbeat cycle.
//
// Persistent Polecat Model (gt-4ac):
// Polecats persist after work completion - sandbox is preserved for reuse.
// `gt done` sets agent_state=done and exits (gt-ho4f: there is no done->idle
// transition — "done" is the canonical resting state; the allocator and
// workstate classifier both treat it as reuse-eligible, see
// polecat.State.IsReuseEligible).
// The MR lifecycle continues independently in the Refinery.
// If conflicts arise, Refinery creates a conflict-resolution task for an available polecat.
func HandlePolecatDone(bd *BdCli, workDir, rigName string, msg *mail.Message, router *mail.Router) *HandlerResult {
	result := &HandlerResult{
		MessageID:    msg.ID,
		ProtocolType: ProtoPolecatDone,
	}

	payload, err := ParsePolecatDone(msg.Subject, msg.Body)
	if err != nil {
		result.Error = fmt.Errorf("parsing POLECAT_DONE: %w", err)
		return result
	}

	if stale, reason := isStalePolecatDone(workDir, rigName, payload.PolecatName, msg); stale {
		result.Handled = true
		result.Action = fmt.Sprintf("ignored stale POLECAT_DONE for %s (%s)", payload.PolecatName, reason)
		return result
	}

	if payload.Exit == "PHASE_COMPLETE" {
		result.Handled = true
		result.Action = fmt.Sprintf("phase-complete for %s (gate=%s) - session recycled, awaiting gate", payload.PolecatName, payload.Gate)
		return result
	}

	hasPendingMR := completionPayloadHasPendingMR(bd, workDir, rigName, payload)

	// When Exit==COMPLETED but MRID is empty and MR creation didn't explicitly
	// fail, query beads to check if an MR bead exists for this branch.
	// This handles the case where the MR was created but the ID wasn't included
	// in the POLECAT_DONE message (e.g., message truncation, race condition).
	if !hasPendingMR && payload.Exit == "COMPLETED" && !payload.MRFailed && payload.Branch != "" {
		if mrID := findMRBeadForBranch(bd, workDir, payload.Branch); mrID != "" {
			payload.MRID = mrID
			hasPendingMR = completionPayloadHasPendingMR(bd, workDir, rigName, payload)
		}
	}

	if hasPendingMR {
		result = handlePolecatDonePendingMR(bd, workDir, rigName, payload, result)
	} else {
		result = handlePolecatDoneNoMR(workDir, rigName, payload, result)
	}

	// Notify Mayor that a slot is open regardless of MR status.
	// The polecat is idle either way — Mayor should consider slinging next bead. (GH#2727)
	if result.Handled {
		notifyMayorSlotOpen(workDir, rigName, payload.PolecatName, payload.Exit)
	}

	return result
}

// HandlePolecatDoneFromBead processes polecat completion detected from agent bead
// state (gt-a6gp: nudge-over-mail). Instead of parsing a POLECAT_DONE mail message,
// this reads completion metadata directly from the agent bead's description fields
// (exit_type, mr_id, branch, mr_failed, completion_time).
//
// Self-managed completion (gt-1qlg): Polecats now set agent_state=idle directly,
// so the witness rarely sees agent_state=done. This function is retained as a
// safety net for crash recovery — if a polecat crashes between setting completion
// metadata and transitioning to idle, the witness can process the completion.
//
// The processing logic is identical to HandlePolecatDone: pending MR triggers
// cleanup wisp + MERGE_READY; no MR means simple acknowledgment.
func HandlePolecatDoneFromBead(bd *BdCli, workDir, rigName, polecatName string, fields *beads.AgentFields, router *mail.Router) *HandlerResult {
	result := &HandlerResult{
		ProtocolType: ProtoPolecatDone,
	}

	if fields == nil {
		result.Error = fmt.Errorf("nil agent fields for polecat %s", polecatName)
		return result
	}
	sourceIssue := fields.LastSourceIssue
	if sourceIssue == "" {
		sourceIssue = fields.HookBead
	}

	// Map agent bead fields to the existing PolecatDonePayload for reuse
	payload := &PolecatDonePayload{
		PolecatName: polecatName,
		Exit:        fields.ExitType,
		IssueID:     sourceIssue,
		MRID:        fields.MRID,
		Branch:      fields.Branch,
		MRFailed:    fields.MRFailed,
		PushFailed:  fields.PushFailed,
	}

	if payload.Exit == "PHASE_COMPLETE" {
		result.Handled = true
		result.Action = fmt.Sprintf("phase-complete for %s - session recycled, awaiting gate", polecatName)
		return result
	}

	// Push failed: branch never reached origin (gas-556). Report recovery needed.
	if payload.PushFailed {
		result.Handled = true
		result.Action = fmt.Sprintf("push-failed-recovery-needed for %s (branch=%s issue=%s) — branch not on origin, worktree may be at risk",
			polecatName, payload.Branch, payload.IssueID)
		townRoot, _ := workspace.Find(workDir)
		if townRoot != "" {
			mayorMsg := fmt.Sprintf("PUSH_FAILED: polecat=%s branch=%s issue=%s — branch not on origin, possible work loss",
				polecatName, payload.Branch, payload.IssueID)
			mayorSession := session.MayorSessionName()
			t := tmux.NewTmux()
			if running, err := t.HasSession(mayorSession); err == nil && running {
				_ = t.NudgeSession(mayorSession, mayorMsg)
			}
		}
		return result
	}

	hasPendingMR := completionPayloadHasPendingMR(bd, workDir, rigName, payload)

	// Same MR-discovery fallback as HandlePolecatDone
	if !hasPendingMR && payload.Exit == "COMPLETED" && !payload.MRFailed && payload.Branch != "" {
		if mrID := findMRBeadForBranch(bd, workDir, payload.Branch); mrID != "" {
			payload.MRID = mrID
			hasPendingMR = completionPayloadHasPendingMR(bd, workDir, rigName, payload)
		}
	}

	if hasPendingMR {
		result = handlePolecatDonePendingMR(bd, workDir, rigName, payload, result)
	} else {
		result = handlePolecatDoneNoMR(workDir, rigName, payload, result)
	}

	// Notify Mayor that a slot is open regardless of MR status.
	// Mirror HandlePolecatDone behavior — polecat is idle, Mayor should sling next bead. (GH#2727)
	if result.Handled {
		notifyMayorSlotOpen(workDir, rigName, polecatName, payload.Exit)
	}

	return result
}

func completionPayloadHasPendingMR(bd *BdCli, workDir, rigName string, payload *PolecatDonePayload) bool {
	if payload == nil || payload.MRID == "" {
		return false
	}
	assessment := polecat.AssessActiveMR(beadCLIShower{bd: bd, workDir: workDir}, polecat.ActiveMRInput{
		ActiveMR:        payload.MRID,
		SourceIssueHint: payload.IssueID,
		RequireGitSafe:  true,
		GitSafe:         activeMRGitSafe(workDir, rigName, payload.PolecatName),
	})
	return assessment.Pending
}

// handlePolecatDonePendingMR handles a POLECAT_DONE when there's a pending MR.
// Creates a cleanup wisp, sends MERGE_READY to the Refinery, and nudges it.
func handlePolecatDonePendingMR(bd *BdCli, workDir, rigName string, payload *PolecatDonePayload, result *HandlerResult) *HandlerResult {
	wispID, err := createCleanupWisp(bd, workDir, rigName, payload.PolecatName, payload.IssueID, payload.Branch)
	if err != nil {
		result.Error = fmt.Errorf("creating cleanup wisp: %w", err)
		return result
	}

	if err := UpdateCleanupWispState(bd, workDir, wispID, "merge-requested"); err != nil {
		result.Error = fmt.Errorf("updating wisp state: %w", err)
		return result
	}

	notifyRefineryMergeReady(workDir, rigName, result)

	result.Handled = true
	result.WispCreated = wispID
	result.Action = fmt.Sprintf("deferred cleanup for %s (pending MR=%s, nudged refinery)", payload.PolecatName, payload.MRID)
	return result
}

// notifyRefineryMergeReady emits a MERGE_READY channel event and nudges the
// Refinery to check the merge queue. The channel event unblocks the refinery's
// await-event loop instantly; the tmux nudge is a belt-and-suspenders fallback
// for when the refinery is at the Claude prompt rather than in await-event.
// Errors are non-fatal (Refinery will still pick up work on next patrol cycle).
func notifyRefineryMergeReady(workDir, rigName string, result *HandlerResult) {
	townRoot, _ := workspace.Find(workDir)
	// Emit file-based event so refinery's await-event unblocks instantly.
	if townRoot != "" {
		_, _ = channelevents.EmitToTown(townRoot, "refinery", rigName, "MERGE_READY", []string{
			"source=witness",
			"rig=" + rigName,
		})
	}
	if nudgeErr := nudgeRefinery(townRoot, rigName); nudgeErr != nil {
		if result.Error == nil {
			result.Error = fmt.Errorf("nudging refinery: %w (non-fatal)", nudgeErr)
		}
	}
}

// handlePolecatDoneNoMR handles a POLECAT_DONE with no pending MR.
// Tries auto-nuke; falls back to creating a cleanup wisp for manual intervention.
func handlePolecatDoneNoMR(_, _ string, payload *PolecatDonePayload, result *HandlerResult) *HandlerResult {
	// Persistent polecat model (gt-4ac): polecats stay in "done" after completion,
	// no nuke. `gt done` already set agent_state=done (gt-ho4f: there is no
	// done->idle transition; "done" is the reuse-eligible resting state).
	// We just acknowledge the completion here.
	result.Handled = true
	result.Action = fmt.Sprintf("polecat %s completed (exit=%s, no MR) — sandbox preserved", payload.PolecatName, payload.Exit)
	return result
}

func isStalePolecatDone(workDir, rigName, polecatName string, msg *mail.Message) (bool, string) {
	if msg == nil {
		return false, ""
	}

	initRegistryFromWorkDir(workDir)
	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
	createdAt, err := session.SessionCreatedAt(sessionName)
	if err != nil {
		// Session not found or tmux not running - can't determine staleness, allow message
		return false, ""
	}

	return session.StaleReasonForTimes(msg.Timestamp, createdAt)
}

// HandleLifecycleShutdown processes a LIFECYCLE:Shutdown message.
// Similar to POLECAT_DONE but triggered by daemon rather than polecat.
// Persistent polecat model (gt-4ac): sandbox preserved, polecat remains reuse-eligible.
func HandleLifecycleShutdown(workDir, rigName string, msg *mail.Message) *HandlerResult {
	result := &HandlerResult{
		MessageID:    msg.ID,
		ProtocolType: ProtoLifecycleShutdown,
	}

	// Extract polecat name from subject
	matches := PatternLifecycleShutdown.FindStringSubmatch(msg.Subject)
	if len(matches) < 2 {
		result.Error = fmt.Errorf("invalid LIFECYCLE:Shutdown subject: %s", msg.Subject)
		return result
	}
	polecatName := matches[1]

	// Persistent model: sandbox preserved for reuse.
	// If polecat has dirty state, that's fine — it stays reuse-eligible until
	// someone slings new work to it (which will repair the worktree).
	result.Handled = true
	result.Action = fmt.Sprintf("polecat %s shutdown — sandbox preserved", polecatName)

	return result
}

// HandleHelp processes a HELP message from a polecat requesting intervention.
// Parses the HELP payload, assesses category/severity, and presents a
// classified summary to the witness agent for triage.
func HandleHelp(workDir, rigName string, msg *mail.Message, router *mail.Router) *HandlerResult {
	result := &HandlerResult{
		MessageID:    msg.ID,
		ProtocolType: ProtoHelp,
	}

	// Parse the message
	payload, err := ParseHelp(msg.Subject, msg.Body)
	if err != nil {
		result.Error = fmt.Errorf("parsing HELP: %w", err)
		return result
	}

	// Assess category and severity from content
	payload.Assessment = AssessHelp(payload)

	// Format the help request summary for the witness agent to triage
	summary := FormatHelpSummary(payload)

	result.Handled = true
	result.Action = summary
	return result
}

// HandleMerged processes a MERGED message from the Refinery.
// Verifies cleanup_status before allowing nuke, escalates if work is at risk.
func HandleMerged(bd *BdCli, workDir, rigName string, msg *mail.Message) *HandlerResult {
	result := &HandlerResult{
		MessageID:    msg.ID,
		ProtocolType: ProtoMerged,
	}

	payload, err := ParseMerged(msg.Subject, msg.Body)
	if err != nil {
		result.Error = fmt.Errorf("parsing MERGED: %w", err)
		return result
	}

	wispID, err := findCleanupWisp(bd, workDir, rigName, payload.PolecatName)
	if err != nil {
		result.Error = fmt.Errorf("finding cleanup wisp: %w", err)
		return result
	}

	if wispID == "" {
		result.Handled = true
		result.Action = fmt.Sprintf("no cleanup wisp found for %s (may be already cleaned)", payload.PolecatName)
		return result
	}

	// Verify the polecat's commit is actually on main before allowing nuke.
	onMain, err := verifyCommitOnMain(workDir, rigName, payload.PolecatName)
	if err != nil {
		result.Action = fmt.Sprintf("warning: couldn't verify commit on main for %s: %v", payload.PolecatName, err)
	} else if !onMain {
		result.Handled = true
		result.WispCreated = wispID
		result.Error = fmt.Errorf("polecat %s commit is NOT on main - MERGED signal may be stale, DO NOT NUKE", payload.PolecatName)
		result.Action = fmt.Sprintf("BLOCKED: %s commit not verified on main, merge may have failed", payload.PolecatName)
		return result
	}

	cleanupStatus := getCleanupStatus(workDir, rigName, payload.PolecatName)
	handleMergedCleanupStatus(workDir, rigName, payload.PolecatName, cleanupStatus, wispID, result)
	return result
}

// handleMergedCleanupStatus acknowledges merge completion for persistent polecats.
// Persistent model (gt-4ac): polecats remain reuse-eligible after merge, sandbox preserved.
// ZFC (gt-5rne): Reports cleanup_status as data. The witness agent decides
// whether dirty state warrants escalation — Go code does not make that policy call.
func handleMergedCleanupStatus(_, _, polecatName, cleanupStatus, wispID string, result *HandlerResult) {
	result.Handled = true
	result.WispCreated = wispID
	result.CleanupStatus = cleanupStatus
	result.Action = fmt.Sprintf("polecat %s merged — sandbox preserved (cleanup_status=%s, wisp=%s)", polecatName, cleanupStatus, wispID)
}

// HandleMergeFailed processes a MERGE_FAILED message from the Refinery.
// Notifies the polecat that their merge was rejected and rework is needed.
func HandleMergeFailed(workDir, rigName string, msg *mail.Message, router *mail.Router) *HandlerResult {
	result := &HandlerResult{
		MessageID:    msg.ID,
		ProtocolType: ProtoMergeFailed,
	}

	// Parse the message
	payload, err := ParseMergeFailed(msg.Subject, msg.Body)
	if err != nil {
		result.Error = fmt.Errorf("parsing MERGE_FAILED: %w", err)
		return result
	}

	// Nudge the polecat about the failure instead of sending permanent mail.
	initRegistryFromWorkDir(workDir)
	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), payload.PolecatName)
	nudgeMsg := fmt.Sprintf("MERGE_FAILED: branch=%s issue=%s type=%s error=%s — fix and resubmit with 'gt done'",
		payload.Branch, payload.IssueID, payload.FailureType, payload.Error)
	t := tmux.NewTmux()
	if err := t.NudgeSession(sessionName, nudgeMsg); err != nil {
		result.Error = fmt.Errorf("nudging polecat about failure: %w", err)
		return result
	}

	result.Handled = true
	result.Action = fmt.Sprintf("nudged %s about merge failure: %s - %s", payload.PolecatName, payload.FailureType, payload.Error)

	return result
}

// HandleSwarmStart processes a SWARM_START message from the Mayor.
// Creates a swarm tracking wisp to monitor batch polecat work.
func HandleSwarmStart(bd *BdCli, workDir string, msg *mail.Message) *HandlerResult {
	result := &HandlerResult{
		MessageID:    msg.ID,
		ProtocolType: ProtoSwarmStart,
	}

	// Parse the message
	payload, err := ParseSwarmStart(msg.Body)
	if err != nil {
		result.Error = fmt.Errorf("parsing SWARM_START: %w", err)
		return result
	}

	// Create a swarm tracking wisp
	wispID, err := createSwarmWisp(bd, workDir, payload)
	if err != nil {
		result.Error = fmt.Errorf("creating swarm wisp: %w", err)
		return result
	}

	result.Handled = true
	result.WispCreated = wispID
	result.Action = fmt.Sprintf("created swarm tracking wisp %s for %s", wispID, payload.SwarmID)

	return result
}

// createCleanupWisp creates a wisp to track polecat cleanup, tagged with the
// owning rig via assignee (gt-gsrz: "polecat:<name>" alone collides across
// rigs that reuse polecat names, and cleanup wisps can land in a database
// shared across rigs — see CleanupWispAssignee).
func createCleanupWisp(bd *BdCli, workDir, rigName, polecatName, issueID, branch string) (string, error) {
	title := fmt.Sprintf("cleanup:%s", polecatName)
	description := fmt.Sprintf("Verify and cleanup polecat %s", polecatName)
	if issueID != "" {
		description += fmt.Sprintf("\nIssue: %s", issueID)
	}
	if branch != "" {
		description += fmt.Sprintf("\nBranch: %s", branch)
	}

	labels := strings.Join(CleanupWispLabels(polecatName, "pending"), ",")

	output, err := bd.Exec(workDir, "create",
		"--ephemeral",
		"--json",
		"--title", title,
		"--description", description,
		"--labels", labels,
		"--assignee", CleanupWispAssignee(rigName),
	)
	if err != nil {
		return "", err
	}

	// Parse JSON output from bd create --json
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &created); err != nil {
		return "", fmt.Errorf("could not parse bead ID from bd create output: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("bd create --json returned empty ID")
	}
	return created.ID, nil
}

// createSwarmWisp creates a wisp to track swarm (batch) work.
func createSwarmWisp(bd *BdCli, workDir string, payload *SwarmStartPayload) (string, error) {
	title := fmt.Sprintf("swarm:%s", payload.SwarmID)
	description := fmt.Sprintf("Tracking batch: %s\nTotal: %d polecats", payload.SwarmID, payload.Total)

	labels := strings.Join(SwarmWispLabels(payload.SwarmID, payload.Total, 0, payload.StartedAt), ",")

	output, err := bd.Exec(workDir, "create",
		"--ephemeral",
		"--json",
		"--title", title,
		"--description", description,
		"--labels", labels,
	)
	if err != nil {
		return "", err
	}

	// Parse JSON output from bd create --json
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &created); err != nil {
		return "", fmt.Errorf("could not parse bead ID from bd create output: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("bd create --json returned empty ID")
	}
	return created.ID, nil
}

// findCleanupWisp finds an existing cleanup wisp for a polecat, scoped to
// rigName via assignee so a same-named polecat in another rig can't match
// (gt-gsrz: see CleanupWispAssignee).
func findCleanupWisp(bd *BdCli, workDir, rigName, polecatName string) (string, error) {
	// Cleanup wisps are ephemeral (gt-4mnd): "bd list --label" only searches
	// the issues table and never sees them, regardless of flags. Use "bd
	// query" instead, same fix as findMRBeadForBranch (GH#2446).
	output, err := bd.Exec(workDir, "query",
		fmt.Sprintf("ephemeral=true AND label=polecat:%s AND label=state:merge-requested AND status=open AND assignee=%s", polecatName, CleanupWispAssignee(rigName)),
		"--json",
	)
	if err != nil {
		return "", err
	}

	// Parse JSON to get the wisp ID
	if output == "" || output == "[]" || output == "null" {
		return "", nil
	}

	var items []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		return "", fmt.Errorf("parsing cleanup wisp response: %w", err)
	}
	if len(items) > 0 {
		return items[0].ID, nil
	}
	return "", nil
}

// getCleanupStatus retrieves the cleanup_status from a polecat's agent bead.
// Returns the status string: "clean", "has_uncommitted", "has_stash", "has_unpushed"
// Returns empty string if agent bead doesn't exist or has no cleanup_status.
//
// ZFC #10: This enables the Witness to verify it's safe to nuke before proceeding.
// The polecat self-reports its git state when running `gt done`, and we trust that report.
func getCleanupStatus(workDir, rigName, polecatName string) string {
	// Construct agent bead ID using the rig's configured prefix
	// This supports non-gt prefixes like "bd-" for the beads rig
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		// Fall back to default prefix
		townRoot = workDir
	}
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)

	_, fields, err := beads.New(workDir).ForAgentBead().GetAgentBead(agentBeadID)
	if err != nil || fields == nil {
		// Agent bead doesn't exist or lookup failed - return empty (unknown status)
		return ""
	}
	return fields.CleanupStatus
}

// findMRBeadForBranch queries beads for an open merge-request bead whose
// branch field matches the given branch name. Returns the bead ID if found,
// or empty string if no matching MR bead exists.
func findMRBeadForBranch(bd *BdCli, workDir, branch string) string {
	// Use "bd query" with ephemeral=true to search the wisps table where
	// MR beads live (GH#2446). "bd list --type=merge-request" only searches
	// the issues table and misses wisps.
	output, err := bd.Exec(workDir, "query",
		"ephemeral=true AND label=gt:merge-request AND status=open",
		"--json")
	if err != nil || output == "" || output == "[]" || output == "null" {
		return ""
	}

	var items []struct {
		ID          string `json:"id"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		return ""
	}

	// Verify exact branch match using structured field parser
	for _, item := range items {
		mrFields := beads.ParseMRFields(&beads.Issue{Description: item.Description})
		if mrFields != nil && mrFields.Branch == branch {
			return item.ID
		}
	}
	return ""
}

// nudgeRefinery wakes the refinery session to check the merge queue.
// Uses immediate delivery: sends directly to the tmux pane.
// No cooperative queue — idle agents never call Drain(), so queued
// nudges would be stuck forever. Direct delivery is safe: if the
// agent is busy, text buffers in tmux and is processed at next prompt.
//
// Package-level var so tests can override with a real failure — a fake tmux
// binary can't easily produce one, since HasSession's ErrNoServer handling
// collapses "no server at all" into (false, nil) before NudgeSession is ever
// attempted (gt-mf5q review).
var nudgeRefinery = _nudgeRefinery

func _nudgeRefinery(townRoot, rigName string) error {
	initRegistryFromTownRoot(townRoot)
	sessionName := session.RefinerySessionName(session.PrefixFor(rigName))

	// Check if refinery is running
	t := tmux.NewTmux()
	running, err := t.HasSession(sessionName)
	if err != nil {
		return fmt.Errorf("checking refinery session: %w", err)
	}

	if !running {
		// Refinery not running - daemon will start it on next heartbeat.
		// MR beads are discoverable from the merge queue.
		return nil
	}

	// Immediate delivery: send directly to tmux pane.
	// No cooperative queue — idle agents never call Drain(), so queued
	// nudges would be stuck forever. Direct delivery is safe: if the
	// agent is busy, text buffers in tmux and is processed at next prompt.
	return t.NudgeSession(sessionName, "New MR available - check merge queue for pending work")
}

var slotOpenRecoveryCheck = func(workDir, rigName, polecatName string) (string, error) {
	return util.ExecWithOutput(workDir, "gt", "polecat", "check-recovery", rigName+"/"+polecatName, "--json", "--reconcile-cleanup")
}

type slotOpenSchedulerStatus struct {
	Paused      bool `json:"paused"`
	QueuedReady int  `json:"queued_ready"`
	Capacity    struct {
		Max  int `json:"max"`
		Free int `json:"free"`
	} `json:"capacity"`
}

type slotOpenSchedulerResult struct {
	Before     slotOpenSchedulerStatus
	After      slotOpenSchedulerStatus
	Ran        bool
	Dispatched int
	Output     string
}

var runSchedulerForSlotOpen = defaultRunSchedulerForSlotOpen
var slotOpenDecisionForNotify = slotOpenDecision

func defaultRunSchedulerForSlotOpen(townRoot string) (slotOpenSchedulerResult, error) {
	var result slotOpenSchedulerResult

	before, err := readSchedulerStatusForSlotOpen(townRoot)
	if err != nil {
		return result, err
	}
	result.Before = before

	if before.Paused || before.Capacity.Max <= 0 || before.Capacity.Free <= 0 || before.QueuedReady == 0 {
		return result, nil
	}

	output, err := runGTForSlotOpen(townRoot, "scheduler", "run")
	result.Ran = true
	result.Output = output
	if err != nil {
		return result, err
	}
	result.Dispatched = parseSchedulerRunDispatched(output)

	after, err := readSchedulerStatusForSlotOpen(townRoot)
	if err != nil {
		return result, err
	}
	result.After = after
	return result, nil
}

func parseSchedulerRunDispatched(output string) int {
	fields := strings.Fields(output)
	for i, field := range fields {
		if field != "Dispatched" || i+1 >= len(fields) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimRight(fields[i+1], ","))
		if err == nil {
			return n
		}
	}
	return 0
}

func readSchedulerStatusForSlotOpen(townRoot string) (slotOpenSchedulerStatus, error) {
	var status slotOpenSchedulerStatus
	output, err := runGTForSlotOpen(townRoot, "scheduler", "status", "--json")
	if err != nil {
		return status, err
	}
	jsonOutput := strings.TrimSpace(output)
	if idx := strings.Index(jsonOutput, "{"); idx > 0 {
		jsonOutput = jsonOutput[idx:]
	}
	if err := json.Unmarshal([]byte(jsonOutput), &status); err != nil {
		return status, fmt.Errorf("parse scheduler status: %w", err)
	}
	return status, nil
}

func runGTForSlotOpen(townRoot string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gt", args...)
	cmd.Dir = townRoot
	cmd.Env = append(beads.BuildMutationRoutingBDEnv(os.Environ(), filepath.Join(townRoot, ".beads")), "GT_DAEMON=1")
	out, err := cmd.CombinedOutput()
	output := string(out)
	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("gt %s timed out after 5m", strings.Join(args, " "))
	}
	if err != nil {
		return output, fmt.Errorf("gt %s failed: %w (output: %s)", strings.Join(args, " "), err, strings.TrimSpace(output))
	}
	return output, nil
}

func shouldNotifyMayorSlotOpen(workDir, rigName, polecatName string) (bool, string) {
	output, err := slotOpenRecoveryCheck(workDir, rigName, polecatName)
	if err != nil {
		return false, fmt.Sprintf("check-recovery failed: %v", err)
	}

	var status struct {
		Verdict  string   `json:"verdict"`
		Blockers []string `json:"blockers,omitempty"`
	}
	jsonOutput := strings.TrimSpace(output)
	if idx := strings.Index(jsonOutput, "{"); idx > 0 {
		jsonOutput = jsonOutput[idx:]
	}
	if err := json.Unmarshal([]byte(jsonOutput), &status); err != nil {
		return false, fmt.Sprintf("check-recovery json parse failed: %v", err)
	}
	if status.Verdict != "SAFE_TO_NUKE" {
		reason := "check-recovery verdict=" + status.Verdict
		if len(status.Blockers) > 0 {
			reason += " blockers=" + strings.Join(status.Blockers, ";")
		}
		return false, reason
	}
	return true, ""
}

// notifyMayorSlotOpen nudges the Mayor that a polecat slot is now open.
// This is critical for pipeline throughput: without it, the Mayor sits idle
// even when open beads exist, because it never learns about the completion.
// Prefers nudge per communication hygiene, falls back to mail if nudge
// can't reach the Mayor (e.g., ACP session, no tmux). (GH#2727)
func notifyMayorSlotOpen(workDir, rigName, polecatName, exitType string) {
	townRoot, _ := workspace.Find(workDir)
	if townRoot == "" {
		return
	}
	if exitType != string(ExitTypeCompleted) {
		decision := slotOpenDecisionForNotify(workDir, townRoot, rigName, polecatName, exitType)
		if !decision.Reusable {
			_, _ = channelevents.EmitToTown(townRoot, "mayor", "", "SLOT_BLOCKED", []string{
				"source=witness",
				"rig=" + rigName,
				"polecat=" + polecatName,
				"exit=" + exitType,
				"reason=" + decision.Reason,
			})
		}
		return
	}
	if ok, reason := shouldNotifyMayorSlotOpen(workDir, rigName, polecatName); !ok {
		fmt.Fprintf(os.Stderr, "witness: suppressing SLOT_OPEN for %s/%s: %s\n", rigName, polecatName, reason)
		return
	}
	decision := slotOpenDecisionForNotify(workDir, townRoot, rigName, polecatName, exitType)
	if !decision.Reusable {
		_, _ = channelevents.EmitToTown(townRoot, "mayor", "", "SLOT_BLOCKED", []string{
			"source=witness",
			"rig=" + rigName,
			"polecat=" + polecatName,
			"exit=" + exitType,
			"reason=" + decision.Reason,
		})
		return
	}
	if result, err := runSchedulerForSlotOpen(townRoot); err != nil {
		fmt.Fprintf(os.Stderr, "witness: SLOT_OPEN scheduler trigger failed for %s/%s: %v\n", rigName, polecatName, err)
		if result.Dispatched > 0 {
			return
		}
	} else if result.Dispatched > 0 {
		if status, ok := schedulerOpenAfterSlot(result); ok {
			notifyMayorSchedulerOpen(townRoot, rigName, polecatName, exitType, status)
		}
		return
	} else if status, ok := schedulerOpenAfterSlot(result); ok {
		notifyMayorSchedulerOpen(townRoot, rigName, polecatName, exitType, status)
		return
	} else if status := schedulerStatusAfterSlot(result); status.Capacity.Max > 0 && (status.Paused || status.Capacity.Free <= 0) {
		return
	}

	// Emit SLOT_OPEN channel event so Mayor's await-event unblocks instantly.
	_, _ = channelevents.EmitToTown(townRoot, "mayor", "", "SLOT_OPEN", []string{
		"source=witness",
		"rig=" + rigName,
		"polecat=" + polecatName,
		"exit=" + exitType,
	})

	// Try nudge first — lightweight, no Dolt commit.
	mayorSession := session.MayorSessionName()
	t := tmux.NewTmux()
	if running, err := t.HasSession(mayorSession); err == nil && running {
		msg := fmt.Sprintf("SLOT_OPEN: %s/%s completed (exit=%s) — slot available. Run `gt polecat list` to verify and sling next bead.", rigName, polecatName, exitType)
		if err := t.NudgeSession(mayorSession, msg); err == nil {
			return // Nudge delivered — no mail needed.
		}
	}

	// Nudge failed or Mayor not in tmux (e.g., ACP/Claude Code session).
	// Fall back to mail so the completion is not silently lost.
	subject := fmt.Sprintf("SLOT_OPEN: %s/%s completed (exit=%s)", rigName, polecatName, exitType)
	body := fmt.Sprintf("Polecat %s/%s finished (exit=%s). Slot available for next bead.", rigName, polecatName, exitType)
	cmd := exec.Command("gt", "mail", "send", "mayor/", "-s", subject, "-m", body)
	cmd.Dir = townRoot
	_ = cmd.Run()
}

func schedulerOpenAfterSlot(result slotOpenSchedulerResult) (slotOpenSchedulerStatus, bool) {
	status := schedulerStatusAfterSlot(result)
	return status, !status.Paused && status.Capacity.Max > 0 && status.Capacity.Free > 0 && status.QueuedReady == 0
}

func schedulerStatusAfterSlot(result slotOpenSchedulerResult) slotOpenSchedulerStatus {
	status := result.Before
	if result.Ran {
		status = result.After
	}
	return status
}

func notifyMayorSchedulerOpen(townRoot, rigName, polecatName, exitType string, status slotOpenSchedulerStatus) {
	_, _ = channelevents.EmitToTown(townRoot, "mayor", "", "SCHEDULER_OPEN", []string{
		"source=witness",
		"rig=" + rigName,
		"polecat=" + polecatName,
		"exit=" + exitType,
		"capacity_free=" + strconv.Itoa(status.Capacity.Free),
		"queued_ready=" + strconv.Itoa(status.QueuedReady),
	})

	mayorSession := session.MayorSessionName()
	t := tmux.NewTmux()
	msg := fmt.Sprintf("SCHEDULER_OPEN: %s/%s completed (exit=%s); scheduler has capacity but no eligible queued beads remain.", rigName, polecatName, exitType)
	if running, err := t.HasSession(mayorSession); err == nil && running {
		if err := t.NudgeSession(mayorSession, msg); err == nil {
			return
		}
	}

	subject := fmt.Sprintf("SCHEDULER_OPEN: %s/%s completed (exit=%s)", rigName, polecatName, exitType)
	cmd := exec.Command("gt", "mail", "send", "mayor/", "-s", subject, "-m", msg)
	cmd.Dir = townRoot
	_ = cmd.Run()
}

func slotOpenDecision(workDir, townRoot, rigName, polecatName, exitType string) polecat.SlotReuseDecision {
	if exitType != string(ExitTypeCompleted) {
		return polecat.SlotReuseDecision{Reason: "exit-" + strings.ToLower(exitType)}
	}
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
	rigBeads := beads.New(workDir)
	_, fields, err := rigBeads.ForAgentBead().GetAgentBead(agentID)
	input := polecat.SlotReuseInput{State: polecat.StateIdle, CleanupStatus: polecat.CleanupUnknown, HookBeadSafe: true, GitCheckFailed: err != nil || fields == nil}
	issueID := ""
	if fields != nil {
		// gt-ui2x: the bead was actually read here — hook_bead, push_failed,
		// mr_failed and active_mr below are verified facts, not the unread
		// defaults a not-found/error result leaves in place. See
		// ResolveIgnoreCleanupStatus's agentBeadRead/liveGitProbeRan branch.
		input.AgentBeadRead = true
		issueID = fields.LastSourceIssue
		if issueID == "" {
			issueID = fields.HookBead
		}
		if fields.HookBead != "" {
			input.HookBead = fields.HookBead
			input.HookBeadTerminal = witnessIssueTerminal(rigBeads, fields.HookBead)
			input.HookBeadSafe = input.HookBeadTerminal
		}
		input.PushFailed = fields.PushFailed
		input.MRFailed = fields.MRFailed
		input.ActiveMR = fields.ActiveMR
		if fields.CleanupStatus != "" {
			input.CleanupStatus = polecat.CleanupStatus(fields.CleanupStatus)
		}
	}
	clonePath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	g := git.NewGit(clonePath)
	bd := beads.New(beads.ResolveBeadsDir(workDir))
	var targetRefs []string
	if branch, err := g.CurrentBranch(); err == nil {
		input.Branch = branch
		// claude-41j.1 D9: label this as a measured answer so the reuse verdict
		// re-derives from it and demotes the recorded cleanup_status to a hint.
		input.GitStateSource = polecat.GitStateSourceLive
		var targetRefLookupFailed bool
		targetRefs, targetRefLookupFailed = witnessRecoveryTargetRefs(bd, fields, branch)
		if targetRefLookupFailed {
			input.MQLookupFailed = true
		}
		if status, err := g.CheckUncommittedWork(); err == nil {
			input.GitDirty = !status.CleanExcludingRuntimeAndIndexSkew(g)
			input.StashCount = status.StashCount
			input.UnpushedCommits = status.UnpushedCommits
		} else {
			input.GitCheckFailed = true
			input.GitStateSource = polecat.GitStateSourceUnknown
		}
		if preservation, err := g.BranchPreservationStatus(branch, "origin", targetRefs); err == nil {
			input.UnpushedCommits = preservation.UnpreservedPatchCount
		} else {
			input.GitCheckFailed = true
			input.GitStateSource = polecat.GitStateSourceUnknown
		}
	} else {
		input.GitCheckFailed = true
		input.GitStateSource = polecat.GitStateSourceUnknown
	}
	// gt-hsg: gitSafe is passed straight into AssessActiveMR below and
	// nowhere else — DecideSlotReuse re-derives it, and IgnoreCleanupStatus,
	// from these same raw facts via NewWorkstateInput. Resolving it here too
	// would be exactly the duplicated promotion-policy copy the unification
	// requirement exists to prevent.
	gitSafe := !input.GitCheckFailed && !input.GitDirty && input.StashCount == 0 && input.UnpushedCommits == 0
	if fields != nil && fields.ActiveMR != "" {
		sourceHint := fields.LastSourceIssue
		if sourceHint == "" {
			sourceHint = fields.HookBead
		}
		assessment := polecat.AssessActiveMRWithLandedEvidence(bd, polecat.ActiveMRInput{ActiveMR: fields.ActiveMR, SourceIssueHint: sourceHint, RequireGitSafe: true, GitSafe: gitSafe},
			func() polecat.LandedEvidence { return polecat.ProbeWorkLandedOnRef(clonePath, input.Branch, "origin") })
		if assessment.Pending {
			input.ActiveMRBlocker = assessment.Reason
		}
		if assessment.SourceTerminal {
			input.ActiveMRSourceTerminal = true
		}
	}
	input.MQCheckRequired = input.Branch != ""
	input.HasSubmittableWork = witnessHasSubmittableWork(clonePath, targetRefs)
	input.AssignedBeadTerminal = witnessIssueTerminal(rigBeads, issueID)
	input.MQNotRequired = witnessMQNotRequiredSource(rigBeads, issueID)
	if input.MQCheckRequired && input.HasSubmittableWork && !input.AssignedBeadTerminal && !input.MQNotRequired {
		mr, err := rigBeads.FindMRForBranchAny(input.Branch)
		if err != nil {
			input.MQLookupFailed = true
		} else {
			input.MRSubmitted = mr != nil
		}
	}
	return polecat.DecideSlotReuse(input)
}

func witnessRecoveryTargetRefs(bd *beads.Beads, fields *beads.AgentFields, branch string) ([]string, bool) {
	if fields == nil || bd == nil {
		return nil, false
	}
	var refs []string
	lookupFailed := false
	if fields.ActiveMR != "" {
		if issue, err := bd.Show(fields.ActiveMR); err == nil {
			if mrFields := beads.ParseMRFields(issue); mrFields != nil && mrFields.Target != "" {
				refs = append(refs, mrFields.Target)
			}
		} else if !errors.Is(err, beads.ErrNotFound) {
			lookupFailed = true
		}
	}
	if branch != "" {
		if issue, err := bd.FindMRForBranchAny(branch); err == nil {
			if mrFields := beads.ParseMRFields(issue); mrFields != nil && mrFields.Target != "" {
				refs = append(refs, mrFields.Target)
			}
		} else if !errors.Is(err, beads.ErrNotFound) {
			lookupFailed = true
		}
	}
	if fields.LastSourceIssue != "" && fields.LastSourceIssue != fields.HookBead {
		if issue, err := bd.Show(fields.LastSourceIssue); err == nil {
			refs = append(refs, witnessAttachmentTargetRefs(bd, issue)...)
		} else {
			lookupFailed = true
		}
	}
	if fields.HookBead != "" {
		if issue, err := bd.Show(fields.HookBead); err == nil {
			refs = append(refs, witnessAttachmentTargetRefs(bd, issue)...)
		} else {
			lookupFailed = true
		}
	}
	return witnessUniqueRefs(refs), lookupFailed
}

func witnessAttachmentTargetRefs(bd *beads.Beads, issue *beads.Issue) []string {
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil {
		return nil
	}
	var refs []string
	witnessAppendBaseBranchRefs(&refs, attachment.FormulaVars)
	for _, value := range attachment.AttachedVars {
		witnessAppendBaseBranchRefs(&refs, value)
	}
	if attachment.ConvoyID != "" && bd != nil {
		if convoy, err := bd.Show(attachment.ConvoyID); err == nil {
			if fields := beads.ParseConvoyFields(convoy); fields != nil && fields.BaseBranch != "" {
				refs = append(refs, fields.BaseBranch)
			}
		}
	}
	return refs
}

func witnessAppendBaseBranchRefs(refs *[]string, vars string) {
	for _, line := range strings.Split(vars, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "base_branch" {
			continue
		}
		if value = strings.TrimSpace(value); value != "" {
			*refs = append(*refs, value)
		}
	}
}

func witnessUniqueRefs(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func witnessActiveMRBlocker(bd *beads.Beads, mrID string) string {
	if mrID == "" {
		return ""
	}
	if bd == nil {
		return fmt.Sprintf("active_mr=%s status=unverified", mrID)
	}
	mr, err := bd.Show(mrID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return ""
		}
		return fmt.Sprintf("active_mr=%s status=lookup_error: %v", mrID, err)
	}
	if mr == nil || beads.IssueStatus(mr.Status).IsTerminal() {
		return ""
	}
	return fmt.Sprintf("active_mr=%s status=%s", mrID, mr.Status)
}

func witnessIssueTerminal(bd *beads.Beads, issueID string) bool {
	if bd == nil || issueID == "" {
		return false
	}
	issue, err := bd.Show(issueID)
	return err == nil && issue != nil && beads.IssueStatus(issue.Status).IsTerminal()
}

func witnessMQNotRequiredSource(bd *beads.Beads, issueID string) bool {
	if bd == nil || issueID == "" {
		return false
	}
	issue, err := bd.Show(issueID)
	if err != nil || issue == nil {
		return false
	}
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil {
		return false
	}
	return attachment.NoMerge || attachment.ReviewOnly || strings.EqualFold(strings.TrimSpace(attachment.MergeStrategy), "local")
}

func witnessHasSubmittableWork(worktreePath string, targetRefs []string) bool {
	g := git.NewGit(worktreePath)
	branch, _ := g.CurrentBranch()
	status, err := g.BranchTargetStatus(branch, "origin", targetRefs)
	return err == nil && status.UnpreservedPatchCount > 0
}

// RecoveryPayload contains data for RECOVERY_NEEDED escalation.
type RecoveryPayload struct {
	PolecatName   string
	Rig           string
	CleanupStatus string
	Branch        string
	IssueID       string
	DetectedAt    time.Time
}

// EscalateRecoveryNeeded nudges the Deacon about a RECOVERY_NEEDED situation.
// Previously sent permanent mail; now uses ephemeral nudge since the deacon
// can discover recovery state from cleanup wisps and polecat status.
// ZFC (gt-5rne): Not called directly from handlers — available for callers
// who decide escalation is warranted based on reported CleanupStatus data.
func EscalateRecoveryNeeded(workDir, rigName string, payload *RecoveryPayload) (string, error) {
	initRegistryFromWorkDir(workDir)
	sessionName := session.DeaconSessionName()
	nudgeMsg := fmt.Sprintf("RECOVERY_NEEDED: %s/%s cleanup_status=%s branch=%s issue=%s detected=%s — coordinate recovery before authorizing cleanup",
		rigName, payload.PolecatName, payload.CleanupStatus, payload.Branch, payload.IssueID, payload.DetectedAt.Format(time.RFC3339))
	t := tmux.NewTmux()
	if err := t.NudgeSession(sessionName, nudgeMsg); err != nil {
		return "", fmt.Errorf("nudging deacon about recovery: %w", err)
	}
	return "nudge", nil
}

// UpdateCleanupWispState updates a cleanup wisp's state label.
func UpdateCleanupWispState(bd *BdCli, workDir, wispID, newState string) error {
	// Get current labels to preserve other labels
	output, err := bd.Exec(workDir, "show", wispID, "--json")
	if err != nil {
		return fmt.Errorf("getting wisp: %w", err)
	}

	// Extract polecat name from existing labels via JSON parsing
	polecatName := extractPolecatFromJSON(output)

	if polecatName == "" {
		polecatName = "unknown"
	}

	// Update with new state — pass one --set-labels=<label> per label,
	// matching the pattern used in agent_state.go and molecule_await_signal.go.
	labels := CleanupWispLabels(polecatName, newState)
	args := []string{"update", wispID}
	for _, l := range labels {
		args = append(args, "--set-labels="+l)
	}
	if err := bd.Run(workDir, args...); err != nil {
		return err
	}

	// Timestamp the state transition in wisp_events so the reaper's
	// merge-requested protection TTL has a durable anchor (gt-apam). A
	// bd --set-labels update alone leaves wisp_events without the row, and
	// without the anchor the reaper cannot tell a lost MR from a fresh one.
	// Best-effort: the label is the source of truth, the event is metadata,
	// so a failure here only degrades back to created_at anchoring.
	if newState == "merge-requested" {
		recordCleanupWispLabelSet(bd, workDir, wispID, newState)
	}
	return nil
}

// wispIDShape matches the bead-id syntax bd generates for wisps (a prefix,
// which may be empty, plus hyphen-joined alphanumeric segments — e.g.
// "gt-apam", "hq-wisp-fw1m"). recordCleanupWispLabelSet checks wispID against
// it before the ID is interpolated into SQL text, since bd.Exec's "sql"
// subcommand takes a raw query string with no parameter-binding path.
var wispIDShape = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)

// recordCleanupWispLabelSet records a state label_set row in wisp_events,
// inserting only when no such row exists for the wisp yet (gt-apam).
func recordCleanupWispLabelSet(bd *BdCli, workDir, wispID, newState string) {
	// wispID reaches here from createCleanupWisp/ensureCleanupWisp bead IDs,
	// which are bd-generated today but not validated on the way in; guard the
	// SQL-text interpolation below rather than trust that (gt-apam).
	if !wispIDShape.MatchString(wispID) {
		log.Printf("witness: cleanup wisp label_set timestamp skipped for %q: not a valid bead id (TTL anchor falls back to created_at)", wispID)
		return
	}
	// wisp_events carries no label column: event_type='label_set' marks the
	// transition and old_value names the label that was set.
	stamp := time.Now().UTC().Format("2006-01-02 15:04:05")
	countQuery := "SELECT COUNT(*) FROM wisp_events WHERE issue_id = '" + wispID +
		"' AND event_type = 'label_set' AND old_value = 'state:" + newState + "'"
	output, err := bd.Exec(workDir, "sql", countQuery)
	if err != nil {
		log.Printf("witness: cleanup wisp label_set timestamp query failed for %s: %v (TTL anchor falls back to created_at)", wispID, err)
		return
	}
	// The count renders as a table (header + value row); take the last
	// whitespace-delimited token and parse it.
	tokens := strings.Fields(output)
	if len(tokens) == 0 {
		return
	}
	count, err := strconv.Atoi(tokens[len(tokens)-1])
	if err != nil {
		// Unparseable, non-empty render — bd's table format may have changed.
		// Skip the insert rather than risk a double-insert from misreading it
		// as zero; the TTL anchor falls back to created_at.
		log.Printf("witness: cleanup wisp label_set count unparseable for %s: %q (TTL anchor falls back to created_at)", wispID, output)
		return
	}
	if count > 0 {
		return // already timestamped
	}
	insertQuery := "INSERT INTO wisp_events (issue_id, event_type, actor, old_value, created_at) " +
		"VALUES ('" + wispID + "', 'label_set', 'witness', 'state:" + newState + "', '" + stamp + "')"
	if _, err := bd.Exec(workDir, "sql", insertQuery); err != nil {
		log.Printf("witness: cleanup wisp label_set timestamp insert failed for %s: %v", wispID, err)
	}
}

// extractPolecatFromJSON extracts the polecat name from bd show --json output.
// Returns empty string if the output is malformed or no polecat label is found.
func extractPolecatFromJSON(output string) string {
	var items []struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(output), &items); err != nil || len(items) == 0 {
		return ""
	}
	for _, label := range items[0].Labels {
		if name, ok := strings.CutPrefix(label, "polecat:"); ok {
			return name
		}
	}
	return ""
}

// RestartPolecatSession restarts a polecat's tmux session without destroying
// the worktree or branch. This preserves the polecat's work (commits, branches)
// while giving it a fresh agent process.
//
// Used by the witness instead of NukePolecat when a polecat is stuck, hung, or
// has a dead agent process but still has work worth preserving (gt-dsgp).
//
// The restart flow:
//  1. Kill the existing tmux session (if alive)
//  2. Start a fresh session via `gt session restart`
//  3. The new session picks up the polecat's existing hook and continues
func RestartPolecatSession(workDir, rigName, polecatName string) error {
	// Pause gate (gt-ahik): see pauseGateSkip's doc for why. This is a
	// second, cheap check behind that choke point — a read of one file, no
	// Dolt, no config lookup — safe for any future caller that reaches this
	// function directly. Fails CLOSED, same as pauseGateSkip (gt-wisp-6ajo):
	// the error branch below skips the restart rather than proceeding on an
	// unreadable marker.
	townRoot := workDirToTownRoot(workDir)
	paused, st, perr := agentpause.PauseGate(townRoot, rigName, constants.RolePolecat, polecatName)
	switch {
	case perr != nil:
		log.Printf("warning: pause gate check for %s/%s failed (%v); skipping restart (fail closed)",
			rigName, polecatName, perr)
		return nil
	case paused:
		actor := ""
		if st != nil {
			actor = st.PausedBy
		}
		log.Printf("info: skip restart of %s/%s: agent is paused (%s, %s)",
			rigName, polecatName, agentpause.Reason(st), actor)
		return nil
	}

	address := fmt.Sprintf("%s/%s", rigName, polecatName)
	if err := restartSessionExec(workDir, address); err != nil {
		return fmt.Errorf("session restart failed: %w", err)
	}
	return nil
}

// panicIfTestBinary makes a destructive real-world operation (killing a tmux
// session, nuking a polecat worktree, restarting a live session) impossible
// to reach from a `go test` binary unless the caller has injected a fake over
// the package-level executor variable that guards it. testing.Testing() is
// authoritative for any `go test` binary regardless of env vars or the
// workDir/rig/polecat name arguments a particular test passes in — env-based
// mitigations can be bypassed by a real name slipping through as a plain
// string, this cannot (gt-5itbt).
func panicIfTestBinary(op string) {
	if testing.Testing() {
		panic(fmt.Sprintf(
			"HERMETIC VIOLATION: %s attempted a real, destructive operation from "+
				"inside a test binary. Inject a fake over the package-level executor "+
				"variable instead of exercising the real implementation (gt-5itbt).", op))
	}
}

// restartSessionExec performs the actual session restart. It is a package
// variable so tests can assert the pause gate's decision without spawning a
// real `gt session restart`, which a non-hermetic test process could point at
// the live town (gt-wisp-6ajo). Swap it only from a non-parallel test, and
// restore it in t.Cleanup. The default panics under a test binary (gt-5itbt)
// instead of silently running for real when a test forgets to swap it.
var restartSessionExec = func(workDir, address string) error {
	panicIfTestBinary("RestartPolecatSession: gt session restart " + address)
	return util.ExecRun(workDir, "gt", "session", "restart", address, "--force")
}

// nukePolecatFunc is a package variable so tests can assert the zombie
// archive path's decision without shelling out to the real `gt polecat
// nuke`, which kills a live tmux session and deletes a real worktree (gt-evdg).
// Swap it only from a non-parallel test, and restore it in t.Cleanup. A test
// that forgets to swap it still cannot reach a real subprocess: the default
// is NukePolecat, whose own tmux-kill and `gt polecat nuke` seams
// (nukeKillSessionExec, nukePolecatWorktreeExec below) panic under a test
// binary unless separately faked (gt-5itbt).
var nukePolecatFunc = NukePolecat

// nukeKillSessionExec kills sessionName's tmux session, the first step of a
// polecat nuke. Package variable so tests can inject a fake instead of
// touching a real tmux server; the default panics under a test binary
// (gt-5itbt) instead of silently running for real when a test forgets to
// swap it.
var nukeKillSessionExec = func(sessionName string) {
	panicIfTestBinary("NukePolecat: kill tmux session " + sessionName)
	t := tmux.NewTmux()

	// Check if session exists and kill it
	if running, _ := t.HasSession(sessionName); running {
		// Try graceful shutdown first (Ctrl-C), then force kill
		_ = t.SendKeysRaw(sessionName, "C-c")
		// Brief delay for graceful handling
		time.Sleep(100 * time.Millisecond)
		// Force kill the session
		if err := t.KillSession(sessionName); err != nil {
			// Log but continue - session might already be dead
			// The important thing is we tried
		}
	}
}

// nukePolecatWorktreeExec runs `gt polecat nuke <address>` to clean up the
// worktree, branch and beads. Package variable so tests can inject a fake
// instead of touching the live town; the default panics under a test binary
// (gt-5itbt) instead of silently running for real when a test forgets to
// swap it.
var nukePolecatWorktreeExec = func(workDir, address string) error {
	panicIfTestBinary("NukePolecat: gt polecat nuke " + address)
	return util.ExecRun(workDir, "gt", "polecat", "nuke", address)
}

// NukePolecat executes the actual nuke operation for a polecat.
// This kills the tmux session, removes the worktree, and cleans up beads.
// Refuses to nuke polecats with pending MRs in the refinery queue (gt-6a9d).
// Refuses to nuke if Mayor ACP session is active (gt-qnp).
func NukePolecat(bd *BdCli, workDir, rigName, polecatName string) error {
	// Persistence interlock (gt-qnp): veto cleanup if Mayor ACP session is active.
	townRoot := workDirToTownRoot(workDir)
	checker := mayor.NewCleanupVetoChecker(townRoot)
	if vetoed, reason := checker.ShouldVetoCleanup(); vetoed {
		return fmt.Errorf("refusing to nuke %s/%s: %s", rigName, polecatName, reason)
	}

	// Safety gate (gt-6a9d): refuse to nuke if MR is pending in refinery.
	// Nuking deletes the remote branch, which the refinery needs to merge.
	initRegistryFromWorkDir(workDir)
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
	if hasPendingMR(bd, workDir, rigName, polecatName, agentBeadID) {
		return fmt.Errorf("refusing to nuke %s/%s: MR pending in refinery (gt-6a9d)", rigName, polecatName)
	}

	// CRITICAL: Kill the tmux session FIRST and unconditionally.
	// We do this explicitly here because gt polecat nuke may fail to kill the
	// session due to rig loading issues or race conditions with IsRunning checks.
	// See: gt-g9ft5 - sessions were piling up because nuke wasn't killing them.
	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
	nukeKillSessionExec(sessionName)

	// Now run gt polecat nuke to clean up worktree, branch, and beads
	address := fmt.Sprintf("%s/%s", rigName, polecatName)
	if err := nukePolecatWorktreeExec(workDir, address); err != nil {
		return fmt.Errorf("nuke failed: %w", err)
	}

	return nil
}

// NukePolecatResult contains the result of an auto-nuke attempt.
type NukePolecatResult struct {
	Nuked   bool
	Skipped bool
	Reason  string
	Error   error
}

// AutoNukeIfClean is a legacy function preserved for backward compatibility.
// With persistent polecats (gt-4ac), polecats are no longer auto-nuked.
// This function now always returns a "skipped" result since polecats go idle
// instead of being destroyed. The polecat's sandbox is preserved for reuse.
func AutoNukeIfClean(workDir, rigName, polecatName string) *NukePolecatResult {
	return &NukePolecatResult{
		Skipped: true,
		Reason:  "persistent polecat model: sandbox preserved for reuse (gt-4ac)",
	}
}

// verifyCommitOnMain checks if the polecat's current commit is on the default branch.
// This prevents nuking a polecat whose work wasn't actually merged.
//
// In multi-remote setups, the code may live on a remote other than "origin"
// (e.g., "gastown" for gastown.git). This function checks ALL remotes to find
// the one containing the default branch with the merged commit.
//
// Returns:
//   - true, nil: commit is verified on default branch
//   - false, nil: commit is NOT on default branch (don't nuke!)
//   - false, error: couldn't verify (treat as unsafe)
//
// This is a package-level var so tests can override it.
var verifyCommitOnMain = _verifyCommitOnMain

func _verifyCommitOnMain(workDir, rigName, polecatName string) (bool, error) {
	// Find town root from workDir
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		return false, fmt.Errorf("finding town root: %v", err)
	}

	// Get configured default branch for this rig
	defaultBranch := "main" // fallback
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}

	// Construct polecat path, handling both new and old structures
	// New structure: polecats/<name>/<rigname>/
	// Old structure: polecats/<name>/
	polecatPath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if _, err := os.Stat(polecatPath); os.IsNotExist(err) {
		// Fall back to old structure
		polecatPath = filepath.Join(townRoot, rigName, "polecats", polecatName)
	}

	// Get git for the polecat worktree
	g := git.NewGit(polecatPath)

	// Get the current HEAD commit SHA
	commitSHA, err := g.Rev("HEAD")
	if err != nil {
		return false, fmt.Errorf("getting polecat HEAD: %w", err)
	}

	// Get all configured remotes and check each one for the commit
	// This handles multi-remote setups where code may be on a remote other than "origin"
	remotes, err := g.Remotes()
	if err != nil {
		// If we can't list remotes, fall back to checking just the local branch
		isOnDefaultBranch, err := g.IsAncestor(commitSHA, defaultBranch)
		if err != nil {
			return false, fmt.Errorf("checking if commit is on %s: %w", defaultBranch, err)
		}
		return isOnDefaultBranch, nil
	}

	// Try each remote/<defaultBranch> until we find one where commit is an ancestor
	for _, remote := range remotes {
		remoteBranch := remote + "/" + defaultBranch
		isOnRemote, err := g.IsAncestor(commitSHA, remoteBranch)
		if err == nil && isOnRemote {
			return true, nil
		}
	}

	// Also try the local default branch (in case we're not tracking a remote)
	isOnDefaultBranch, err := g.IsAncestor(commitSHA, defaultBranch)
	if err == nil && isOnDefaultBranch {
		return true, nil
	}

	// Commit is not on any remote's default branch
	return false, nil
}

// verifyBranchAlreadyMerged checks whether the polecat's current branch work has
// already landed on the default branch — including via SQUASH merge, which
// rewrites commit SHAs and therefore escapes a plain ancestor check.
//
// Flow (aa-apw):
//  1. Fast path: ancestor check via verifyCommitOnMain (catches fast-forward /
//     regular merges).
//  2. Target-preservation path: reuse git.BranchTargetStatus so squash-merged
//     checkpoint work, patch-equivalent work, and advanced default branches are
//     classified the same way as check-recovery and reuse.
//
// gt-skwt: the on-disk branch can belong to a PREVIOUS assignment that
// finished and merged before this polecat was force-reassigned to a new
// hookBead — its session died before it ever checked out a branch for the
// new work. Its merge status says nothing about the CURRENT hookBead's
// (unstarted) work, so both paths above are skipped unless the checked-out
// branch's embedded issue ID actually matches hookBead.
//
// gt-evdg: a branch that never diverged from the default branch trivially
// satisfies this function's evidence too, indistinguishable from real work
// that landed — see handleZombieRestart, which gates on hookBead's own
// status for that reason.
//
// Returns:
//   - true, nil: work on this branch is already on default branch (skip restart,
//     safe to archive)
//   - false, nil: work has NOT fully landed, or the on-disk branch belongs to
//     a different (superseded) assignment — continue with restart
//   - false, error: couldn't verify — caller should treat as unsafe and restart
//
// Package-level var so tests can override.
var verifyBranchAlreadyMerged = _verifyBranchAlreadyMerged

func _verifyBranchAlreadyMerged(workDir, rigName, polecatName, hookBead string) (bool, error) {
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		return false, fmt.Errorf("finding town root: %v", err)
	}

	polecatPath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if _, err := os.Stat(polecatPath); os.IsNotExist(err) {
		polecatPath = filepath.Join(townRoot, rigName, "polecats", polecatName)
	}

	g := git.NewGit(polecatPath)

	branch, err := g.CurrentBranch()
	if err != nil {
		return false, err
	}

	// gt-skwt: reject a stale branch left over from a superseded assignment.
	if hookBead != "" {
		if meta, ok := polecat.ParseBranchName(branch); ok && meta.Issue != "" && meta.Issue != hookBead {
			return false, nil
		}
	}

	// Fast path: reuse existing ancestor check.
	if onMain, err := verifyCommitOnMain(workDir, rigName, polecatName); err == nil && onMain {
		return true, nil
	}

	defaultBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}

	remotes, err := g.Remotes()
	if err != nil || len(remotes) == 0 {
		remotes = []string{"origin"}
	}

	for _, remote := range remotes {
		upstream := remote + "/" + defaultBranch
		status, err := g.BranchTargetStatus(branch, remote, []string{upstream})
		if err != nil {
			continue // try next remote
		}
		if status.Preserved {
			return true, nil
		}
	}

	return false, nil
}

// ZombieClassification categorizes why a polecat was classified as a zombie.
// These are distinct from AgentState — they describe the zombie detection
// reason, not the agent's lifecycle state. See gt-tsut.
type ZombieClassification string

const (
	// ZombieStuckInDone: polecat hung in gt done (>60s with done-intent label).
	ZombieStuckInDone ZombieClassification = "stuck-in-done"
	// ZombieAgentDeadInSession: tmux session alive but agent process died.
	ZombieAgentDeadInSession ZombieClassification = "agent-dead-in-session"
	// ZombieBeadClosedStillRunning: agent alive but hooked bead already closed.
	ZombieBeadClosedStillRunning ZombieClassification = "bead-closed-still-running"
	// ZombieDoneIntentDead: session died while executing gt done.
	ZombieDoneIntentDead ZombieClassification = "done-intent-dead"
	// ZombieIdleDirtySandbox: idle polecat with uncommitted changes.
	ZombieIdleDirtySandbox ZombieClassification = "idle-dirty-sandbox"
	// ZombieSessionDeadActive: session dead but agent state indicates active work.
	ZombieSessionDeadActive ZombieClassification = "session-dead-active"
	// ZombieAgentSelfReportedStuck: agent self-reported stuck via heartbeat v2 (gt-3vr5).
	ZombieAgentSelfReportedStuck ZombieClassification = "agent-self-reported-stuck"

	// ZombieNeverHeartbeated: live session with assigned work but no heartbeat file
	// written — agent likely stuck at startup (e.g., auth 401 blocking initialization).
	// Detected once the session exceeds the HeartbeatStartupGrace threshold.
	// Flagged for formula-step review; no auto-action (auth errors don't self-heal). (gt-uk7)
	ZombieNeverHeartbeated ZombieClassification = "never-heartbeated"
	// ZombieSubmittedStillRunning: gt done submitted work cleanly, but the polecat
	// session stayed alive with an open hook and no fresh heartbeat. This catches
	// the post-submit/pre-exit ghost idle gap from GH#3055.
	ZombieSubmittedStillRunning ZombieClassification = "submitted-still-running"
)

// ImpliesActiveWork returns true if this classification indicates the polecat
// had evidence of recent work (active state or hooked bead). Used by
// receiptVerdictForZombie to derive patrol verdicts from the typed classification
// rather than a separately-computed boolean. See gt-tsut.
func (c ZombieClassification) ImpliesActiveWork() bool {
	switch c {
	case ZombieStuckInDone, ZombieAgentDeadInSession, ZombieBeadClosedStillRunning,
		ZombieDoneIntentDead, ZombieSessionDeadActive, ZombieAgentSelfReportedStuck,
		ZombieNeverHeartbeated:
		return true
	default:
		return false
	}
}

// ZombieResult describes a detected zombie polecat and the action taken.
type ZombieResult struct {
	PolecatName    string
	AgentState     string               // Real agent state from DB (e.g., "working", "idle")
	Classification ZombieClassification // Why this polecat is classified as a zombie (gt-tsut)
	HookBead       string
	CleanupStatus  string // Observed cleanup_status (ZFC: report data, agent decides policy)
	WasActive      bool   // true if evidence of recent work (active state or hooked bead)
	Action         string // "restarted", "escalated", "cleanup-wisp-created", "auto-nuked" (explicit nuke only)
	BeadRecovered  bool   // true if hooked bead was reset to open for re-dispatch
	Error          error
}

// DetectZombiePolecatsResult contains the results of a zombie detection sweep.
type DetectZombiePolecatsResult struct {
	Checked        int
	Zombies        []ZombieResult
	ConvoyFailures []ConvoyFailureResult // Mountain-Eater Layer 1: convoy failure tracking (gt-cfq)
	Errors         []error               // Transient errors that prevented checking some polecats
}

// DetectZombiePolecats cross-references polecat agent state with tmux session
// existence and agent process liveness to find zombie polecats. Two zombie classes:
//   - Session-dead: tmux session is dead but agent bead still shows agent_state=
//     "working", "running", or "spawning", or has a hook_bead assigned.
//   - Agent-dead: tmux session exists but the agent process (Claude/node) inside
//     it has died. Detected via IsAgentAlive. See gt-kj6r6.
//
// Zombies cannot send POLECAT_DONE or other signals, so they sit undetected
// by the reactive signal-based patrol. This function provides proactive detection.
//
// Race safety: Records the detection timestamp before checking session liveness.
// Before taking any action, re-verifies that the session hasn't been recreated
// since detection. This prevents killing newly-spawned sessions that reuse the
// same name.
//
// Dedup: Checks for existing cleanup wisps before escalating, preventing
// infinite escalation loops on subsequent patrol cycles.
//
// gt-dsgp: Restart-first policy. For each zombie found, we RESTART the session
// instead of nuking. This preserves the polecat's worktree and branch, preventing
// work loss. Nuking only happens via explicit `gt polecat nuke` command.
//
// For each zombie found:
//   - If polecat has a pending MR: skip (not a zombie, waiting for refinery)
//   - If session is dead but state is working: restart the session
//   - If agent is dead inside live session: restart the session
//   - If agent is hung (no output for 30+ min): restart the session
//   - If git state is dirty (unpushed/uncommitted work): report cleanup_status,
//     create cleanup wisp (witness agent decides escalation policy, gt-5rne)
func DetectZombiePolecats(bd *BdCli, workDir, rigName string, router *mail.Router) *DetectZombiePolecatsResult {
	result := &DetectZombiePolecatsResult{}

	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		townRoot = workDir
	}
	initRegistryFromTownRoot(townRoot)

	// Load witness thresholds from config (fallback to compiled-in defaults).
	witCfg := config.LoadOperationalConfig(townRoot).GetWitnessConfig()

	polecatsDir := filepath.Join(townRoot, rigName, "polecats")
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return result
	}

	t := tmux.NewTmux()

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		polecatName := entry.Name()
		sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
		result.Checked++

		// Pause gate (gt-ahik): see pauseGateSkip doc.
		if pauseGateSkip(townRoot, rigName, polecatName) {
			continue
		}

		detectedAt := time.Now()

		sessionAlive, err := t.HasSession(sessionName)
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Errorf("checking session %s: %w", sessionName, err))
			continue
		}

		prefix := beads.GetPrefixForRig(townRoot, rigName)
		agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)

		// gt-2gra: Fetch agent bead data once per polecat instead of 3-5 times
		// across helper functions. The snapshot is passed to sub-functions.
		snap := fetchAgentBeadSnapshot(workDir, agentBeadID)

		var labels []string
		if snap != nil {
			labels = snap.Labels
		}
		doneIntent := extractDoneIntent(labels)

		if sessionAlive {
			// gt-s8bq: Idle Polecat Heresy fix. Idle polecats are HEALTHY — they
			// have no hook_bead, agent_state="idle", and their sandbox is preserved
			// for reuse. Skip them entirely during patrol. Only report if the
			// sandbox is dirty (uncommitted changes in idle state).
			agentState := ""
			if snap != nil {
				agentState = snap.AgentState
			}
			if beads.AgentState(agentState) == AgentStateIdle {
				cleanupStatus := snap.cleanupStatus()
				if cleanupStatus != "" && cleanupStatus != "clean" {
					// ZFC (gt-5rne): Report data, don't escalate. The witness agent
					// decides whether dirty idle state warrants escalation.
					zombie := ZombieResult{
						PolecatName:    polecatName,
						AgentState:     agentState,
						Classification: ZombieIdleDirtySandbox,
						CleanupStatus:  cleanupStatus,
						WasActive:      false,
						Action:         "detected-dirty-idle-polecat",
					}
					result.Zombies = append(result.Zombies, zombie)
				}
				// Clean idle polecat — healthy, skip entirely.
				continue
			}

			if zombie, found := detectZombieLiveSession(bd, workDir, townRoot, rigName, polecatName, sessionName, t, doneIntent, witCfg, snap, agentBeadID); found {
				result.Zombies = append(result.Zombies, zombie)
			}
			continue // Either handled or not a zombie
		}

		if zombie, found := detectZombieDeadSession(bd, workDir, townRoot, rigName, polecatName, sessionName, t, doneIntent, detectedAt, witCfg, snap, agentBeadID); found {
			result.Zombies = append(result.Zombies, zombie)
		}
	}

	// Mountain-Eater Layer 1 (gt-cfq): Track polecat failures for convoy-tracked issues.
	// For each zombie with an active hook_bead (polecat failed without completing work),
	// check if the issue belongs to a convoy and track the failure.
	trackConvoyFailures(bd, workDir, result)

	return result
}

// pauseGateSkip is the choke point every zombie classification and its
// consequent action (restart, archive/nuke via aa-apw, done-intent label
// clearing, cleanup wisp creation) sits behind: an operator-sanctioned pause
// (gt agent pause) is indistinguishable from a stuck agent to the scanner
// alone — the mayor's SIGSTOP froze flint and the stuck-agent dog respawned
// it 20 minutes later (gt-ahik). DetectZombiePolecats calls this before any
// of the above runs, so a paused polecat is never classified as a zombie and
// none of their side effects run — reported as skipped, not restarted.
// DetectStalledPolecats calls it too, so a frozen pane never gets blind
// dialog-recovery keystrokes sent into it.
//
// PauseGate fails CLOSED: an unreadable marker is treated as paused, same as
// an explicit one, and that failure is logged as a warning rather than
// silently swallowed.
func pauseGateSkip(townRoot, rigName, polecatName string) bool {
	paused, pst, perr := agentpause.PauseGate(townRoot, rigName, constants.RolePolecat, polecatName)
	if !paused {
		return false
	}
	if perr != nil {
		log.Printf("warning: pause gate check for %s/%s failed (%v); skipping zombie detection (fail closed)",
			rigName, polecatName, perr)
	} else {
		log.Printf("info: skip zombie detection for %s/%s: agent is paused (%s)",
			rigName, polecatName, agentpause.Reason(pst))
	}
	return true
}

// observeDoneIntentActivity is the real-activity probe the stuck-in-done gate
// runs; a seam so tests can supply a snapshot without a live tmux session.
var observeDoneIntentActivity = ObserveRealActivity

// restartStuckSession is the restart the stuck-in-done gate performs; a seam so
// tests can assert the decision without spawning `gt session restart`.
var restartStuckSession = RestartPolecatSession

// detectZombieLiveSession checks a polecat with a live tmux session for zombie indicators:
// stuck done-intent, dead agent process, or closed bead while still running.
//
// gt-dsgp: Uses restart-first policy. Instead of nuking polecats, restarts their
// sessions to preserve worktrees and branches.
func detectZombieLiveSession(bd *BdCli, workDir, townRoot, rigName, polecatName, sessionName string, t *tmux.Tmux, doneIntent *DoneIntent, witCfg *config.WitnessThresholds, snap *agentBeadSnapshot, agentBeadID string) (ZombieResult, bool) {
	// gt-2gra: Agent state and hook bead are read from the pre-fetched snapshot
	// instead of calling getAgentBeadState multiple times per code path.
	snapState, snapHook := "", ""
	if snap != nil {
		snapState, snapHook = snap.AgentState, snap.HookBead
	}

	// Heartbeat v2 check (gt-3vr5): if the agent reports its own state via heartbeat,
	// trust the agent-reported state instead of inferring from timers.
	// The witness makes exactly ONE inference: is the heartbeat fresh?
	hb := polecat.ReadSessionHeartbeat(townRoot, sessionName)
	if hb != nil && hb.IsV2() {
		stale := time.Since(hb.Timestamp) >= polecat.SessionHeartbeatStaleThreshold
		if !stale {
			switch hb.EffectiveState() {
			case polecat.HeartbeatExiting:
				// Agent self-reports exiting — trust it, no timer-based inference.
				// Replaces done-intent stuck timeout for v2 agents.
				return ZombieResult{}, false

			case polecat.HeartbeatStuck:
				// Agent self-reports stuck — escalate (don't restart, agent is alive).
				zombie := ZombieResult{
					PolecatName:    polecatName,
					AgentState:     snapState,
					Classification: ZombieAgentSelfReportedStuck,
					HookBead:       snapHook,
					WasActive:      true,
					Action:         fmt.Sprintf("escalated (agent self-reported stuck: %s)", hb.Context),
				}
				return zombie, true

			case polecat.HeartbeatWorking, polecat.HeartbeatIdle:
				// Fresh heartbeat, healthy state — not a zombie.
				return ZombieResult{}, false
			}
		}
		// Stale v2 heartbeat — fall through to legacy detection.
		// Agent may have died; let the existing checks determine action.
	}

	// Legacy detection: Check for done-intent stuck too long (polecat hung in gt done).
	// gt-dsgp: Restart instead of nuke — the session is stuck trying to exit,
	// a fresh start will let it retry or pick up its hook cleanly.
	//
	// gt-z7vr: the age makes this a candidate, not a verdict. A polecat whose
	// gt done runs a long gate, or which is resolving a rebase after main moved
	// during gt done, passes the timeout while alive and producing output;
	// restarting it there interrupts the rebase and leaves a same-named origin
	// branch at the pre-rebase tip, which one session later reads as real
	// divergence and needs an operator force push (gt-bf5x). So the restart also
	// requires the transcript to show no work since the done-intent was written.
	if doneIntent != nil && time.Since(doneIntent.Timestamp) > witCfg.DoneIntentStuckTimeoutD() {
		// TOCTOU guard (gt-0pst): Re-check session liveness before restarting.
		// The session could have exited normally between our initial check and here.
		if alive, _ := t.HasSession(sessionName); !alive {
			return ZombieResult{}, false
		}
		// Not positive evidence of a wedge: return before the later checks, which
		// assume no done-intent is in flight and would nudge a healthy polecat
		// that is still inside gt done.
		if !observeDoneIntentActivity(t, polecatName, sessionName, "").ConfirmsStoppedWork(doneIntent.Timestamp) {
			return ZombieResult{}, false
		}
		zombie := ZombieResult{
			PolecatName:    polecatName,
			AgentState:     snapState,
			Classification: ZombieStuckInDone,
			HookBead:       snapHook,
			WasActive:      true,
			Action:         fmt.Sprintf("restarted-stuck-session (done-intent age=%v)", time.Since(doneIntent.Timestamp).Round(time.Second)),
		}
		// Clear ALL done-intent labels before restart so the polecat doesn't
		// immediately re-trigger stuck-in-done on the next patrol cycle (gt-wmpy).
		clearAllDoneIntentLabels(beads.New(workDir).ForAgentBead(), agentBeadID, snap)
		if err := restartStuckSession(workDir, rigName, polecatName); err != nil {
			zombie.Error = err
			zombie.Action = fmt.Sprintf("restart-stuck-session-failed: %v", err)
		}
		return zombie, true
	}

	// Tmux alive but agent process dead (gt-kj6r6).
	// gt-dsgp: Restart instead of nuke — preserve worktree and branch.
	if !t.IsAgentAlive(sessionName) {
		zombie := ZombieResult{
			PolecatName:    polecatName,
			AgentState:     snapState,
			Classification: ZombieAgentDeadInSession,
			HookBead:       snapHook,
			WasActive:      true,
			Action:         "restarted-agent-dead-session",
		}
		// TOCTOU guard (gt-0pst): Re-check session liveness before restarting.
		// The session could have exited normally between our initial check and here.
		if alive, _ := t.HasSession(sessionName); !alive {
			return ZombieResult{}, false
		}
		if err := RestartPolecatSession(workDir, rigName, polecatName); err != nil {
			zombie.Error = err
			zombie.Action = fmt.Sprintf("restart-agent-dead-session-failed: %v", err)
		}
		return zombie, true
	}

	// Agent alive but hooked bead closed — occupying slot without work (gt-h1l6i).
	// gt-dsgp: Restart instead of nuke — the fresh session will pick up its hook
	// and run gt done properly, or go idle waiting for new work.
	if hookSt, hookOk := getBeadStatus(bd, workDir, snapHook); snapHook != "" && hookOk && hookSt == "closed" {
		zombie := ZombieResult{
			PolecatName:    polecatName,
			AgentState:     snapState,
			Classification: ZombieBeadClosedStillRunning,
			HookBead:       snapHook,
			WasActive:      true,
			Action:         "restarted-bead-closed-polecat",
		}
		// TOCTOU guard (gt-0pst): Re-check session liveness before restarting.
		// The session could have exited normally between our initial check and here.
		if alive, _ := t.HasSession(sessionName); !alive {
			return ZombieResult{}, false
		}
		if err := RestartPolecatSession(workDir, rigName, polecatName); err != nil {
			zombie.Error = err
			zombie.Action = fmt.Sprintf("restart-bead-closed-failed: %v", err)
		}
		return zombie, true
	}

	// GH#3055: gt done can successfully submit work and leave cleanup_status=clean,
	// but fail before exiting the polecat session. If successful MR evidence exists
	// and the hook is either gone or still open, nudge the live session to finish
	// instead of letting it sit idle forever.
	if zombie, found := detectSubmittedStillRunning(bd, workDir, polecatName, sessionName, t, hb, snap, witCfg.HeartbeatStartupGraceD()); found {
		return zombie, true
	}

	// Live session with assigned work OR an active agent_state (e.g. "spawning")
	// but no heartbeat file: agent stuck at startup (e.g., auth 401 blocking
	// initialization, or a partial spawn that never durably attached a hook_bead).
	// Once the session is old enough to have written a first heartbeat and hasn't,
	// flag for formula-step review. gt-gf6t: previously gated on hook_bead alone,
	// so a polecat stuck at agent_state=spawning with no hook_bead yet was
	// invisible here even though `gt polecat list --json` already flags any
	// active agent_state as NEEDS_RECOVERY regardless of hook_bead. Idle/done/
	// nuked states never reach this function, so this can't fire on a healthy
	// idle polecat.
	// ZFC (gt-uk7): No auto-restart — auth errors don't self-heal on restart.
	// gt-gx2v: the absent heartbeat is missing metadata, not evidence of death,
	// so a working session is not flagged at all.
	if (snapHook != "" || beads.AgentState(snapState).IsActive()) && hb == nil {
		if createdAt, err := t.GetSessionCreatedTime(sessionName); err == nil {
			age := time.Since(createdAt)
			grace := witCfg.HeartbeatStartupGraceD()
			if age > grace {
				live := neverHeartbeatedLiveness(t, townRoot, rigName, polecatName, sessionName, createdAt.Add(grace))
				if live.Working {
					return ZombieResult{}, false
				}
				return ZombieResult{
					PolecatName:    polecatName,
					AgentState:     snapState,
					Classification: ZombieNeverHeartbeated,
					HookBead:       snapHook,
					WasActive:      true,
					Action:         fmt.Sprintf("flagged-for-review (no heartbeat, session-age=%v, %s)", age.Round(time.Second), live.Detail),
				}, true
			}
		}
	}

	return ZombieResult{}, false
}

// neverHeartbeatedFreshWindow is how recently a signal must have moved for a
// live session with no heartbeat file to count as working rather than wedged.
// Derived from SessionHeartbeatStaleThreshold — the shortest freshness window
// any other consumer applies — so "recent" means the same thing to everyone.
const neverHeartbeatedFreshWindow = polecat.SessionHeartbeatStaleThreshold

// neverHeartbeatedEvidence is the liveness verdict the never-heartbeated rule
// needs before it flags a live, heartbeatless polecat (gt-gx2v). The heartbeat
// file is written by `gt` subcommands, so its absence dates the last `gt` call,
// not the last work — a polecat inside one long turn runs none for many minutes.
type neverHeartbeatedEvidence struct {
	// Working is true when a signal shows the session producing output.
	Working bool
	// Detail names every signal read and what it showed. It is carried verbatim
	// into the flag so a false positive is diagnosable from the patrol mail
	// alone, without a second peek at the session.
	Detail string
}

// neverHeartbeatedLiveness gathers that evidence and decides from it. A seam:
// the gate reads tmux, a transcript on disk and the container-gate pool, and
// tests pin health without any of the three.
var neverHeartbeatedLiveness = assessNeverHeartbeatedLiveness

func assessNeverHeartbeatedLiveness(t *tmux.Tmux, townRoot, rigName, polecatName, sessionName string, graceDeadline time.Time) neverHeartbeatedEvidence {
	act := ObserveRealActivity(t, polecatName, sessionName, "")
	return classifyNeverHeartbeatedLiveness(act, heldGateSlot(townRoot, rigName, polecatName), graceDeadline, time.Now())
}

// classifyNeverHeartbeatedLiveness decides whether a live heartbeatless session
// is working. Pure, so each verified false-positive shape is pinnable without
// tmux, a slot pool, or a transcript on disk (gt-gx2v).
func classifyNeverHeartbeatedLiveness(act RealActivity, gateSlot string, graceDeadline, now time.Time) neverHeartbeatedEvidence {
	gateDetail := "gate-slot=none"
	if gateSlot != "" {
		gateDetail = gateSlot
	}
	ev := neverHeartbeatedEvidence{
		Detail: strings.Join([]string{
			gateDetail,
			transcriptEvidence(act, now),
			paneOutputEvidence(act, now),
		}, ", "),
	}
	// The slot is read live, so holding one needs no timestamp test: a holder
	// that never heartbeated has lost its heartbeat file, not its suite.
	ev.Working = gateSlot != "" ||
		outputSince(act.LastActivity, act.ActivitySource == ActivitySourceTranscript, graceDeadline, now) ||
		outputSince(act.PaneOutputAt, !act.PaneOutputAt.IsZero(), graceDeadline, now)
	return ev
}

// outputSince reports whether a signal timestamp proves current work: output
// inside the freshness window that also postdates graceDeadline. Both halves
// matter. A pane writes at session creation, so without the deadline the spawn
// banner of a just-created session reads as work; and output that stopped long
// ago must not immunize a wedge for the rest of its life.
func outputSince(at time.Time, known bool, graceDeadline, now time.Time) bool {
	if !known || at.IsZero() {
		return false
	}
	return at.After(graceDeadline) && now.Sub(at) < neverHeartbeatedFreshWindow
}

// transcriptEvidence renders the transcript signal for the flag message. An
// unreadable transcript is reported as such, never as quiet: absence of
// evidence is not evidence of a wedge.
func transcriptEvidence(act RealActivity, now time.Time) string {
	switch {
	case act.ActivitySource == ActivitySourceTranscript && !act.LastActivity.IsZero():
		return fmt.Sprintf("transcript=%v old/%d bytes (dates this session)",
			now.Sub(act.LastActivity).Round(time.Second), act.TranscriptBytes)
	case len(act.Errors) > 0:
		return fmt.Sprintf("transcript=unreadable (%s)", strings.Join(act.Errors, "; "))
	default:
		return "transcript=none"
	}
}

func paneOutputEvidence(act RealActivity, now time.Time) string {
	if act.PaneOutputAt.IsZero() {
		return "pane-output=none"
	}
	return fmt.Sprintf("pane-output=%v old", now.Sub(act.PaneOutputAt).Round(time.Second))
}

// heldGateSlot names the container-gate slot this polecat holds, or "" when it
// holds none — a holder has its own verification suite running right now. The
// "<rig>/<polecat>" role format is the one `gt slot run` records, and the one
// the polecat Stop hook already matches on (internal/cmd/tap_polecat_stop.go);
// a mismatch here would silently disable this half of the liveness check.
func heldGateSlot(townRoot, rigName, polecatName string) string {
	cg := config.LoadOperationalConfig(townRoot).GetContainerGateConfig()
	rep, err := slot.StatusPoolLocksOnly(townRoot, slot.Pool{Slots: cg.SlotsV(), ReservedForGate: cg.ReservedForGateV()})
	if err != nil {
		return ""
	}
	mine := rep.HeldBy(rigName + "/" + polecatName)
	if len(mine) == 0 {
		return ""
	}
	return fmt.Sprintf("gate-slot held by %s (verification suite running)", mine[0].Owner.Role)
}

func detectSubmittedStillRunning(bd *BdCli, workDir, polecatName, sessionName string, t *tmux.Tmux, hb *polecat.SessionHeartbeat, snap *agentBeadSnapshot, staleThreshold time.Duration) (ZombieResult, bool) {
	snapState, snapHook := "", ""
	if snap != nil {
		snapState, snapHook = snap.AgentState, snap.HookBead
	}
	hookStatus := "none"
	if snapHook != "" {
		hookSt, hookOk := getBeadStatus(bd, workDir, snapHook)
		if !hookOk || !isOpenHookStatus(hookSt) {
			return ZombieResult{}, false
		}
		hookStatus = hookSt
	}
	age, shouldNudge := isSubmittedStillRunningCandidate(snap, hb, staleThreshold)
	if !shouldNudge {
		return ZombieResult{}, false
	}

	zombie := ZombieResult{
		PolecatName:    polecatName,
		AgentState:     snapState,
		Classification: ZombieSubmittedStillRunning,
		HookBead:       snapHook,
		CleanupStatus:  snap.cleanupStatus(),
		WasActive:      false,
		Action:         fmt.Sprintf("nudged-exit-submitted-session (idle=%v, hook_status=%s)", age.Round(time.Second), hookStatus),
	}
	msg := fmt.Sprintf("RECOVERY_NEEDED: gt done appears submitted (hook=%s, cleanup_status=clean), but this session is still running with no fresh heartbeat for %v. If work is already submitted, exit now; otherwise run gt done again.", hookStatusForNudge(snapHook), age.Round(time.Second))
	if err := t.NudgeSession(sessionName, msg); err != nil {
		zombie.Error = err
		zombie.Action = fmt.Sprintf("nudge-exit-submitted-session-failed: %v", err)
	}
	return zombie, true
}

func isSubmittedStillRunningCandidate(snap *agentBeadSnapshot, hb *polecat.SessionHeartbeat, staleThreshold time.Duration) (time.Duration, bool) {
	if snap == nil || snap.cleanupStatus() != "clean" || !hasSuccessfulSubmissionEvidence(snap) {
		return 0, false
	}
	if beads.AgentState(snap.AgentState) == AgentStateIdle {
		return 0, false
	}
	age := snap.age()
	if hb != nil {
		age = time.Since(hb.Timestamp)
	}
	return age, age >= staleThreshold
}

func hookStatusForNudge(hookBead string) string {
	if hookBead == "" {
		return "none"
	}
	return hookBead
}

func isOpenHookStatus(status string) bool {
	switch status {
	case "open", "hooked", "in_progress":
		return true
	default:
		return false
	}
}

func hasSuccessfulSubmissionEvidence(snap *agentBeadSnapshot) bool {
	if snap == nil {
		return false
	}
	if snap.Fields != nil && (snap.Fields.MRFailed || snap.Fields.PushFailed) {
		return false
	}
	if snap.ActiveMR != "" {
		return true
	}
	if snap.Fields == nil {
		return false
	}
	return snap.Fields.ActiveMR != "" || snap.Fields.MRID != ""
}

// detectZombieDeadSession checks a polecat with a dead tmux session for zombie indicators:
// stale done-intent, or active agent state / hooked bead with no session.
//
// gt-dsgp: Uses restart-first policy. Instead of nuking polecats with dead sessions,
// restarts them to preserve worktrees and branches.
func detectZombieDeadSession(bd *BdCli, workDir, townRoot, rigName, polecatName, sessionName string, t *tmux.Tmux, doneIntent *DoneIntent, detectedAt time.Time, witCfg *config.WitnessThresholds, snap *agentBeadSnapshot, agentBeadID string) (ZombieResult, bool) {
	// gt-2gra: Agent state and hook bead are read from the pre-fetched snapshot.
	snapState, snapHook := "", ""
	if snap != nil {
		snapState, snapHook = snap.AgentState, snap.HookBead
	}

	// Heartbeat v2 check (gt-3vr5): for dead sessions, a fresh heartbeat means
	// the session isn't actually dead (race condition). A stale heartbeat confirms death.
	// This check is supplementary — dead session detection proceeds normally after.
	if hb := polecat.ReadSessionHeartbeat(townRoot, sessionName); hb != nil && hb.IsV2() {
		stale := time.Since(hb.Timestamp) >= polecat.SessionHeartbeatStaleThreshold
		if !stale {
			// Fresh heartbeat but session appears dead — possible race.
			// Skip zombie detection; the session may have just restarted.
			return ZombieResult{}, false
		}
	}

	// Done-intent: polecat was trying to exit.
	if doneIntent != nil {
		age := time.Since(doneIntent.Timestamp)
		if age < witCfg.DoneIntentRecentGraceD() {
			return ZombieResult{}, false // Recent — still working through gt done
		}

		// If bead is already closed, the polecat completed successfully.
		// The dead session is expected (gt done kills it). Leave it alone. (gt-sy8)
		hookSt, hookFound := getBeadStatus(bd, workDir, snapHook)
		beadAlreadyClosed := snapHook != "" && hookFound && (hookSt == "closed" || hookSt == "")
		if beadAlreadyClosed {
			// gt-dsgp: Polecat completed its work. Don't nuke, don't restart.
			// The sandbox is preserved for reuse by future slings.
			return ZombieResult{}, false
		}

		// Persistent polecat model (gt-6a9d): Do NOT touch if there's a pending MR.
		// The polecat completed normally (gt done → session exit). Its MR is in the
		// refinery queue. Nuking would delete the remote branch before the refinery
		// can merge it. The dead session is expected, not a zombie.
		// gt-2gra: Use snapshot's ActiveMR instead of calling getAgentActiveMR.
		if hasPendingMRFromSnapshot(bd, workDir, rigName, polecatName, snap) {
			return ZombieResult{}, false
		}
		if terminalSafeDoneSnapshot(bd, workDir, rigName, polecatName, snap) {
			return ZombieResult{}, false
		}

		// gt-jv7v: the label alone is not a crashed exit. A restart only
		// resumes work that is still there to resume — see
		// doneIntentWorthRestarting. Clearing the label keeps the next patrol
		// cycle from re-deciding it.
		if !doneIntentWorthRestarting(snap, age, witCfg.DoneIntentMaxAgeD()) {
			clearAllDoneIntentLabels(beads.New(workDir).ForAgentBead(), agentBeadID, snap)
			return ZombieResult{}, false
		}

		// gt-dsgp: Restart instead of nuke — the session died during gt done,
		// restart it so it can retry the exit sequence or pick up new work.
		// Clear ALL done-intent labels before restart so the polecat doesn't
		// immediately re-trigger stuck-in-done on the next patrol cycle (gt-wmpy).
		clearAllDoneIntentLabels(beads.New(workDir).ForAgentBead(), agentBeadID, snap)
		zombie := ZombieResult{
			PolecatName:    polecatName,
			AgentState:     snapState,
			Classification: ZombieDoneIntentDead,
			HookBead:       snapHook,
			WasActive:      true,
			Action:         fmt.Sprintf("restarted (done-intent age=%v, type=%s)", age.Round(time.Second), doneIntent.ExitType),
		}
		if err := RestartPolecatSession(workDir, rigName, polecatName); err != nil {
			zombie.Error = err
			zombie.Action = fmt.Sprintf("restart-failed (done-intent): %v", err)
		}
		return zombie, true
	}

	// Standard zombie detection: active state or hooked bead with dead session.
	typedState := beads.AgentState(snapState)
	if !isZombieState(typedState, snapHook) {
		return ZombieResult{}, false
	}

	// GH#2795: A "done" or "nuked" polecat with a dead session has completed
	// or been intentionally stopped. The dead session is expected — the hook
	// bead may not be "closed" yet (refinery queue, manual cleanup), but the
	// polecat is not a zombie. Without this check, isZombieState returns true
	// on every patrol cycle (hookBead != ""), flooding the mayor inbox.
	if typedState == beads.AgentStateDone || typedState == beads.AgentStateNuked {
		return ZombieResult{}, false
	}

	// GH#2036: Spawning polecats have hook_bead assigned but no tmux session yet.
	// This is expected during worktree creation and session startup. Skip zombie
	// detection if the polecat has been spawning for less than SpawnGracePeriod.
	if typedState == beads.AgentStateSpawning {
		// gt-2gra: Use snapshot's age instead of calling getAgentBeadAge.
		spawnAge := snap.age()
		if spawnAge < SpawnGracePeriod {
			return ZombieResult{}, false
		}
		// Spawning for too long — fall through to zombie handling
	}

	// A polecat whose hook bead is already CLOSED (or reaped) completed its
	// work successfully. The dead session is expected (gt done kills it).
	// Don't flag as zombie or trigger re-dispatch. (gt-sy8)
	// gt-dsgp: Don't nuke — sandbox preserved for reuse.
	// gt-qbh: Treat missing beads (empty status from successful lookup) as closed.
	// Wisp beads get reaped after completion, so getBeadStatus returns ("", true)
	// for reaped wisps. A missing bead is not evidence of a crash.
	// But a FAILED lookup ("", false) — e.g., cross-rig routing error — must
	// NOT be treated as closed. Default to restart (safe). (hq-wisp-n530)
	//
	// hookStatus/hookFound are reused below by handleZombieRestart (gt-evdg)
	// so a zombie with a hook bead costs exactly one bd show, not two.
	var hookStatus string
	var hookFound bool
	if snapHook != "" {
		hookStatus, hookFound = getBeadStatus(bd, workDir, snapHook)
		if hookFound && (hookStatus == "closed" || hookStatus == "") {
			return ZombieResult{}, false
		}
	}

	// TOCTOU guard: verify session wasn't recreated since detection.
	if sessionRecreated(t, sessionName, detectedAt) {
		return ZombieResult{}, false
	}

	zombie := ZombieResult{
		PolecatName:    polecatName,
		AgentState:     snapState,
		Classification: ZombieSessionDeadActive,
		HookBead:       snapHook,
		WasActive:      snapHook != "" || typedState.IsActive(),
	}

	// gt-dsgp: Restart instead of nuking. For dirty state, escalate AND restart.
	// gt-2gra: Use snapshot's cleanup status instead of calling getCleanupStatus.
	cleanupStatus := snap.cleanupStatus()
	handleZombieRestart(bd, workDir, rigName, polecatName, snapHook, hookStatus, hookFound, cleanupStatus, &zombie)
	return zombie, true
}

// isZombieState returns true if the agent state or hook bead indicates a zombie.
// Uses typed AgentState to leverage IsActive() metadata rather than hardcoded
// string comparisons. See gt-tsut.
func isZombieState(agentState beads.AgentState, hookBead string) bool {
	if hookBead != "" {
		return true
	}
	return agentState.IsActive()
}

// handleZombieRestart determines the restart action for a confirmed zombie (gt-dsgp).
// Restarts the session regardless of cleanup state. For dirty state, creates a
// cleanup wisp for tracking but does NOT escalate — the witness agent decides
// whether to escalate based on the reported CleanupStatus (ZFC gt-5rne).
// Error chaining (gt-v95d): multiple errors are preserved, not silently dropped.
//
// gt-7vs1: For dirty state, uses create-then-dedup pattern to prevent TOCTOU races
// between concurrent patrol cycles. The cleanup wisp is created first as an atomic
// interlock, then checked for duplicates. Deterministic winner selection (lowest
// wisp ID) ensures exactly one patrol proceeds with the restart.
//
// gt-qnp: If Mayor ACP session is active, vetoes automatic cleanup to allow Mayor review.
//
// hookStatus/hookFound are the bd status of hookBead, already looked up once
// by the caller (gt-evdg) — passing them in avoids a second bd show for the
// same bead on every zombie with a hook.
func handleZombieRestart(bd *BdCli, workDir, rigName, polecatName, hookBead, hookStatus string, hookFound bool, cleanupStatus string, zombie *ZombieResult) {
	zombie.CleanupStatus = cleanupStatus
	skipRestart := false

	// aa-apw: If this polecat's branch work is already merged into the default
	// branch (including via squash merge, which rewrites SHAs and fools a plain
	// ancestor check), do NOT restart. Restarting would let the polecat push its
	// pre-squash HEAD and create a duplicate MR for work already in main.
	// Instead archive the polecat — its work is done.
	//
	// gt-evdg: git evidence alone can't tell "never started" from "merged" —
	// an unclaimed hookBead (status "open") means real work can't have
	// happened, so a "merged" verdict is trusted only when the bead was
	// actually claimed (hooked/in_progress/closed/reaped) or there is no
	// hookBead at all. A failed status lookup is treated the same as "open":
	// unknown is not evidence of done.
	archiveEligible := hookBead == "" || (hookFound && hookStatus != "open")
	if archiveEligible {
		if merged, err := verifyBranchAlreadyMerged(workDir, rigName, polecatName, hookBead); err == nil && merged {
			zombie.Action = "archived-work-already-merged (aa-apw)"
			if nukeErr := nukePolecatFunc(bd, workDir, rigName, polecatName); nukeErr != nil {
				zombie.Error = fmt.Errorf("archive: %w", nukeErr)
				zombie.Action = fmt.Sprintf("archive-failed-work-already-merged: %v", nukeErr)
			}
			return
		}
	}

	// Persistence interlock (gt-qnp): check if Mayor ACP session is active before cleanup.
	townRoot := workDirToTownRoot(workDir)
	if mayor.IsACPActive(townRoot) {
		existingWisp := findAnyCleanupWisp(bd, workDir, rigName, polecatName)
		if existingWisp != "" {
			zombie.Action = fmt.Sprintf("cleanup-deferred-acp (cleanup_status=%s, existing-wisp=%s)", cleanupStatus, existingWisp)
			return
		}
		wispID, wispErr := createCleanupWisp(bd, workDir, rigName, polecatName, hookBead, "")
		if wispErr != nil {
			zombie.Error = wispErr
		}
		zombie.Action = fmt.Sprintf("cleanup-deferred-acp:%s (Mayor ACP session active)", wispID)
		return
	}

	switch cleanupStatus {
	case "clean", "":
		zombie.Action = "restarted"

	case "has_uncommitted", "has_stash", "has_unpushed":
		// Dirty state — create cleanup wisp for tracking if not already tracked.
		// ZFC (gt-5rne): Report data, don't escalate. The witness agent decides policy.

		// Fast path: if a cleanup wisp already exists from a previous patrol cycle,
		// the polecat was already restarted and became zombie again. Just restart.
		existingWisp := findAnyCleanupWisp(bd, workDir, rigName, polecatName)
		if existingWisp != "" {
			zombie.Action = fmt.Sprintf("already-tracked (cleanup_status=%s, existing-wisp=%s)", cleanupStatus, existingWisp)
			break
		}

		// No existing wisp — create one as the atomic interlock (gt-7vs1).
		// Previous code checked then created, allowing two concurrent patrols to
		// both see "no wisp" and create duplicates. Now we create first, then dedup.
		wispID, wispErr := createCleanupWisp(bd, workDir, rigName, polecatName, hookBead, "")
		if wispErr != nil {
			zombie.Error = fmt.Errorf("cleanup wisp: %w", wispErr)
			zombie.Action = fmt.Sprintf("restarted-dirty (cleanup_status=%s, wisp-failed)", cleanupStatus)
			break
		}

		// Dedup: re-check after creation to detect races with concurrent patrols.
		// If another patrol also just created a wisp, there will be >1. Use
		// deterministic winner selection (lowest wisp ID) so exactly one patrol
		// proceeds with the restart and the other cleans up its duplicate.
		allWisps := findAllCleanupWisps(bd, workDir, rigName, polecatName)
		if len(allWisps) > 1 {
			sort.Strings(allWisps)
			if wispID != allWisps[0] {
				// Lost the race — close our duplicate and skip restart to avoid
				// disrupting the session the winning patrol is starting.
				_, _ = bd.Exec(workDir, "close", wispID, "--reason=duplicate: concurrent patrol race (gt-7vs1)")
				zombie.Action = fmt.Sprintf("already-tracked (cleanup_status=%s, existing-wisp=%s, closed-dup=%s)", cleanupStatus, allWisps[0], wispID)
				skipRestart = true
			} else {
				// Won the race — clean up the other patrol's duplicate(s).
				for _, w := range allWisps[1:] {
					_, _ = bd.Exec(workDir, "close", w, "--reason=duplicate: concurrent patrol race (gt-7vs1)")
				}
				zombie.Action = fmt.Sprintf("restarted-dirty (cleanup_status=%s, wisp=%s)", cleanupStatus, wispID)
			}
		} else {
			zombie.Action = fmt.Sprintf("restarted-dirty (cleanup_status=%s, wisp=%s)", cleanupStatus, wispID)
		}
	}

	if skipRestart {
		return
	}

	// Restart regardless of cleanup state — the worktree is preserved.
	if err := RestartPolecatSession(workDir, rigName, polecatName); err != nil {
		if zombie.Error == nil {
			zombie.Error = fmt.Errorf("restart: %w", err)
		} else {
			zombie.Error = fmt.Errorf("%w; also restart: %v", zombie.Error, err)
		}
		if zombie.Action == "restarted" {
			zombie.Action = fmt.Sprintf("restart-failed: %v", err)
		}
	}
}

// SpawnGracePeriod is how long to wait before treating a spawning polecat as a
// potential zombie. Polecats in agent_state=spawning have hook_bead assigned but
// no tmux session yet — this is expected during worktree creation and session
// startup. On large repos (80k+ commits, 4.8GB+) sling can take several minutes.
// Without this guard, the witness classifies spawning polecats as zombies and
// nukes them before they finish starting up. See GH#2036.
const SpawnGracePeriod = 5 * time.Minute

// StalledResult represents a single stalled polecat detection.
type StalledResult struct {
	PolecatName   string // e.g., "alpha"
	StallType     string // "startup-stall", "dialog-blocked", "unknown-prompt"
	Action        string // "auto-dismissed", "escalated", "auto-dismissed-and-escalated"
	AgentState    string // Agent state from beads (e.g., "idle", "working")
	HasHookedWork bool   // Whether this polecat has hooked work assigned
	Error         error
}

// DetectStalledPolecatsResult holds aggregate results.
type DetectStalledPolecatsResult struct {
	Checked int             // Number of live polecats inspected
	Stalled []StalledResult // Stalled polecats found and processed
	Errors  []error         // Transient errors
}

// DetectStalledPolecats checks live polecat sessions for agents stuck at
// startup (e.g., on interactive prompts that block automated sessions).
// Unlike zombie detection which looks for dead sessions/agents, this targets
// alive-but-stuck agents that will never make progress without intervention.
//
// Detection uses structured tmux signals (session creation time + last activity)
// rather than screen-scraping pane content. A session is considered stalled when:
//   - It is older than StartupStallThreshold (90s)
//   - Its last tmux activity is older than StartupActivityGrace (60s)
//
// When a startup stall is detected, DismissStartupDialogsBlind is called to
// send blind key sequences that dismiss known blocking dialogs (workspace trust,
// bypass permissions) without screen-scraping pane content. This avoids coupling
// to third-party TUI strings that can change with any Claude Code update.
func DetectStalledPolecats(workDir, rigName string) *DetectStalledPolecatsResult {
	result := &DetectStalledPolecatsResult{}

	// Find town root for path resolution and session naming
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		townRoot = workDir
	}
	initRegistryFromTownRoot(townRoot)

	// Load witness thresholds from config (fallback to compiled-in defaults).
	witCfg := config.LoadOperationalConfig(townRoot).GetWitnessConfig()
	stallThreshold := witCfg.StartupStallThresholdD()
	activityGrace := witCfg.StartupActivityGraceD()

	// List all polecat directories
	polecatsDir := filepath.Join(townRoot, rigName, "polecats")
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return result // No polecats directory
	}

	t := tmux.NewTmux()
	now := time.Now()

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		polecatName := entry.Name()
		sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
		result.Checked++

		// Pause gate (gt-ahik): a frozen (SIGSTOPped) polecat still has a
		// live session and process, so it looks exactly like a stalled one
		// to every check below — recoverDialogBlockedPolecat would send
		// blind keystrokes into a pane the operator is deliberately holding.
		// See pauseGateSkip's doc.
		if pauseGateSkip(townRoot, rigName, polecatName) {
			continue
		}

		// Only check live sessions with alive agents (the opposite of zombie detection)
		sessionAlive, err := t.HasSession(sessionName)
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Errorf("checking session %s: %w", sessionName, err))
			continue
		}
		if !sessionAlive {
			continue // Dead session — zombie detection handles this
		}
		if !t.IsAgentAlive(sessionName) {
			continue // Dead agent — zombie detection handles this
		}

		// Dialog-blocked check (gt-z83): an interactive question/selection
		// dialog (e.g. AskUserQuestion) can block an unattended session while
		// tmux activity still looks fresh — the dialog redraw itself counts
		// as activity. This must fire unconditionally on every live session,
		// not gated on staleness like the checks below, otherwise the agent
		// stalls silently forever.
		if question, blocked, err := t.DetectBlockingQuestionDialog(sessionName); err == nil && blocked {
			result.Stalled = append(result.Stalled,
				recoverDialogBlockedPolecat(townRoot, rigName, polecatName, sessionName, question, t))
			continue
		}

		// Heartbeat v2 check (gt-3vr5): if the agent has a fresh heartbeat,
		// it's alive and making progress — skip stall detection entirely.
		// This replaces tmux activity scraping for v2 agents.
		if hb := polecat.ReadSessionHeartbeat(townRoot, sessionName); hb != nil && hb.IsV2() {
			if time.Since(hb.Timestamp) < polecat.SessionHeartbeatStaleThreshold {
				continue // Fresh v2 heartbeat — agent is alive, not stalled
			}
		}

		// Legacy: Use structured signals to detect startup stalls:
		// session_created (age) + window_activity (last output).
		//
		// gt-2sln: this used to read #{session_activity}, which tmux only
		// advances for ATTACHED sessions. Every agent session is unattended,
		// so that field freezes at session_created and never moves — the
		// activityAge check below silently collapsed to activityAge ==
		// sessionAge, making it dead code and leaving this gate equivalent
		// to "sessionAge > stallThreshold" alone. That misclassified any
		// agent mid-way through a turn longer than the v2 heartbeat's grace
		// window as a startup stall, on every scan, and fired blind dismiss
		// keystrokes into its live pane. #{window_activity} tracks real pane
		// output and advances correctly for unattended sessions.
		createdUnix, err := t.GetSessionCreatedUnix(sessionName)
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Errorf("getting session created time for %s: %w", sessionName, err))
			continue
		}
		sessionAge := now.Sub(time.Unix(createdUnix, 0))
		if sessionAge < stallThreshold {
			continue // Too young — still in normal startup
		}

		activity, err := t.GetWindowActivity(sessionName)
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Errorf("getting window activity for %s: %w", sessionName, err))
			continue
		}
		activityAge := now.Sub(activity)
		if activityAge < activityGrace {
			continue // Recent activity — agent is making progress
		}

		// Session is old enough and has no recent activity: startup stall.
		// Send blind key sequences to dismiss any startup dialogs without
		// screen-scraping pane content (avoids coupling to third-party TUI strings).
		stalled := StalledResult{
			PolecatName: polecatName,
			StallType:   "startup-stall",
		}
		if err := t.DismissStartupDialogsBlind(sessionName); err != nil {
			stalled.Action = "escalated"
			stalled.Error = fmt.Errorf("blind dismiss failed: %w", err)
		} else {
			stalled.Action = "auto-dismissed"
		}
		result.Stalled = append(result.Stalled, stalled)
	}

	return result
}

// recoverDialogBlockedPolecat implements the gt-z83 recovery sequence for a
// polecat stuck behind an interactive question/selection dialog:
//  1. Send Escape to cancel the blocking tool call — the tool returns
//     "canceled" so no fabricated answer enters the agent's context.
//  2. Nudge the session to decide autonomously, or escalate itself if a
//     human decision is genuinely required.
//  3. Escalate MEDIUM to the mayor with the captured question text and
//     session name, so a human can still answer through the normal channel
//     (attach or reply) when it matters. Fingerprinted on the session name
//     so repeated patrol passes on the same stuck session don't spam
//     duplicate escalation beads.
func recoverDialogBlockedPolecat(townRoot, rigName, polecatName, sessionName, question string, t *tmux.Tmux) StalledResult {
	stalled := StalledResult{
		PolecatName: polecatName,
		StallType:   "dialog-blocked",
	}

	dismissErr := t.DismissBlockingQuestionDialog(sessionName)

	nudgeMsg := "Unattended agent — a blocking interactive dialog was detected and canceled. " +
		"Decide autonomously and continue. If a human decision is genuinely required, " +
		`run "gt escalate -s medium '<question>'" instead of waiting on interactive input.`
	nudgeErr := t.NudgeSession(sessionName, nudgeMsg)

	description := fmt.Sprintf("Dialog-blocked: %s/%s stuck on an interactive question", rigName, polecatName)
	reason := fmt.Sprintf(
		"Session: %s\nCaptured question: %s\n\n"+
			"Auto-recovery: sent Escape to cancel the blocking tool call and nudged the "+
			"agent to proceed autonomously. Attach to the session or reply to this "+
			"escalation if it needs a human answer.",
		sessionName, question)
	escCmd := exec.Command("gt", "escalate", description,
		"-s", "medium",
		"--reason", reason,
		"--source", "witness-patrol:dialog-blocked",
		"--fingerprint", "dialog-blocked:"+sessionName,
	)
	escCmd.Dir = townRoot
	escErr := escCmd.Run()

	switch {
	case dismissErr != nil:
		stalled.Action = "escalated"
		stalled.Error = fmt.Errorf("dismiss failed: %w", dismissErr)
	case escErr != nil:
		stalled.Action = "auto-dismissed"
		stalled.Error = fmt.Errorf("escalate failed: %w", escErr)
	default:
		stalled.Action = "auto-dismissed-and-escalated"
	}
	if nudgeErr != nil && stalled.Error == nil {
		stalled.Error = fmt.Errorf("nudge failed: %w", nudgeErr)
	}

	return stalled
}

// CompletionDiscovery represents a polecat completion discovered from agent bead
// metadata rather than POLECAT_DONE mail. This is the primary discovery mechanism
// for polecat state transitions (gt-w0br).
type CompletionDiscovery struct {
	PolecatName    string
	AgentBeadID    string
	ExitType       string // COMPLETED, ESCALATED, DEFERRED, PHASE_COMPLETE
	IssueID        string // from hook_bead
	MRID           string
	Branch         string
	MRFailed       bool
	PushFailed     bool // True when branch push to origin failed (gas-556)
	CompletionTime string
	Action         string // What was done: "merge-ready-sent", "acknowledged-idle", "phase-complete"
	WispCreated    string // ID of cleanup wisp if created
	Error          error
}

// DiscoverCompletionsResult contains results from scanning agent beads for completions.
type DiscoverCompletionsResult struct {
	Checked    int                   // Number of polecats scanned
	Discovered []CompletionDiscovery // Completions found and processed
	Errors     []error               // Transient errors
}

// discoverCompletionsConcurrency bounds how many polecats are inspected in
// parallel by DiscoverCompletions. Each polecat may trigger a `bd show` call
// and, when an active MR is present, a network-bound `git ls-remote` (capped
// per-call at remoteQueryTimeout by internal/git). Before parallelization this
// loop was strictly serial, so the whole completion-discovery phase cost
// scaled as O(polecat_count × per_call_time) — on rigs with 17-30 polecats
// this blew past the deacon patrol's completion-discovery timeout entirely
// (gt-ftt). A bounded worker pool caps the phase cost at roughly
// O(per_call_time × ceil(polecat_count / discoverCompletionsConcurrency))
// instead, without unbounded fan-out of subprocesses/network calls.
const discoverCompletionsConcurrency = 8

// DiscoverCompletions scans all polecat agent beads for completion metadata
// written by gt done. With self-managed completion (gt-1qlg), gt done itself
// sets agent_state=done and nudges refinery — this is now a SAFETY NET that
// catches crash recovery cases where a polecat wrote completion metadata but
// crashed before finishing its own exit sequence. gt-ho4f: there is no
// done->idle transition anywhere in this flow; "done" is the resting state
// (see polecat.State.IsReuseEligible).
//
// For each polecat with completion metadata (exit_type + completion_time set):
//   - PHASE_COMPLETE: acknowledge (polecat recycled, awaiting gate)
//   - COMPLETED with MR: create cleanup wisp, send MERGE_READY to refinery
//   - COMPLETED without MR: acknowledge completion, remains in "done"
//   - ESCALATED/DEFERRED: acknowledge (polecat remains in "done")
//
// After processing, clears the completion metadata on the agent bead to prevent
// re-processing on the next patrol cycle.
//
// This implements 'Discover Don't Track' (PRIMING.md principle #4): the witness
// observes completion state from beads each cycle rather than relying on mail.
//
// Polecats are inspected concurrently (bounded by discoverCompletionsConcurrency)
// so one polecat with a slow or unreachable git remote can't gate the whole scan.
func DiscoverCompletions(bd *BdCli, workDir, rigName string, router *mail.Router) *DiscoverCompletionsResult {
	result := &DiscoverCompletionsResult{}

	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		townRoot = workDir
	}
	initRegistryFromTownRoot(townRoot)

	polecatsDir := filepath.Join(townRoot, rigName, "polecats")
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return result
	}

	var polecatNames []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		polecatNames = append(polecatNames, entry.Name())
	}

	prefix := beads.GetPrefixForRig(townRoot, rigName)

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, discoverCompletionsConcurrency)

	for _, polecatName := range polecatNames {
		wg.Add(1)
		go func(polecatName string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)

			// Get full agent fields including completion metadata
			fields := getAgentBeadFields(workDir, agentBeadID)
			if fields == nil || fields.ExitType == "" || fields.CompletionTime == "" {
				mu.Lock()
				result.Checked++
				mu.Unlock()
				return // No completion metadata — skip
			}

			sourceIssue := fields.LastSourceIssue
			if sourceIssue == "" {
				sourceIssue = fields.HookBead
			}

			discovery := CompletionDiscovery{
				PolecatName:    polecatName,
				AgentBeadID:    agentBeadID,
				ExitType:       fields.ExitType,
				IssueID:        sourceIssue,
				MRID:           fields.MRID,
				Branch:         fields.Branch,
				MRFailed:       fields.MRFailed,
				PushFailed:     fields.PushFailed,
				CompletionTime: fields.CompletionTime,
			}

			// Build a payload compatible with the existing routing logic
			payload := &PolecatDonePayload{
				PolecatName: polecatName,
				Exit:        fields.ExitType,
				IssueID:     sourceIssue,
				MRID:        fields.MRID,
				Branch:      fields.Branch,
				MRFailed:    fields.MRFailed,
				PushFailed:  fields.PushFailed,
			}

			// Route based on exit type and MR presence
			processDiscoveredCompletion(bd, workDir, rigName, payload, &discovery)

			// Clear completion metadata only after successful processing. If cleanup
			// wisp creation/update failed, leave metadata for the next patrol retry.
			var clearErr error
			if discovery.Error == nil {
				if err := clearCompletionMetadata(workDir, agentBeadID); err != nil {
					clearErr = fmt.Errorf("clearing completion metadata for %s: %w", polecatName, err)
				}
			}

			mu.Lock()
			result.Checked++
			result.Discovered = append(result.Discovered, discovery)
			if clearErr != nil {
				result.Errors = append(result.Errors, clearErr)
			}
			mu.Unlock()
		}(polecatName)
	}

	wg.Wait()

	return result
}

// processDiscoveredCompletion routes a discovered completion through the same
// logic as HandlePolecatDone, creating cleanup wisps and sending MERGE_READY
// as appropriate. This is the bead-based equivalent of POLECAT_DONE mail handling.
func processDiscoveredCompletion(bd *BdCli, workDir, rigName string, payload *PolecatDonePayload, discovery *CompletionDiscovery) {
	if payload.Exit == string(ExitTypePhaseComplete) {
		discovery.Action = "phase-complete"
		return
	}

	// Push failed: branch never reached origin. Work is committed locally only.
	// The polecat's worktree may be in /tmp and lost on reboot. Escalate so the
	// witness agent can investigate and trigger recovery (gas-556).
	if payload.PushFailed {
		discovery.Action = fmt.Sprintf("push-failed-recovery-needed (branch=%s issue=%s) — branch not on origin, worktree may be at risk",
			payload.Branch, payload.IssueID)
		// Notify mayor so a new polecat can be dispatched if work is lost.
		townRoot, _ := workspace.Find(workDir)
		if townRoot != "" {
			mayorMsg := fmt.Sprintf("PUSH_FAILED: polecat=%s branch=%s issue=%s — branch not on origin, possible work loss",
				payload.PolecatName, payload.Branch, payload.IssueID)
			mayorSession := session.MayorSessionName()
			t := tmux.NewTmux()
			if running, err := t.HasSession(mayorSession); err == nil && running {
				_ = t.NudgeSession(mayorSession, mayorMsg)
			}
		}
		return
	}

	hasMR := false
	if payload.MRID != "" {
		assessment := polecat.AssessActiveMR(beadCLIShower{bd: bd, workDir: workDir}, polecat.ActiveMRInput{
			ActiveMR:        payload.MRID,
			SourceIssueHint: payload.IssueID,
			RequireGitSafe:  true,
			GitSafe:         activeMRGitSafe(workDir, rigName, payload.PolecatName),
		})
		hasMR = assessment.Pending
	}

	// When Exit==COMPLETED but MRID is empty and MR creation didn't explicitly
	// fail, query beads to check if an MR bead exists for this branch.
	if !hasMR && payload.Exit == string(ExitTypeCompleted) && !payload.MRFailed && payload.Branch != "" {
		if mrID := findMRBeadForBranch(bd, workDir, payload.Branch); mrID != "" {
			payload.MRID = mrID
			hasMR = true
		}
	}

	if hasMR {
		// Idempotency (gt-mf5q): completion discovery is not atomic with the
		// metadata clear that follows it below, so the same completion can be
		// rediscovered — by a concurrent patrol scan, or by a later cycle if
		// the clear failed to persist — and must not produce a second wisp
		// for work already tracked.
		wispID, isNew, closedDups, err := ensureCleanupWisp(bd, workDir, rigName, payload)
		if err != nil {
			discovery.Error = fmt.Errorf("creating cleanup wisp: %w", err)
			return
		}
		discovery.WispCreated = wispID

		// Always (re)confirm the wisp's state transition, even when the wisp
		// already existed. Short-circuiting here on an already-tracked wisp
		// without calling UpdateCleanupWispState would mean a state-update
		// failure on the cycle that created the wisp could never be
		// retried — the wisp would be stranded at state:pending forever, a
		// state findCleanupWisp (which filters on state:merge-requested)
		// never matches, and the failure goes silent the moment a later,
		// error-free cycle clears the metadata that would have triggered
		// the retry (gt-mf5q review case 2).
		if err := UpdateCleanupWispState(bd, workDir, wispID, "merge-requested"); err != nil {
			discovery.Error = fmt.Errorf("updating wisp state: %w", err)
		}

		// Nudge refinery to check merge queue (no permanent mail needed). A
		// nudge failure is non-fatal and must NOT block clearing completion
		// metadata: the wisp above is already created and tracked, so
		// leaving metadata set would only cause this completion to be
		// rediscovered — and re-idempotency-checked into a retry of the
		// state update, not the nudge specifically — on the next cycle.
		townRoot, _ := workspace.Find(workDir)
		nudgeErr := nudgeRefinery(townRoot, rigName)

		verb := "merge-ready-nudged"
		if !isNew {
			verb = "already-tracked"
		}
		discovery.Action = fmt.Sprintf("%s (MR=%s, wisp=%s)", verb, payload.MRID, wispID)
		if len(closedDups) > 0 {
			discovery.Action += fmt.Sprintf(", closed-dup=%s", strings.Join(closedDups, ","))
		}
		if nudgeErr != nil {
			discovery.Action += fmt.Sprintf(", nudge-failed=%v", nudgeErr)
		}

		// Notify Mayor that a slot is open even with pending MR — polecat is idle. (GH#2727)
		notifyMayorSlotOpen(workDir, rigName, payload.PolecatName, payload.Exit)
		return
	}

	// No MR — polecat is idle (persistent polecat model, gt-4ac)
	discovery.Action = fmt.Sprintf("acknowledged-idle (exit=%s)", payload.Exit)

	// Notify Mayor that a slot is open (bead-based discovery path). (GH#2727)
	notifyMayorSlotOpen(workDir, rigName, payload.PolecatName, payload.Exit)
}

// agentBeadSnapshot holds all fields from a single bd show --json call for an agent bead.
// Used to avoid redundant subprocess invocations during zombie detection, where the same
// agent bead was previously queried 3-5 times per polecat per patrol cycle. (gt-2gra)
type agentBeadSnapshot struct {
	AgentState string
	HookBead   string
	Labels     []string
	UpdatedAt  string
	ActiveMR   string
	Fields     *beads.AgentFields // parsed from description
}

// fetchAgentBeadSnapshot fetches all agent bead data in a single bd show call.
// Returns nil if the bead doesn't exist or can't be queried.
func fetchAgentBeadSnapshot(workDir, agentBeadID string) *agentBeadSnapshot {
	issue, fields, err := beads.New(workDir).ForAgentBead().GetAgentBead(agentBeadID)
	if err != nil || issue == nil || fields == nil {
		return nil
	}

	return &agentBeadSnapshot{
		AgentState: fields.AgentState,
		HookBead:   issue.HookBead,
		Labels:     issue.Labels,
		UpdatedAt:  issue.UpdatedAt,
		ActiveMR:   fields.ActiveMR,
		Fields:     fields,
	}
}

// snapshotAge returns the time since the agent bead was last updated.
// Returns a large duration if the timestamp can't be parsed, so callers
// don't accidentally skip zombie detection on parse failure.
func (s *agentBeadSnapshot) age() time.Duration {
	if s == nil || s.UpdatedAt == "" {
		return 24 * time.Hour
	}
	updatedAt, err := time.Parse(time.RFC3339, s.UpdatedAt)
	if err != nil {
		updatedAt, err = time.Parse("2006-01-02 15:04:05", s.UpdatedAt)
		if err != nil {
			return 24 * time.Hour
		}
	}
	return time.Since(updatedAt)
}

// cleanupStatus returns the cleanup_status from the agent bead's description fields.
func (s *agentBeadSnapshot) cleanupStatus() string {
	if s == nil || s.Fields == nil {
		return ""
	}
	return s.Fields.CleanupStatus
}

// getAgentBeadFields reads the full agent description fields from an agent bead,
// including completion metadata (exit_type, mr_id, branch, mr_failed, completion_time).
// Returns nil if the bead doesn't exist or can't be parsed.
func getAgentBeadFields(workDir, agentBeadID string) *beads.AgentFields {
	_, fields, err := beads.New(workDir).ForAgentBead().GetAgentBead(agentBeadID)
	if err != nil {
		return nil
	}
	return fields
}

// clearCompletionMetadata removes completion metadata fields from an agent
// bead so the same completion is not re-processed on the next patrol cycle.
// It goes through the agent-scoped wrapper (never shell `bd update`): a
// cwd-routed update from the town root landed on the legacy town row and
// left the canonical rig row stale (gt-a6g).
func clearCompletionMetadata(workDir, agentBeadID string) error {
	bd := beads.New(workDir).ForAgentBead()
	issue, fields, err := bd.GetAgentBead(agentBeadID)
	if err != nil {
		return fmt.Errorf("reading agent bead %s: %w", agentBeadID, err)
	}
	if issue == nil || fields == nil {
		return fmt.Errorf("reading agent bead %s: %w", agentBeadID, beads.ErrNotFound)
	}

	empty := ""
	updates := beads.AgentFieldUpdates{
		ExitType:       &empty,
		MRID:           &empty,
		CompletionTime: &empty,
	}
	if !fields.MRFailed && !fields.PushFailed {
		updates.Branch = &empty
	}
	return bd.UpdateAgentDescriptionFields(agentBeadID, updates)
}

// getBeadStatus returns the status of a bead (e.g., "open", "closed", "hooked").
// Returns the status string and true if the lookup succeeded, or ("", false) if
// the bead couldn't be queried (network error, cross-rig routing failure, etc.).
// Callers must check the bool to distinguish "bead not found/reaped" from "lookup error."
func getBeadStatus(bd *BdCli, workDir, beadID string) (string, bool) {
	if beadID == "" {
		return "", false
	}
	output, err := bd.Exec(workDir, "show", beadID, "--json")
	if err != nil || output == "" {
		return "", false
	}
	var issues []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(output), &issues); err != nil || len(issues) == 0 {
		// Valid response but no results — bead was reaped/deleted.
		return "", true
	}
	return issues[0].Status, true
}

// survivingWorkForBead is the shared surviving-work predicate
// (polecat.WorkSurvival) for a bead in rigName. A seam for tests.
var survivingWorkForBead = func(workDir, rigName, beadID string) (string, error) {
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		townRoot = workDir
	}
	return polecat.SurvivingWorkForIssue(filepath.Join(townRoot, rigName), beadID)
}

// resetAbandonedBead resets a dead polecat's hooked bead so it can be re-dispatched.
// If the bead is in "hooked" or "in_progress" status, it:
//  0. Checks if the polecat's work is already on main — if so, closes
//     the bead instead of resetting (prevents re-dispatch of completed work)
//  1. Records the respawn in the witness spawn-count ledger
//  2. Resets status to open
//  3. Clears assignee
//  4. Sends mail to deacon for re-dispatch (includes respawn count; SPAWN_STORM
//     prefix and Urgent priority when count exceeds max bead respawns config)
//
// Returns true if the bead was recovered.
func resetAbandonedBead(bd *BdCli, workDir, rigName, hookBead, polecatName string, router *mail.Router) bool {
	if hookBead == "" {
		return false
	}
	status, ok := getBeadStatus(bd, workDir, hookBead)
	if !ok || (status != "hooked" && status != "in_progress") {
		return false
	}

	// Load max respawns threshold from config.
	trRoot, trErr := workspace.Find(workDir)
	if trErr != nil || trRoot == "" {
		trRoot = workDir
	}
	maxRespawns := config.LoadOperationalConfig(trRoot).GetWitnessConfig().MaxBeadRespawnsV()

	// Guard: if the polecat's commit is already on the default branch,
	// the work is done — close the bead instead of resetting for re-dispatch.
	// This prevents the spawn-storm / duplicate-work loop described in #2036.
	if onMain, err := verifyCommitOnMain(workDir, rigName, polecatName); err == nil && onMain {
		reason := fmt.Sprintf("Work already on main (verified by witness, polecat %s)", polecatName)
		if err := bd.Run(workDir, "close", hookBead, "-r", reason); err != nil {
			fmt.Fprintf(os.Stderr, "witness: failed to close bead %s (work already on main): %v\n", hookBead, err)
		}
		return false
	}

	// Guard: work that survives on a polecat branch (unmerged patches) keeps
	// the hook. Resetting it would let the deacon re-dispatch the bead to a
	// fresh polecat starting from main over that work (gt-ibt8, gt-da2x). The
	// re-sling guard in gt sling then points the operator at --branch. An
	// unknown answer also keeps the hook; a rig with no git repo has no branch
	// to protect.
	if branch, err := survivingWorkForBead(workDir, rigName, hookBead); err != nil && !errors.Is(err, polecat.ErrNoRigRepo) {
		fmt.Fprintf(os.Stderr, "witness: keeping %s hooked to %s/%s: could not check for surviving work: %v\n", hookBead, rigName, polecatName, err)
		return false
	} else if branch != "" {
		fmt.Fprintf(os.Stderr, "witness: keeping %s hooked to %s/%s: work survives on %s (resume with gt sling %s %s --branch %s)\n",
			hookBead, rigName, polecatName, branch, hookBead, rigName, branch)
		return false
	}

	// Circuit breaker (clown show #22): if this bead has already been
	// respawned too many times, escalate to mayor instead of re-dispatching.
	// This prevents the witness→deacon→spawn feedback loop from creating
	// unbounded polecats when a task repeatedly kills its polecat.
	if ShouldBlockRespawn(workDir, hookBead) {
		if router != nil {
			msg := &mail.Message{
				From:     fmt.Sprintf("%s/witness", rigName),
				To:       "mayor/",
				Subject:  fmt.Sprintf("SPAWN_BLOCKED %s (respawn limit reached)", hookBead),
				Priority: mail.PriorityUrgent,
				Body: fmt.Sprintf(`Bead %s has been respawned %d+ times and keeps failing.
Re-dispatch blocked to prevent spawn storm.

Polecat: %s/%s
Previous Status: %s

Action required: investigate why this task keeps killing its polecat,
then either close the bead or reset the respawn counter.`,
					hookBead, maxRespawns, rigName, polecatName, status),
			}
			if err := router.Send(msg); err != nil {
				fmt.Fprintf(os.Stderr, "witness: failed to send SPAWN_BLOCKED mail for %s: %v, attempting nudge fallback\n", hookBead, err)
				// Nudge mayor as fallback — nudges are more reliable than mail
				t := tmux.NewTmux()
				nudgeMsg := fmt.Sprintf("SPAWN_BLOCKED %s (respawn limit reached) from %s/%s — mail send failed, investigate spawn storm",
					hookBead, rigName, polecatName)
				if nudgeErr := t.NudgeSession(session.MayorSessionName(), nudgeMsg); nudgeErr != nil {
					fmt.Fprintf(os.Stderr, "witness: nudge fallback to mayor also failed for %s: %v\n", hookBead, nudgeErr)
				}
			}
		}
		return false
	}

	// Track respawn count for audit and storm detection.
	respawnCount := RecordBeadRespawn(workDir, hookBead)

	// Reset bead status to open and clear assignee — guarded on the dead
	// polecat still holding it (--if-assignee): a bead re-slung since the scan
	// stays with its new owner, and the guarded write is the claim transfer bd
	// permits on an in_progress bead that another agent holds.
	if err := bd.Run(workDir, "update", hookBead, "--status=open", "--assignee=",
		"--if-assignee="+fmt.Sprintf("%s/polecats/%s", rigName, polecatName)); err != nil {
		return false
	}

	// Send mail to deacon for re-dispatch
	if router != nil {
		subject := fmt.Sprintf("RECOVERED_BEAD %s", hookBead)
		priority := mail.PriorityHigh
		stormNote := ""
		if respawnCount >= maxRespawns {
			subject = fmt.Sprintf("SPAWN_STORM RECOVERED_BEAD %s (respawned %dx)", hookBead, respawnCount)
			priority = mail.PriorityUrgent
			stormNote = fmt.Sprintf("\n\n⚠️ SPAWN STORM: bead has been reset %d times. "+
				"Next respawn will be BLOCKED. "+
				"Check polecat completion protocol or close the bead manually.",
				respawnCount)
		}
		msg := &mail.Message{
			From:     fmt.Sprintf("%s/witness", rigName),
			To:       "deacon/",
			Subject:  subject,
			Priority: priority,
			Body: fmt.Sprintf(`Recovered abandoned bead from dead polecat.

Bead: %s
Polecat: %s/%s
Previous Status: %s
Respawn Count: %d%s

The bead has been reset to open with no assignee.
Please re-dispatch to an available polecat.`,
				hookBead, rigName, polecatName, status, respawnCount, stormNote),
		}
		if err := router.Send(msg); err != nil {
			fmt.Fprintf(os.Stderr, "witness: failed to send RECOVERED_BEAD mail for %s: %v, attempting nudge fallback\n", hookBead, err)
			// Nudge deacon as fallback — nudges are more reliable than mail
			t := tmux.NewTmux()
			nudgeMsg := fmt.Sprintf("RECOVERED_BEAD %s from %s/%s (status=%s, respawns=%d) — mail send failed, please re-dispatch",
				hookBead, rigName, polecatName, status, respawnCount)
			if nudgeErr := t.NudgeSession(session.DeaconSessionName(), nudgeMsg); nudgeErr != nil {
				fmt.Fprintf(os.Stderr, "witness: nudge fallback to deacon also failed for %s: %v\n", hookBead, nudgeErr)
			}
		}
	}

	return true
}

// OrphanedBeadResult contains a single detected orphaned bead.
type OrphanedBeadResult struct {
	BeadID        string
	Assignee      string // Original assignee (e.g. "gastown/polecats/alpha")
	PolecatName   string // Extracted polecat name
	BeadRecovered bool
}

// DetectOrphanedBeadsResult contains the results of an orphaned bead scan.
type DetectOrphanedBeadsResult struct {
	Checked int
	Orphans []OrphanedBeadResult
	Errors  []error
}

// DetectOrphanedBeads finds in_progress or hooked beads assigned to non-existent polecats.
//
// This complements DetectZombiePolecats which scans FROM polecat directories.
// If a polecat was nuked and its directory removed, DetectZombiePolecats won't
// see it, but the bead remains in_progress/hooked. This function scans FROM
// beads to catch that case.
func DetectOrphanedBeads(bd *BdCli, workDir, rigName string, router *mail.Router) *DetectOrphanedBeadsResult {
	result := &DetectOrphanedBeadsResult{}

	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		townRoot = workDir
	}
	initRegistryFromTownRoot(townRoot)

	// Scan both in_progress and hooked beads — resetAbandonedBead handles both
	// states, and orphaned beads can be stuck in either.
	var beadList []struct {
		ID       string `json:"id"`
		Assignee string `json:"assignee"`
	}
	for _, status := range []string{"in_progress", "hooked"} {
		output, err := bd.Exec(workDir, "list", "--status="+status, "--json", "--limit=0")
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("listing %s beads: %w", status, err))
			continue
		}
		if output == "" {
			continue
		}
		var batch []struct {
			ID       string `json:"id"`
			Assignee string `json:"assignee"`
		}
		if err := json.Unmarshal([]byte(output), &batch); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("parsing %s beads: %w", status, err))
			continue
		}
		beadList = append(beadList, batch...)
	}

	t := tmux.NewTmux()

	for _, bead := range beadList {
		if bead.Assignee == "" {
			continue // No assignee — not a dead-polecat orphan
		}

		// Parse assignee: "rigname/polecats/polecatname"
		parts := strings.Split(bead.Assignee, "/")
		if len(parts) != 3 || parts[1] != "polecats" {
			continue // Not a polecat assignee (crew, refinery, etc.)
		}
		assigneeRig := parts[0]
		polecatName := parts[2]

		// Only check beads assigned to polecats in this rig
		if assigneeRig != rigName {
			continue
		}
		result.Checked++

		// Check if the polecat's tmux session exists
		sessionName := session.PolecatSessionName(session.PrefixFor(assigneeRig), polecatName)
		sessionAlive, err := t.HasSession(sessionName)
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Errorf("checking session %s for bead %s: %w", sessionName, bead.ID, err))
			continue
		}
		if sessionAlive {
			continue // Polecat is alive — not an orphan
		}

		// Session is dead. Also check if polecat directory still exists
		// (if dir exists, DetectZombiePolecats will handle it)
		polecatsDir := filepath.Join(townRoot, assigneeRig, "polecats", polecatName)
		if _, statErr := os.Stat(polecatsDir); statErr == nil {
			continue // Directory exists — DetectZombiePolecats handles this case
		} else if !os.IsNotExist(statErr) {
			// Transient error (permission denied, I/O error) — skip to avoid false recovery
			result.Errors = append(result.Errors,
				fmt.Errorf("checking polecat dir %s for bead %s: %w", polecatsDir, bead.ID, statErr))
			continue
		}

		// Re-check directory and session immediately before reset to narrow the
		// TOCTOU window — a polecat could have been recreated between the first
		// checks and now.
		if _, statErr := os.Stat(polecatsDir); statErr == nil {
			continue // Directory reappeared — skip, not an orphan anymore
		} else if !os.IsNotExist(statErr) {
			result.Errors = append(result.Errors,
				fmt.Errorf("re-checking polecat dir %s for bead %s: %w", polecatsDir, bead.ID, statErr))
			continue
		}
		if alive, _ := t.HasSession(sessionName); alive {
			continue // Session reappeared — polecat was respawned, not an orphan
		}

		// Polecat is truly gone (no session, no directory). Reset the bead.
		orphan := OrphanedBeadResult{
			BeadID:      bead.ID,
			Assignee:    bead.Assignee,
			PolecatName: polecatName,
		}
		orphan.BeadRecovered = resetAbandonedBead(bd, workDir, assigneeRig, bead.ID, polecatName, router)
		result.Orphans = append(result.Orphans, orphan)
	}

	return result
}

// OrphanedMoleculeResult represents a single orphaned molecule detection.
type OrphanedMoleculeResult struct {
	BeadID        string // The base work bead with the orphaned molecule
	MoleculeID    string // The attached molecule (wisp) ID
	Assignee      string // The dead polecat's full address
	PolecatName   string // Just the polecat name
	Closed        int    // Number of issues closed (molecule + descendants)
	BeadRecovered bool   // Whether the parent bead was reset for re-dispatch
	Error         error
}

// DetectOrphanedMoleculesResult holds aggregate results of the orphan scan.
type DetectOrphanedMoleculesResult struct {
	Checked int                      // Number of polecat-assigned beads checked
	Orphans []OrphanedMoleculeResult // Orphaned molecules found and processed
	Errors  []error
}

// DetectOrphanedMolecules scans for mol-polecat-work molecule instances whose
// owning polecat no longer exists. For each orphaned molecule, it closes the
// molecule and its descendant step issues, unblocking the parent work bead.
//
// Detection chain: hooked/in_progress bead → polecat assignee → check existence →
// read attached_molecule → close molecule + descendants.
//
// This complements DetectZombiePolecats (which scans FROM polecat directories)
// by scanning FROM beads. Once a polecat is nuked and its directory removed,
// DetectZombiePolecats can't see it — but the orphaned molecules remain.
//
// See: https://github.com/steveyegge/gastown/issues/1381
func DetectOrphanedMolecules(bd *BdCli, workDir, rigName string, router *mail.Router) *DetectOrphanedMoleculesResult {
	result := &DetectOrphanedMoleculesResult{}

	// Find town root for path resolution and session naming
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		townRoot = workDir
	}
	initRegistryFromTownRoot(townRoot)

	// Step 1: List beads that could have attached molecules.
	// Slung beads start as status=hooked; polecats may change them to in_progress.
	type beadSummary struct {
		ID       string `json:"id"`
		Assignee string `json:"assignee"`
	}
	var allBeads []beadSummary
	for _, status := range []string{"hooked", "in_progress"} {
		output, err := bd.Exec(workDir, "list", "--status="+status, "--json", "--limit=0")
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("listing %s beads: %w", status, err))
			continue
		}
		if output == "" {
			continue
		}
		var items []beadSummary
		if err := json.Unmarshal([]byte(output), &items); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("parsing %s beads: %w", status, err))
			continue
		}
		allBeads = append(allBeads, items...)
	}

	if len(allBeads) == 0 {
		return result
	}

	// Step 2: Check each polecat-assigned bead
	polecatPrefix := rigName + "/polecats/"
	t := tmux.NewTmux()
	polecatsDir := filepath.Join(townRoot, rigName, "polecats")

	for _, b := range allBeads {
		if !strings.HasPrefix(b.Assignee, polecatPrefix) {
			continue
		}

		polecatName := strings.TrimPrefix(b.Assignee, polecatPrefix)
		result.Checked++

		// Check if polecat still has a tmux session
		sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
		hasSession, sessionErr := t.HasSession(sessionName)
		if sessionErr != nil {
			result.Errors = append(result.Errors,
				fmt.Errorf("checking session %s for bead %s: %w", sessionName, b.ID, sessionErr))
			continue
		}
		if hasSession {
			continue // Polecat is alive
		}

		// Check if polecat directory still exists (might be mid-cleanup)
		polecatDir := filepath.Join(polecatsDir, polecatName)
		if _, statErr := os.Stat(polecatDir); statErr == nil {
			continue // Directory exists; DetectZombiePolecats handles these
		} else if !os.IsNotExist(statErr) {
			// Transient error (permission denied, I/O error) — skip to avoid false positive
			result.Errors = append(result.Errors,
				fmt.Errorf("checking polecat dir %s for bead %s: %w", polecatDir, b.ID, statErr))
			continue
		}

		// TOCTOU re-check: polecat could have been recreated between initial
		// checks and now. Re-verify before destructive action.
		if _, statErr := os.Stat(polecatDir); statErr == nil {
			continue // Directory reappeared — skip
		} else if !os.IsNotExist(statErr) {
			result.Errors = append(result.Errors,
				fmt.Errorf("re-checking polecat dir %s for bead %s: %w", polecatDir, b.ID, statErr))
			continue
		}
		if alive, _ := t.HasSession(sessionName); alive {
			continue // Session reappeared — polecat was respawned
		}

		// Polecat is dead and gone — read the full bead to check for attached molecule
		attachedMol := getAttachedMoleculeID(bd, workDir, b.ID)
		if attachedMol == "" {
			continue // No molecule attached
		}

		// Check molecule status — skip if already closed or reaped.
		// On lookup failure, skip (safe — don't close what we can't verify).
		molStatus, molFound := getBeadStatus(bd, workDir, attachedMol)
		if !molFound || molStatus == "closed" || molStatus == "" {
			continue
		}

		// Close the orphaned molecule and its descendants
		orphan := OrphanedMoleculeResult{
			BeadID:      b.ID,
			MoleculeID:  attachedMol,
			Assignee:    b.Assignee,
			PolecatName: polecatName,
		}

		closed, closeErr := closeMoleculeWithDescendants(bd, workDir, attachedMol)
		if closeErr != nil {
			orphan.Error = closeErr
			result.Errors = append(result.Errors, closeErr)
		}
		orphan.Closed = closed

		// Reset the parent bead so it can be re-dispatched
		orphan.BeadRecovered = resetAbandonedBead(bd, workDir, rigName, b.ID, polecatName, router)

		result.Orphans = append(result.Orphans, orphan)
	}

	return result
}

// getAttachedMoleculeID reads a bead and returns its attached_molecule ID, if any.
func getAttachedMoleculeID(bd *BdCli, workDir, beadID string) string {
	output, err := bd.Exec(workDir, "show", beadID, "--json")
	if err != nil || output == "" {
		return ""
	}

	var issues []struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(output), &issues); err != nil || len(issues) == 0 {
		return ""
	}

	fields := beads.ParseAttachmentFields(&beads.Issue{Description: issues[0].Description})
	if fields == nil {
		return ""
	}
	return fields.AttachedMolecule
}

// closeMoleculeWithDescendants closes a molecule and all its descendant step
// issues using the bd CLI. Returns the total number of issues closed.
func closeMoleculeWithDescendants(bd *BdCli, workDir, moleculeID string) (int, error) {
	// Recursively close descendants first (bottom-up)
	closed, descErr := closeDescendantsViaCLI(bd, workDir, moleculeID)

	// Close the molecule itself
	reason := "Orphaned mol-polecat-work — owning polecat no longer exists (issue #1381)"
	if err := bd.Run(workDir, "close", moleculeID, "-r", reason); err != nil {
		closeErr := fmt.Errorf("closing molecule %s: %w", moleculeID, err)
		if descErr != nil {
			return closed, fmt.Errorf("%w; also: %v", closeErr, descErr)
		}
		return closed, closeErr
	}
	closed++

	return closed, descErr
}

// closeDescendantsViaCLI recursively closes descendant issues of a parent
// using bd CLI commands. Returns count of issues closed and any error.
func closeDescendantsViaCLI(bd *BdCli, workDir, parentID string) (int, error) {
	// List children of this parent
	output, err := bd.Exec(workDir, "list", "--parent="+parentID, "--json")
	if err != nil {
		return 0, fmt.Errorf("listing children of %s: %w", parentID, err)
	}
	if output == "" {
		return 0, nil
	}

	var children []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(output), &children); err != nil {
		return 0, fmt.Errorf("parsing children of %s: %w", parentID, err)
	}

	if len(children) == 0 {
		return 0, nil
	}

	// Recursively close grandchildren first
	totalClosed := 0
	var errs []error
	for _, child := range children {
		n, err := closeDescendantsViaCLI(bd, workDir, child.ID)
		totalClosed += n
		if err != nil {
			errs = append(errs, err)
		}
	}

	// Close open direct children
	var idsToClose []string
	for _, child := range children {
		if child.Status != "closed" {
			idsToClose = append(idsToClose, child.ID)
		}
	}

	if len(idsToClose) > 0 {
		reason := "Orphaned mol-polecat-work step — owning polecat no longer exists"
		args := append([]string{"close"}, idsToClose...)
		args = append(args, "-r", reason)
		if err := bd.Run(workDir, args...); err != nil {
			errs = append(errs, fmt.Errorf("closing children of %s: %w", parentID, err))
		} else {
			totalClosed += len(idsToClose)
		}
	}

	if len(errs) > 0 {
		return totalClosed, errs[0]
	}
	return totalClosed, nil
}

// DoneIntent represents a parsed done-intent label from an agent bead.
type DoneIntent struct {
	ExitType  string
	Timestamp time.Time
}

// extractDoneIntent parses a done-intent:<type>:<unix-ts> label from a label list.
// When multiple done-intent labels exist (from repeated gt done attempts), returns
// the one with the NEWEST timestamp — the live intent supersedes stale ones.
// Returns nil if no done-intent label is found or if all are malformed.
func extractDoneIntent(labels []string) *DoneIntent {
	var best *DoneIntent
	for _, label := range labels {
		if !strings.HasPrefix(label, "done-intent:") {
			continue
		}
		// Format: done-intent:<type>:<unix-ts>
		parts := strings.SplitN(label, ":", 3)
		if len(parts) != 3 {
			continue // Malformed — skip, don't abort
		}
		ts, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			continue // Malformed timestamp — skip
		}
		candidate := &DoneIntent{
			ExitType:  parts[1],
			Timestamp: time.Unix(ts, 0),
		}
		// Keep the newest label (largest timestamp).
		if best == nil || candidate.Timestamp.After(best.Timestamp) {
			best = candidate
		}
	}
	return best
}

// doneIntentWorthRestarting reports whether a dead session's done-intent is a
// crashed exit a restart could still recover, rather than residue on an agent
// bead whose work is gone: an idle or unhooked polecat has nothing to resume,
// a done-cp:witness-notified checkpoint means the completion already reached
// the witness, and past maxAge the recorded attempt is moot (gt-jv7v).
//
// Restarting needs positive evidence of resumable work: an unreadable agent
// bead (nil snap) is not evidence, so nothing is restarted on one.
//
// Neither is a hook held under a state that marks a deliberate hold. That hook
// belongs to the hold, so restarting on the label alone breaks the hold and
// costs a re-sling (gt-vql3). An empty agent_state is no hold, so legacy beads
// keep the crash-recovery path.
func doneIntentWorthRestarting(snap *agentBeadSnapshot, age, maxAge time.Duration) bool {
	if snap == nil {
		return false
	}
	state := beads.AgentState(snap.AgentState)
	if snap.HookBead == "" || state == AgentStateIdle {
		return false
	}
	if state.ProtectsFromCleanup() {
		return false
	}
	if hasDoneCheckpoint(snap, "witness-notified") {
		return false
	}
	return age <= maxAge
}

// hasDoneCheckpoint reports whether the agent bead carries a done-cp:<stage>
// label. gt done writes them as done-cp:<stage>:<value>:<unix-ts>; the witness
// reads the stage only, so matching the prefix avoids importing internal/cmd
// for the constant (gt-jv7v).
func hasDoneCheckpoint(snap *agentBeadSnapshot, stage string) bool {
	if snap == nil {
		return false
	}
	prefix := "done-cp:" + stage + ":"
	for _, label := range snap.Labels {
		if strings.HasPrefix(label, prefix) {
			return true
		}
	}
	return false
}

// beadUpdater is a minimal interface for updating agent bead labels.
// Used to make clearAllDoneIntentLabels testable without mocking the full bd CLI.
type beadUpdater interface {
	Update(id string, opts beads.UpdateOptions) error
}

// clearAllDoneIntentLabels removes ALL done-intent:* labels from the agent bead
// before restart. It uses beads.New(workDir).ForAgentBead() for correct routing
// (raw shell bd update lands on the wrong database for cwd-routed calls, gt-a6g).
// Labels are derived from snap (the caller's pre-fetched snapshot) to avoid a
// redundant bd show subprocess call.
func clearAllDoneIntentLabels(upder beadUpdater, agentBeadID string, snap *agentBeadSnapshot) {
	if agentBeadID == "" || snap == nil {
		return
	}
	var toRemove []string
	for _, label := range snap.Labels {
		if strings.HasPrefix(label, "done-intent:") {
			toRemove = append(toRemove, label)
		}
	}
	if len(toRemove) == 0 {
		return
	}
	if err := upder.Update(agentBeadID, beads.UpdateOptions{
		RemoveLabels: toRemove,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "witness: clear done-intent labels on %s: %v\n", agentBeadID, err)
	}
}

// sessionRecreated checks whether a tmux session was (re)created after the
// given timestamp. Returns true if the session exists and was created after
// detectedAt, indicating a new session replaced the dead one (TOCTOU guard).
func sessionRecreated(t *tmux.Tmux, sessionName string, detectedAt time.Time) bool {
	alive, err := t.HasSession(sessionName)
	if err != nil || !alive {
		return false // Still dead — not recreated
	}
	// Session exists now. Check if it was created after our detection.
	createdAt, err := session.SessionCreatedAt(sessionName)
	if err != nil {
		// Can't determine creation time — assume recreated to be safe.
		// Better to skip a real zombie than kill a live session.
		return true
	}
	return !createdAt.Before(detectedAt)
}

// findAnyCleanupWisp checks if any cleanup wisp already exists for a polecat,
// regardless of state. Used to prevent duplicate escalation on repeated patrol
// cycles for the same zombie. Scoped to rigName via assignee so a same-named
// polecat in another rig can't match (gt-gsrz: see CleanupWispAssignee).
func findAnyCleanupWisp(bd *BdCli, workDir, rigName, polecatName string) string {
	// Cleanup wisps are ephemeral (gt-4mnd): "bd list --label" only searches
	// the issues table and never sees them, regardless of flags. Use "bd
	// query" instead, same fix as findMRBeadForBranch (GH#2446).
	output, err := bd.Exec(workDir, "query",
		fmt.Sprintf("ephemeral=true AND label=cleanup AND label=polecat:%s AND status=open AND assignee=%s", polecatName, CleanupWispAssignee(rigName)),
		"--json",
	)
	if err != nil {
		return ""
	}
	if output == "" || output == "[]" || output == "null" {
		return ""
	}
	var items []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &items); err != nil || len(items) == 0 {
		return ""
	}
	return items[0].ID
}

// findAllCleanupWisps returns all open cleanup wisp IDs for a polecat.
// Used for dedup after wisp creation to detect races between concurrent patrol
// cycles (gt-7vs1). If the query fails, returns nil (caller treats as no race).
// Scoped to rigName via assignee so a same-named polecat in another rig can't
// match (gt-gsrz: see CleanupWispAssignee).
func findAllCleanupWisps(bd *BdCli, workDir, rigName, polecatName string) []string {
	// Cleanup wisps are ephemeral (gt-4mnd): "bd list --label" only searches
	// the issues table and never sees them, regardless of flags. Use "bd
	// query" instead, same fix as findMRBeadForBranch (GH#2446).
	output, err := bd.Exec(workDir, "query",
		fmt.Sprintf("ephemeral=true AND label=cleanup AND label=polecat:%s AND status=open AND assignee=%s", polecatName, CleanupWispAssignee(rigName)),
		"--json",
	)
	if err != nil {
		return nil
	}
	if output == "" || output == "[]" || output == "null" {
		return nil
	}
	var items []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &items); err != nil || len(items) == 0 {
		return nil
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

// findCleanupWispsForCompletion returns open cleanup wisp IDs for a polecat
// that match a specific (issueID, branch) completion. Unlike
// findAllCleanupWisps, which keys on polecat name alone, this scopes the
// match to the exact completion being processed — a polecat can legitimately
// hold open cleanup wisps for OTHER issues at the same time (dispatch reuse),
// so matching on name alone would misclassify those as duplicates. Used to
// make completion discovery idempotent (gt-mf5q). Also scoped to rigName via
// assignee so a same-named polecat in another rig can't match (gt-gsrz).
func findCleanupWispsForCompletion(bd *BdCli, workDir, rigName, polecatName, issueID, branch string) []string {
	// Cleanup wisps are ephemeral (gt-4mnd): "bd list --label" only searches
	// the issues table and never sees them, regardless of flags. Use "bd
	// query" instead, same fix as findMRBeadForBranch (GH#2446).
	output, err := bd.Exec(workDir, "query",
		fmt.Sprintf("ephemeral=true AND label=cleanup AND label=polecat:%s AND status=open AND assignee=%s", polecatName, CleanupWispAssignee(rigName)),
		"--json",
	)
	if err != nil || output == "" || output == "[]" || output == "null" {
		return nil
	}
	var items []struct {
		ID          string `json:"id"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		return nil
	}
	var matches []string
	for _, item := range items {
		descIssue, descBranch := parseCleanupWispDescription(item.Description)
		if descIssue == issueID && descBranch == branch {
			matches = append(matches, item.ID)
		}
	}
	return matches
}

// parseCleanupWispDescription extracts the "Issue: " and "Branch: " lines
// createCleanupWisp writes into a cleanup wisp's description, so completion
// discovery can match an existing wisp to a specific (issue, branch) pair.
func parseCleanupWispDescription(description string) (issueID, branch string) {
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Issue: "); ok {
			issueID = v
		} else if v, ok := strings.CutPrefix(line, "Branch: "); ok {
			branch = v
		}
	}
	return issueID, branch
}

// ensureCleanupWisp returns the cleanup wisp ID tracking a completion,
// creating one if none exists yet. Idempotent on (issue, branch): see
// findCleanupWispsForCompletion. isNew reports whether this call created the
// wisp (as opposed to finding one already tracking this completion).
// closedDups lists any duplicate wisps closed along the way, for operator
// visibility in the caller's Action string.
//
// Also guards the concurrency window between check and create: if two patrol
// scans both pass the precheck for the same completion, both create a wisp,
// so a post-create recheck deterministically picks one winner (lowest wisp
// ID) and closes the rest — same create-then-dedup pattern as gt-7vs1's
// zombie restart path.
func ensureCleanupWisp(bd *BdCli, workDir, rigName string, payload *PolecatDonePayload) (wispID string, isNew bool, closedDups []string, err error) {
	if existing := findCleanupWispsForCompletion(bd, workDir, rigName, payload.PolecatName, payload.IssueID, payload.Branch); len(existing) > 0 {
		sort.Strings(existing)
		return existing[0], false, nil, nil
	}

	newID, err := createCleanupWisp(bd, workDir, rigName, payload.PolecatName, payload.IssueID, payload.Branch)
	if err != nil {
		return "", false, nil, err
	}

	if dups := findCleanupWispsForCompletion(bd, workDir, rigName, payload.PolecatName, payload.IssueID, payload.Branch); len(dups) > 1 {
		sort.Strings(dups)
		winner := dups[0]
		for _, w := range dups[1:] {
			_, _ = bd.Exec(workDir, "close", w, "--reason=duplicate: concurrent patrol race (gt-mf5q)")
			closedDups = append(closedDups, w)
		}
		return winner, winner == newID, closedDups, nil
	}

	return newID, true, nil, nil
}

// hasPendingMR checks if a polecat has work waiting in the refinery merge queue.
// Returns true if either:
//  1. A cleanup wisp exists for this polecat (HandlePolecatDone created it for a pending MR)
//  2. The agent bead has an active_mr field set
//
// Used to prevent zombie detection from nuking polecats whose MR is still being
// processed by the refinery. Nuking would delete the remote branch and orphan the MR.
// See: gt-6a9d
func hasPendingMR(bd *BdCli, workDir, rigName, polecatName, agentBeadID string) bool {
	// Check 1: Cleanup wisp with merge-requested state (created by HandlePolecatDone)
	wispID, wispErr := findCleanupWisp(bd, workDir, rigName, polecatName)
	if wispErr != nil || wispID != "" {
		return true
	}

	// Check 2: active_mr on agent bead (set by gt done when MR is created)
	activeMR, sourceHint := getAgentMRContext(workDir, agentBeadID)
	assessment := polecat.AssessActiveMR(beadCLIShower{bd: bd, workDir: workDir}, polecat.ActiveMRInput{ActiveMR: activeMR, SourceIssueHint: sourceHint, RequireGitSafe: true, GitSafe: activeMRGitSafe(workDir, rigName, polecatName)})
	return assessment.Pending
}

// hasPendingMRFromSnapshot checks for a pending MR using a pre-fetched ActiveMR
// value from the agent bead snapshot, avoiding a redundant bd show call. (gt-2gra)
func hasPendingMRFromSnapshot(bd *BdCli, workDir, rigName, polecatName string, snap *agentBeadSnapshot) bool {
	// Check 1: Cleanup wisp with merge-requested state (created by HandlePolecatDone)
	wispID, wispErr := findCleanupWisp(bd, workDir, rigName, polecatName)
	if wispErr != nil || wispID != "" {
		return true
	}

	// Check 2: active_mr from pre-fetched snapshot
	activeMR := ""
	sourceHint := ""
	if snap != nil {
		activeMR = snap.ActiveMR
		sourceHint = snap.HookBead
		if snap.Fields != nil {
			if activeMR == "" {
				activeMR = snap.Fields.ActiveMR
			}
			sourceHint = snap.Fields.LastSourceIssue
			if sourceHint == "" {
				sourceHint = snap.Fields.HookBead
			}
			if sourceHint == "" {
				sourceHint = snap.HookBead
			}
		}
	}
	assessment := polecat.AssessActiveMR(beadCLIShower{bd: bd, workDir: workDir}, polecat.ActiveMRInput{ActiveMR: activeMR, SourceIssueHint: sourceHint, RequireGitSafe: true, GitSafe: activeMRGitSafe(workDir, rigName, polecatName)})
	return assessment.Pending
}

func activeMRBlockerFromCLI(bd *BdCli, workDir, activeMR string) string {
	if activeMR == "" {
		return ""
	}
	if bd == nil {
		return fmt.Sprintf("active_mr=%s status=unverified", activeMR)
	}
	output, err := bd.Exec(workDir, "show", activeMR, "--json")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return ""
		}
		return fmt.Sprintf("active_mr=%s status=lookup_error: %v", activeMR, err)
	}
	status := issueStatusFromShowJSON(output)
	if status == "" || beads.IssueStatus(status).IsTerminal() {
		return ""
	}
	return fmt.Sprintf("active_mr=%s status=%s", activeMR, status)
}

func issueStatusFromShowJSON(output string) string {
	output = strings.TrimSpace(output)
	if output == "" || output == "null" || output == "[]" {
		return ""
	}
	var items []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(output), &items); err == nil && len(items) > 0 {
		return items[0].Status
	}
	var item struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(output), &item); err == nil {
		return item.Status
	}
	return ""
}

func activeMRGitSafe(workDir, rigName, polecatName string) bool {
	townRoot := workDirToTownRoot(workDir)
	if townRoot == "" || rigName == "" || polecatName == "" {
		return false
	}
	clonePath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	g := git.NewGit(clonePath)
	branch, err := g.CurrentBranch()
	if err != nil || branch == "" {
		return false
	}
	status, err := g.CheckUncommittedWork()
	if err != nil {
		return false
	}
	if !status.CleanExcludingRuntime() || status.StashCount > 0 || status.UnpushedCommits > 0 {
		return false
	}
	pushed, unpushed, err := g.BranchPushedToRemote(branch, "origin")
	if err != nil {
		return false
	}
	return pushed && unpushed == 0
}

func terminalSafeDoneSnapshot(bd *BdCli, workDir, rigName, polecatName string, snap *agentBeadSnapshot) bool {
	if snap == nil || snap.Fields == nil || !activeMRGitSafe(workDir, rigName, polecatName) {
		return false
	}
	if snap.HookBead != "" || snap.Fields.HookBead != "" {
		return false
	}
	sourceIssue := snap.Fields.LastSourceIssue
	if sourceIssue == "" {
		return false
	}
	issue, err := (beadCLIShower{bd: bd, workDir: workDir}).Show(sourceIssue)
	if err != nil || issue == nil {
		return false
	}
	return beads.IssueStatus(issue.Status).IsTerminal()
}

// getAgentMRContext retrieves active_mr and durable source context from an agent bead.
func getAgentMRContext(workDir, agentBeadID string) (string, string) {
	issue, fields, err := beads.New(workDir).ForAgentBead().GetAgentBead(agentBeadID)
	if err != nil || issue == nil || fields == nil {
		return "", ""
	}
	sourceHint := fields.LastSourceIssue
	if sourceHint == "" {
		sourceHint = fields.HookBead
	}
	if sourceHint == "" {
		sourceHint = issue.HookBead
	}
	return fields.ActiveMR, sourceHint
}

type beadCLIShower struct {
	bd      *BdCli
	workDir string
}

func (s beadCLIShower) Show(issueID string) (*beads.Issue, error) {
	if s.bd == nil || s.bd.Exec == nil {
		return nil, fmt.Errorf("bd unavailable")
	}
	output, err := s.bd.Exec(s.workDir, "show", issueID, "--json")
	if err != nil {
		if isBdNotFoundError(err) {
			return nil, beads.ErrNotFound
		}
		return nil, err
	}
	output = strings.TrimSpace(output)
	if output == "" || output == "[]" || output == "null" {
		return nil, beads.ErrNotFound
	}
	var issues []beads.Issue
	if err := json.Unmarshal([]byte(output), &issues); err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		return nil, beads.ErrNotFound
	}
	return &issues[0], nil
}

func isBdNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such")
}
