package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/util"
)

// The reap pass's host half: patrolscan decides which seats may go, this file
// answers its questions over the gt CLI and the beads. The safety verdict is
// check-recovery's, never rebuilt here (design Revision 1, gt-kqzns).

const (
	// patrolScanRecoveryBatchTimeout bounds one `gt polecat check-recovery-batch`.
	// It reads every polecat in the rig and probes each worktree's git state.
	patrolScanRecoveryBatchTimeout = 120 * time.Second

	// patrolScanReapTimeout bounds one `gt polecat nuke`: the 3 minutes a
	// restart gets, since a nuke preserves the branch to origin first.
	patrolScanReapTimeout = patrolScanRestartTimeout

	// nukeRefusedExitCode is `gt polecat nuke`'s safety-refusal status. It
	// repeats internal/cmd's NukeRefusedExitCode because internal/cmd imports
	// this package, so the daemon cannot read the constant (gt-vuiii).
	nukeRefusedExitCode = 3
)

// errReapRefused marks a nuke the CLI refused on a safety check. The tick
// reports it as a blocked seat, not a failure; patrolScanOptions adds it to
// IsRefusal so the pure pass makes that call.
var errReapRefused = errors.New("nuke refused on a safety check")

// reapBatch is one rig's parsed check-recovery-batch result for a tick:
// verdicts by polecat, or the error the run left behind.
type reapBatch struct {
	byName map[string]patrolscan.Recovery
	err    error
}

// reapClaim is one rig's non-terminal beads for a tick, or the error the
// listing left behind.
type reapClaim struct {
	issues []*beads.Issue
	err    error
}

var _ patrolscan.ReapEnv = (*patrolScanHost)(nil)

// reapCleanup is the worktree_cleanup block this host reaps under, nil when
// the tick carries none.
func (h *patrolScanHost) reapCleanup() *agentconfig.WorktreeCleanupConfig {
	return reapCleanupConfig(h.d.patrolConfig)
}

// reapNotCovered is the error every ReapEnv method returns for a rig the
// config does not cover: a mis-scoped call reads as unknown, never as a
// removal.
func (h *patrolScanHost) reapNotCovered(rig string) error {
	return fmt.Errorf("worktree cleanup does not cover rig %s", rig)
}

func (h *patrolScanHost) reapCoversRig(rig string) bool {
	c := h.reapCleanup()
	return c.IsEnabled() && c.CoversRig(rig)
}

// Recovery answers with the seat's verdict from this tick's
// check-recovery-batch run.
func (h *patrolScanHost) Recovery(rig, polecat string) (patrolscan.Recovery, error) {
	if !h.reapCoversRig(rig) {
		return patrolscan.Recovery{}, h.reapNotCovered(rig)
	}
	batch, err := h.recoveryBatch(rig)
	if err != nil {
		return patrolscan.Recovery{}, err
	}
	// A polecat the batch did not mention is unknown, never a default: a
	// rotation the sweep missed must not read as a safe seat.
	rv, ok := batch.byName[polecat]
	if !ok {
		return patrolscan.Recovery{}, fmt.Errorf("check-recovery-batch %s: no verdict for polecat %s", rig, polecat)
	}
	return rv, nil
}

// recoveryBatch runs the rig's bulk check-recovery once per tick and caches
// it: the reap pass asks for a verdict on every seat that has no session and
// no live work, so one sweep must answer them all rather than one bd-heavy
// exec per seat (gt-b839).
func (h *patrolScanHost) recoveryBatch(rig string) (reapBatch, error) {
	if b, ok := h.reapBatches[rig]; ok {
		return b, b.err
	}
	b := h.runRecoveryBatch(rig)
	if h.reapBatches == nil {
		h.reapBatches = map[string]reapBatch{}
	}
	h.reapBatches[rig] = b
	return b, b.err
}

func (h *patrolScanHost) runRecoveryBatch(rig string) reapBatch {
	if h.d.gtPath == "" {
		return reapBatch{err: fmt.Errorf("%w: no gt binary resolved", errNoDaemonStarter)}
	}
	ctx, cancel := context.WithTimeout(h.d.ctxOrBackground(), patrolScanRecoveryBatchTimeout)
	defer cancel()
	// --reconcile-cleanup is the only write check-recovery has; the tick
	// writes nothing but the removal, so it is never passed.
	cmd := exec.CommandContext(ctx, h.d.gtPath, "polecat", "check-recovery-batch", rig, "--json") //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = h.town()
	cmd.Env = daemonGTEnv(os.Environ())
	util.SetProcessGroup(cmd)
	stdout, stderr, err := h.d.runCmd(cmd)
	if err != nil {
		return reapBatch{err: fmt.Errorf("gt polecat check-recovery-batch %s: %w: %s", rig, err, lastLine(string(stderr)))}
	}
	batch, err := parseRecoveryBatch(rig, stdout)
	if err != nil {
		return reapBatch{err: err}
	}
	return batch
}

// recoveryStatusJSON is the slice of `gt polecat check-recovery-batch --json`
// the reap pass reads. The CLI's own type lives in internal/cmd, which the
// daemon cannot import.
type recoveryStatusJSON struct {
	Polecat        string   `json:"polecat"`
	Verdict        string   `json:"verdict"`
	Reason         string   `json:"reason"`
	Reusable       bool     `json:"reusable"`
	SafeToNuke     bool     `json:"safe_to_nuke"`
	Branch         string   `json:"branch"`
	Issue          string   `json:"issue"`
	ActiveMR       string   `json:"active_mr"`
	Blockers       []string `json:"blockers"`
	GitStateSource string   `json:"git_state_source"`
}

// parseRecoveryBatch decodes one rig's sweep. Bad JSON, empty output and a
// status with no polecat are errors: a verdict that did not parse must not
// become a zero Recovery, which the pure pass would read as a real answer.
func parseRecoveryBatch(rig string, out []byte) (reapBatch, error) {
	if len(bytes.TrimSpace(out)) == 0 {
		return reapBatch{}, fmt.Errorf("check-recovery-batch %s: empty output", rig)
	}
	var statuses []recoveryStatusJSON
	if err := json.Unmarshal(out, &statuses); err != nil {
		return reapBatch{}, fmt.Errorf("check-recovery-batch %s: decoding JSON: %w", rig, err)
	}
	byName := make(map[string]patrolscan.Recovery, len(statuses))
	for _, s := range statuses {
		if s.Polecat == "" {
			return reapBatch{}, fmt.Errorf("check-recovery-batch %s: a status names no polecat", rig)
		}
		byName[s.Polecat] = patrolscan.Recovery{
			Verdict:        s.Verdict,
			Reusable:       s.Reusable,
			SafeToNuke:     s.SafeToNuke,
			Reason:         s.Reason,
			Branch:         s.Branch,
			Issue:          s.Issue,
			ActiveMR:       s.ActiveMR,
			Blockers:       s.Blockers,
			GitStateSource: s.GitStateSource,
		}
	}
	return reapBatch{byName: byName}, nil
}

// Reap removes one seat through `gt polecat nuke <rig>/<name>`, never with
// --force: the check-recovery verdict authorized it, and a nuke that refuses
// anyway is a blocked seat.
func (h *patrolScanHost) Reap(rig, polecat string) error {
	if !h.reapCoversRig(rig) {
		return h.reapNotCovered(rig)
	}
	if h.d.gtPath == "" {
		return fmt.Errorf("%w: no gt binary resolved", errNoDaemonStarter)
	}
	ctx, cancel := context.WithTimeout(h.d.ctxOrBackground(), patrolScanReapTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.d.gtPath, "polecat", "nuke", rig+"/"+polecat) //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = h.town()
	cmd.Env = daemonGTEnv(os.Environ())
	util.SetProcessGroup(cmd)
	_, stderr, err := h.d.runCmd(cmd)
	if err == nil {
		return nil
	}
	if exitCodeOf(err) == nukeRefusedExitCode {
		return fmt.Errorf("%w: %s", errReapRefused, lastLine(string(stderr)))
	}
	return fmt.Errorf("gt polecat nuke %s/%s: %w: %s", rig, polecat, err, lastLine(string(stderr)))
}

// exitCodeOf returns the process exit status an error carries, or -1.
func exitCodeOf(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

// IdleSince is the later of the seat's agent bead update and its hooked work
// bead's close (its last update when never closed); a parked seat also counts
// its park time. An unreadable input is an error; a seat with no readable
// input is the zero time, which the pure pass reads as not eligible.
func (h *patrolScanHost) IdleSince(rig, polecat string) (time.Time, error) {
	if !h.reapCoversRig(rig) {
		return time.Time{}, h.reapNotCovered(rig)
	}
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(h.town(), rig), rig, polecat)
	agent, err := h.readBeads(rig).Show(id)
	if err != nil {
		return time.Time{}, fmt.Errorf("bd show %s: %w", id, err)
	}
	if agent == nil {
		return time.Time{}, fmt.Errorf("bd show %s: no issue", id)
	}
	var since time.Time
	updated, err := parseBeadTime("updated_at", agent.UpdatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("agent bead %s: %w", id, err)
	}
	since = laterTime(since, updated)

	if hook := hookBeadRef(agent); hook != "" {
		work, err := h.readBeads(rig).Show(hook)
		if err != nil {
			return time.Time{}, fmt.Errorf("bd show %s: %w", hook, err)
		}
		if work != nil {
			// A closed bead's close is the seat's last real activity; an open
			// one's update is the best available proxy.
			when, err := parseBeadTime("closed_at", work.ClosedAt)
			if err != nil {
				return time.Time{}, fmt.Errorf("work bead %s: %w", hook, err)
			}
			if when.IsZero() {
				when, err = parseBeadTime("updated_at", work.UpdatedAt)
				if err != nil {
					return time.Time{}, fmt.Errorf("work bead %s: %w", hook, err)
				}
			}
			since = laterTime(since, when)
		}
	}

	rec, err := h.Intent(rig, polecat)
	if err != nil {
		return time.Time{}, fmt.Errorf("intent record %s/%s: %w", rig, polecat, err)
	}
	if rec.Desired == intent.DesiredPark {
		since = laterTime(since, rec.PausedAt)
	}
	return since, nil
}

// hookBeadRef is the seat's hooked work bead, the source of the grace clock's
// close time. The direct-tracking model leaves the agent bead's hook_bead
// empty for some seats, so its last source issue is the fallback (gt-14a).
func hookBeadRef(agent *beads.Issue) string {
	if agent.HookBead != "" {
		return agent.HookBead
	}
	f := beads.ParseAgentFields(agent.Description)
	if f == nil {
		return ""
	}
	if f.HookBead != "" {
		return f.HookBead
	}
	return f.LastSourceIssue
}

// parseBeadTime parses an RFC 3339 bead timestamp; empty is the zero time.
func parseBeadTime(field, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q is not RFC 3339: %w", field, value, err)
	}
	return t, nil
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// BranchClaimed is the first non-terminal bead in the rig that names branch:
// its resume_branch note, or the assignee of the branch's polecat. A failed
// list is an error, since the claim may be in the status that failed.
func (h *patrolScanHost) BranchClaimed(rig, branch string) (string, error) {
	if !h.reapCoversRig(rig) {
		return "", h.reapNotCovered(rig)
	}
	if branch == "" {
		return "", nil
	}
	claims, err := h.nonTerminalBeads(rig)
	if err != nil {
		return "", fmt.Errorf("listing %s beads for branch %s: %w", rig, branch, err)
	}
	owner := branchPolecat(branch)
	for _, is := range claims.issues {
		if patrolscan.ResumeBranchFromNotes(is.Notes) == branch {
			return is.ID, nil
		}
		if owner != "" && is.Assignee == rig+"/polecats/"+owner {
			return is.ID, nil
		}
	}
	return "", nil
}

// nonTerminalBeads lists the rig's non-terminal beads once per tick, so the
// per-seat branch-claim question costs one listing rather than five queries
// per seat.
func (h *patrolScanHost) nonTerminalBeads(rig string) (reapClaim, error) {
	if c, ok := h.reapClaims[rig]; ok {
		return c, c.err
	}
	issues, err := h.listByStatus(rig, "", "open", "in_progress", "hooked", "blocked", "deferred")
	c := reapClaim{issues: issues, err: err}
	if h.reapClaims == nil {
		h.reapClaims = map[string]reapClaim{}
	}
	h.reapClaims[rig] = c
	return c, c.err
}

// branchPolecat is the polecat a `polecat/<name>/<bead>@<hash>` branch
// belongs to, "" for a name this codebase did not mint.
func branchPolecat(branch string) string {
	parts := strings.Split(branch, "/")
	if len(parts) < 3 || parts[0] != "polecat" {
		return ""
	}
	return parts[1]
}
