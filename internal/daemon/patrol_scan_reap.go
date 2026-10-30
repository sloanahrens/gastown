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

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// The reap pass's host half: patrolscan decides which seats may go, this file
// answers its questions over the gt CLI and the beads. The safety verdict is
// check-recovery's, never rebuilt here (design Revision 1, gt-kqzns).

const (
	// patrolScanRecoveryBatchBase is the part of the bound on one `gt polecat
	// check-recovery-batch` that does not depend on the rig's size.
	patrolScanRecoveryBatchBase = 120 * time.Second

	// patrolScanRecoveryBatchPerPolecat is what each polecat in the rig adds to
	// that bound. The batch reads every polecat and probes each worktree's git
	// state, so its cost grows with the seat count.
	patrolScanRecoveryBatchPerPolecat = 15 * time.Second

	// patrolScanRecoveryBatchCap is the longest one batch may run, however many
	// seats the rig holds, so a wedged sweep cannot hold the tick indefinitely.
	patrolScanRecoveryBatchCap = 10 * time.Minute

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

// recoveryBatchBound is the time one check-recovery-batch run gets in a rig of
// polecats seats: the base, plus the allowance per polecat, never more than the
// cap.
func recoveryBatchBound(polecats int) time.Duration {
	polecats = max(polecats, 0)
	bound := patrolScanRecoveryBatchBase + time.Duration(polecats)*patrolScanRecoveryBatchPerPolecat
	return min(bound, patrolScanRecoveryBatchCap)
}

// rigPolecats counts the rig's polecat seats for the batch bound. A rig whose
// seats cannot be listed has no count to size a bound by, and says so in the
// error.
func (h *patrolScanHost) rigPolecats(rig string) (int, error) {
	names, err := h.Polecats(rig)
	if err != nil {
		return 0, err
	}
	return len(names), nil
}

func (h *patrolScanHost) runRecoveryBatch(rig string) reapBatch {
	if h.d.gtPath == "" {
		return reapBatch{err: fmt.Errorf("%w: no gt binary resolved", errNoDaemonStarter)}
	}
	polecats, err := h.rigPolecats(rig)
	if err != nil {
		// A bound sized for no seats would be the shortest one and kill a
		// sweep over a large rig; with the seat count unknown the sweep gets
		// the longest bound, and the log says why.
		if h.d.logger != nil {
			h.d.logger.Printf("patrol_scan: %s: reap: listing polecat seats for the batch bound failed, using the %s cap: %v",
				rig, patrolScanRecoveryBatchCap, err)
		}
		return h.runRecoveryBatchWithin(rig, unknownPolecats, patrolScanRecoveryBatchCap)
	}
	return h.runRecoveryBatchWithin(rig, polecats, recoveryBatchBound(polecats))
}

// unknownPolecats is the seat count of a rig whose seats could not be listed.
const unknownPolecats = -1

// runRecoveryBatchWithin runs the rig's sweep under bound, the deadline
// recoveryBatchBound gave a rig of polecats seats (unknownPolecats when the
// seats could not be listed). The deadline runs on the daemon's clock (clk()),
// so a test's fake clock decides when it fires rather than the host's load. A
// run the deadline kills names its elapsed time, bound and seat count in its
// error, which is the line the reap pass writes for every seat the batch could
// not answer.
func (h *patrolScanHost) runRecoveryBatchWithin(rig string, polecats int, bound time.Duration) reapBatch {
	ctx, cancel := clockwork.WithTimeout(h.d.ctxOrBackground(), h.d.clk(), bound)
	defer cancel()
	// --reconcile-cleanup is the only write check-recovery has; the tick
	// writes nothing but the removal, so it is never passed.
	cmd := exec.CommandContext(ctx, h.d.gtPath, "polecat", "check-recovery-batch", rig, "--json") //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = h.town()
	cmd.Env = daemonGTEnv(os.Environ())
	started := time.Now()
	stdout, stderr, err := h.d.runCmd(cmd)
	if err != nil {
		if killedByDeadline(ctx, err) {
			seats := fmt.Sprintf("%d polecats", polecats)
			if polecats == unknownPolecats {
				seats = "an unlisted number of polecats"
			}
			return reapBatch{err: fmt.Errorf("gt polecat check-recovery-batch %s: killed after %s at the %s bound for %s: %w: %s",
				rig, time.Since(started).Round(time.Second), bound, seats, err, lastLine(string(stderr)))}
		}
		return reapBatch{err: fmt.Errorf("gt polecat check-recovery-batch %s: %w: %s", rig, err, lastLine(string(stderr)))}
	}
	batch, err := parseRecoveryBatch(rig, stdout)
	if err != nil {
		return reapBatch{err: err}
	}
	return batch
}

// killedByDeadline reports whether err is the deadline of ctx ending the
// command: the deadline has passed and the command's own error says it was cut
// off, as a context error or as a death by signal (ExitCode -1). A command that
// failed with an exit status of its own as the deadline passed was not killed,
// and its error is the answer.
//
// Done is read before Err: a clock-driven context's Err blocks until its Done
// closes, so a deadline that has not passed has to be read off the channel
// (gt-fbf5n).
func killedByDeadline(ctx context.Context, err error) bool {
	select {
	case <-ctx.Done():
	default:
		return false
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var coded interface{ ExitCode() int }
	return errors.As(err, &coded) && coded.ExitCode() == -1
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
