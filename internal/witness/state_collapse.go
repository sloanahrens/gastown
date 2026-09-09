package witness

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// StateCollapseFinding is a single detected "recorded as done, not in force"
// contradiction: a source issue is closed while the merge-request bead that
// exists to land its fix is still open. This is the failure signature
// catalogued in gt-zzd (instances 4 and 6): a bead can be closed
// independently of its MR merging, and nothing previously re-checked that
// after the fact — every prior instance was caught by an agent reading
// source by hand, never by a mechanism.
type StateCollapseFinding struct {
	IssueID  string // The closed source issue
	MRID     string // The still-open MR bead referencing it as source_issue
	MRStatus string // The MR's current status (e.g. "open", "in_progress")
	Branch   string // The MR's source branch, if recorded
	Target   string // The MR's target branch, if recorded
}

// DetectStateCollapseResult holds the aggregate result of a state-collapse scan.
type DetectStateCollapseResult struct {
	Checked  int // number of open MR beads examined
	Findings []StateCollapseFinding
	Errors   []error
}

// DetectStateCollapse finds issues recorded as done (bead closed) while the
// fix they describe is not verified to be in force: the merge-request bead
// created to land it is still open.
//
// This is scoped to the current open MR queue (bounded by queue depth), not
// a scan of closed-bead history: every open MR already names its source
// issue via the "source_issue:" field (beads.ParseMRFields), so checking
// whether that issue already closed is the cheap direction. Closing a source
// issue only happens alongside closing its MR during a normal merge (see
// refinery post-merge handling) — a closed issue with a still-open MR is
// therefore never expected, and is exactly the "trusting a close" failure
// mode gt-zzd's own catalogue flags as never caught mechanically.
//
// Each finding is recorded as a bead comment on the source issue (durable,
// discoverable via `bd show`) and escalated to the rig's mayor by mail, with
// a tmux nudge fallback if the mail send fails — mirroring the pattern used
// by resetAbandonedBead for SPAWN_BLOCKED/RECOVERED_BEAD notifications.
//
// This check only proves the contradiction at the bead-state level (closed
// issue, open MR). It intentionally does not also re-verify the MR's branch
// against a re-fetched origin/<target> via git — that is a stronger, more
// expensive corroboration (see gt-zzd's "verify at source" method note) left
// for a follow-up rather than folded into this scan.
func DetectStateCollapse(bd *BdCli, workDir, rigName string, router *mail.Router) *DetectStateCollapseResult {
	result := &DetectStateCollapseResult{}
	if bd == nil {
		return result
	}

	output, err := bd.Exec(workDir, "list", "--label=gt:merge-request", "--status=open", "--json", "--limit=0")
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("listing open MRs: %w", err))
		return result
	}
	if output == "" {
		return result
	}

	var mrs []*beads.Issue
	if err := json.Unmarshal([]byte(output), &mrs); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("parsing open MRs: %w", err))
		return result
	}

	for _, mr := range mrs {
		fields := beads.ParseMRFields(mr)
		if fields == nil || fields.SourceIssue == "" {
			continue
		}
		result.Checked++

		status, ok := getBeadStatus(bd, workDir, fields.SourceIssue)
		if !ok || status == "" {
			continue // source issue not found/unreachable — can't assert collapse
		}
		if status != string(beads.StatusClosed) {
			continue // still open, or a non-close terminal state (e.g. tombstone) — no collapse
		}

		finding := StateCollapseFinding{
			IssueID:  fields.SourceIssue,
			MRID:     mr.ID,
			MRStatus: mr.Status,
			Branch:   fields.Branch,
			Target:   fields.Target,
		}
		result.Findings = append(result.Findings, finding)
		reportStateCollapse(bd, workDir, rigName, finding, router)
	}

	return result
}

// reportStateCollapse persists a state-collapse finding to the source issue
// (bead comment) and escalates it to the rig's mayor by mail, falling back
// to a tmux nudge if mail delivery fails.
func reportStateCollapse(bd *BdCli, workDir, rigName string, f StateCollapseFinding, router *mail.Router) {
	comment := fmt.Sprintf(
		"STATE-COLLAPSE: this issue is closed but its merge request %s is still %s "+
			"(branch=%q target=%q). The fix is recorded as done but may not be in force — "+
			"verify at source (git fetch + merge-base --is-ancestor against origin/%s) before trusting this close.",
		f.MRID, f.MRStatus, f.Branch, f.Target, f.Target,
	)
	if err := bd.Run(workDir, "comments", "add", f.IssueID, comment); err != nil {
		fmt.Fprintf(os.Stderr, "witness: failed to comment state-collapse on %s: %v\n", f.IssueID, err)
	}

	if router == nil {
		return
	}

	subject := fmt.Sprintf("STATE_COLLAPSE %s closed, %s still %s", f.IssueID, f.MRID, f.MRStatus)
	body := fmt.Sprintf(`Issue %s is CLOSED but its merge-request %s is still %s — the fix may not be in force.

Branch: %s
Target: %s

This is the gt-zzd state-collapse pattern (instances 4 and 6): a fix recorded
as done while its actual landing is unverified. Confirm at source before
trusting the close:
  git fetch origin && git merge-base --is-ancestor <branch-head-sha> origin/%s

If the branch is not an ancestor, the issue was closed with the fix NOT in
force — reopen it and re-route the MR.`, f.IssueID, f.MRID, f.MRStatus, f.Branch, f.Target, f.Target)

	msg := &mail.Message{
		From:     fmt.Sprintf("%s/witness", rigName),
		To:       "mayor/",
		Subject:  subject,
		Priority: mail.PriorityUrgent,
		Body:     body,
	}
	if err := router.Send(msg); err != nil {
		fmt.Fprintf(os.Stderr, "witness: failed to mail state-collapse finding for %s: %v, attempting nudge fallback\n", f.IssueID, err)
		t := tmux.NewTmux()
		nudgeMsg := fmt.Sprintf("%s — mail send failed, see bead comments on %s", subject, f.IssueID)
		if nudgeErr := t.NudgeSession(session.MayorSessionName(), nudgeMsg); nudgeErr != nil {
			fmt.Fprintf(os.Stderr, "witness: nudge fallback to mayor also failed for %s: %v\n", f.IssueID, nudgeErr)
		}
	}
}

// BranchStrandFinding is the state-collapse signature DetectStateCollapse
// cannot see (gt-3ii): a polecat branch exists on the rig's remote for an
// issue that is now CLOSED, but the fix never reached the target branch and
// no MR bead was ever created to land it. This is strictly worse than the
// gt-zzd case DetectStateCollapse catches: there, an open MR sits in the
// queue as a reminder; here nothing in the system will ever merge the
// branch and there is no queue entry pointing at it.
type BranchStrandFinding struct {
	IssueID     string // The closed source issue
	Branch      string // The polecat branch name (e.g. "polecat/pyrite/gt-hsg+mttuo7sf")
	CloseReason string // The bead's recorded close reason, if any
}

// DetectStrandedBranchesResult holds the aggregate result of a stranded-branch scan.
type DetectStrandedBranchesResult struct {
	Checked  int // number of closed-issue polecat branches examined
	Findings []BranchStrandFinding
	Errors   []error
}

// BranchRefSource abstracts the git remote queries DetectStrandedBranches
// needs, so the scan is unit-testable without a live git remote. Production
// code uses DefaultBranchRefSource; tests provide a fake.
type BranchRefSource struct {
	// ListPolecatBranches returns polecat branch names present on the rig's
	// remote (e.g. "polecat/pyrite/gt-hsg+mttuo7sf", without the
	// "refs/heads/" prefix).
	ListPolecatBranches func() ([]string, error)
	// TargetHasCommitReferencing reports whether any commit reachable from
	// the target branch's remote-tracking ref mentions issueID in its
	// message.
	TargetHasCommitReferencing func(target, issueID string) (bool, error)
}

// DefaultBranchRefSource returns a BranchRefSource backed by a real git
// remote, rooted at repoPath (the rig's canonical clone, e.g.
// "<rig>/mayor/rig").
func DefaultBranchRefSource(repoPath string) *BranchRefSource {
	g := git.NewGit(repoPath)
	return &BranchRefSource{
		ListPolecatBranches: func() ([]string, error) {
			refs, err := g.ListRemoteRefsWithHashes("origin", "refs/heads/polecat/")
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(refs))
			for _, r := range refs {
				names = append(names, strings.TrimPrefix(r.Name, "refs/heads/"))
			}
			return names, nil
		},
		TargetHasCommitReferencing: func(target, issueID string) (bool, error) {
			return g.LogGrep("origin/"+target, issueID)
		},
	}
}

// DetectStrandedBranches finds issues closed while the branch that was
// supposed to carry their fix never reached the target branch and no
// merge-request bead was ever created for it — the state DetectStateCollapse
// cannot see, because it only iterates the open MR queue and this state has
// no MR at all (gt-3ii). It reasons from git branch state rather than queue
// membership: for every polecat branch on the remote whose issue bead is
// CLOSED, it checks whether the fix actually reached the target.
//
// Two false-positive classes were measured against the live gastown branch
// set before shipping this (see gt-3ii comment history: the first proposed
// shape flagged 12/12 unmerged branches, 12/12 of them false) and are
// filtered explicitly rather than inferred from ancestry:
//
//   - Squash merges: this rig squash-merges, so a branch tip is NEVER an
//     ancestor of the target branch even when the work landed cleanly.
//     TargetHasCommitReferencing (git log --grep, matching the issue id the
//     rig convention embeds in every squash commit subject) is the
//     primitive that still answers "did this land" — IsAncestor cannot.
//   - Deliberate discards: a polecat that independently finds its fix
//     already landed under a different issue legitimately closes with
//     "no-changes: ..." (the documented polecat convention) and abandons
//     its branch rather than pushing a duplicate MR. That leaves the same
//     on-disk signature as a genuine strand, so the close reason must be
//     read, not just the status.
func DetectStrandedBranches(bd *BdCli, refs *BranchRefSource, workDir, rigName, targetBranch string, router *mail.Router) *DetectStrandedBranchesResult {
	result := &DetectStrandedBranchesResult{}
	if bd == nil || refs == nil || refs.ListPolecatBranches == nil || refs.TargetHasCommitReferencing == nil {
		return result
	}
	if targetBranch == "" {
		targetBranch = "main"
	}

	branches, err := refs.ListPolecatBranches()
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("listing polecat branches: %w", err))
		return result
	}

	for _, branch := range branches {
		meta, ok := polecat.ParseBranchName(branch)
		if !ok || meta.Issue == "" {
			continue // no issue encoded in the branch name — nothing to check
		}

		status, closeReason, found := getBeadStatusAndCloseReason(bd, workDir, meta.Issue)
		if !found || status != string(beads.StatusClosed) {
			continue // bead unreachable, or not closed — no collapse to report
		}
		result.Checked++

		if isDeliberateDiscard(closeReason) {
			continue // closed with an explicit "no-changes:" abandonment, not a strand
		}

		landed, err := refs.TargetHasCommitReferencing(targetBranch, meta.Issue)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("checking %s against origin/%s: %w", meta.Issue, targetBranch, err))
			continue
		}
		if landed {
			continue // reached the target (commonly via squash) — the branch tip just isn't an ancestor
		}

		finding := BranchStrandFinding{
			IssueID:     meta.Issue,
			Branch:      branch,
			CloseReason: closeReason,
		}
		result.Findings = append(result.Findings, finding)
		reportBranchStrand(bd, workDir, rigName, targetBranch, finding, router)
	}

	return result
}

// isDeliberateDiscard reports whether a bead close reason declares that no
// code changes are landing — the town-wide polecat convention
// (`bd close <id> --reason="no-changes: <explanation>"`), used when a
// polecat finds its fix already covered elsewhere and abandons its branch
// rather than pushing a duplicate/conflicting MR (gt-3ii, verified against
// gt-g6b).
func isDeliberateDiscard(closeReason string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(closeReason)), "no-changes:")
}

// getBeadStatusAndCloseReason returns a bead's status and close reason, and
// true if the lookup succeeded. Mirrors getBeadStatus but also surfaces
// close_reason, needed to distinguish a genuine strand from a deliberate
// discard (gt-3ii).
func getBeadStatusAndCloseReason(bd *BdCli, workDir, beadID string) (status, closeReason string, ok bool) {
	if beadID == "" {
		return "", "", false
	}
	output, err := bd.Exec(workDir, "show", beadID, "--json")
	if err != nil || output == "" {
		return "", "", false
	}
	var issues []struct {
		Status      string `json:"status"`
		CloseReason string `json:"close_reason"`
	}
	if err := json.Unmarshal([]byte(output), &issues); err != nil || len(issues) == 0 {
		// Valid response but no results — bead was reaped/deleted.
		return "", "", true
	}
	return issues[0].Status, issues[0].CloseReason, true
}

// reportBranchStrand persists a branch-strand finding to the source issue
// (bead comment) and escalates it to the rig's mayor by mail, falling back
// to a tmux nudge if mail delivery fails. Mirrors reportStateCollapse.
func reportBranchStrand(bd *BdCli, workDir, rigName, targetBranch string, f BranchStrandFinding, router *mail.Router) {
	comment := fmt.Sprintf(
		"STATE-COLLAPSE (stranded branch): this issue is closed but branch %q was never merged into origin/%s "+
			"and no merge-request bead was ever created for it. The fix is recorded as done but may not be in "+
			"force — verify at source (git fetch + git log origin/%s --grep=%s -F) before trusting this close.",
		f.Branch, targetBranch, targetBranch, f.IssueID,
	)
	if err := bd.Run(workDir, "comments", "add", f.IssueID, comment); err != nil {
		fmt.Fprintf(os.Stderr, "witness: failed to comment branch-strand on %s: %v\n", f.IssueID, err)
	}

	if router == nil {
		return
	}

	subject := fmt.Sprintf("STATE_COLLAPSE %s closed, branch %s never merged", f.IssueID, f.Branch)
	body := fmt.Sprintf(`Issue %s is CLOSED but its branch %s was never merged into origin/%s — no commit there
references it, and no merge-request bead exists for it either.

This is the gt-3ii branch-driven state-collapse pattern: strictly worse than
the MR-driven case (gt-zzd), because there is no queue entry to catch it — if
this is a genuine strand, nothing in the system will ever land the fix.
Confirm at source before trusting the close:
  git fetch origin && git log origin/%s --grep=%s -F

If nothing is found, the issue was closed with the fix NOT in force — reopen
it and re-dispatch.`, f.IssueID, f.Branch, targetBranch, targetBranch, f.IssueID)

	msg := &mail.Message{
		From:     fmt.Sprintf("%s/witness", rigName),
		To:       "mayor/",
		Subject:  subject,
		Priority: mail.PriorityUrgent,
		Body:     body,
	}
	if err := router.Send(msg); err != nil {
		fmt.Fprintf(os.Stderr, "witness: failed to mail branch-strand finding for %s: %v, attempting nudge fallback\n", f.IssueID, err)
		t := tmux.NewTmux()
		nudgeMsg := fmt.Sprintf("%s — mail send failed, see bead comments on %s", subject, f.IssueID)
		if nudgeErr := t.NudgeSession(session.MayorSessionName(), nudgeMsg); nudgeErr != nil {
			fmt.Fprintf(os.Stderr, "witness: nudge fallback to mayor also failed for %s: %v\n", f.IssueID, nudgeErr)
		}
	}
}
