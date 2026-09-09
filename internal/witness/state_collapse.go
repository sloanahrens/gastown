package witness

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
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
