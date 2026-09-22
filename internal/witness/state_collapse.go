package witness

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// StateCollapseFinding is a single detected "recorded as done, not in force"
// contradiction: a source issue is closed while the merge-request bead that
// exists to land its fix is still open. This is the failure signature
// cataloged in gt-zzd (instances 4 and 6): a bead can be closed
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
	Checked     int  // number of open MR beads examined
	MRLookupRan bool // whether the open merge-request queue was successfully resolved
	OpenMRsSeen int  // number of open MRs the resolved queue returned
	Findings    []StateCollapseFinding
	Errors      []error
}

// errNoOpenMRSource is reported when DetectStateCollapse is wired without an
// open-MR source. The scan is defined over the open MR queue, so without the
// source there is nothing to examine: it reports the error and returns
// MRLookupRan=false rather than an empty finding list that reads as an
// all-clear (gt-92ry, same honesty rule DetectStrandedBranches follows for
// its MR lookup after gt-akap).
var errNoOpenMRSource = errors.New("open merge-request lookup unavailable: cannot examine the MR queue for state collapse")

// DetectStateCollapse finds issues recorded as done (bead closed) while the
// fix they describe is not verified to be in force: the merge-request bead
// created to land it is still open.
//
// This is scoped to the current open MR queue (bounded by queue depth), not
// a scan of closed-bead history: every open MR already names its source
// issue via the "source_issue:" field (beads.ParseMRFields), so checking
// whether that issue already closed is the cheap direction. A closed issue
// with a still-open MR is the "trusting a close" failure mode gt-zzd's own
// catalog flags as never caught mechanically — against the live gastown rig
// the first un-blinded run found 8 of 10 queued MRs in that state.
//
// Most of those are NOT collapses: gt done self-closes the source issue as
// "pending_mr: <mr>" at submit time, so an issue closed with a claim of work
// in flight, naming an MR that really is in the queue, is the ordinary
// healthy path and is suppressed. What remains — an issue closed as done
// (any other reason, or none) while the MR meant to land its fix still sits
// open — is the contradiction this scan reports.
//
// Each finding is recorded as a bead comment on the source issue (durable,
// discoverable via `bd show`) and escalated to the rig's mayor by mail, with
// a tmux nudge fallback if the mail send fails — mirroring the pattern used
// by resetAbandonedBead for SPAWN_BLOCKED/RECOVERED_BEAD notifications.
//
// The queue is read through refs.ListOpenMRs, the same injected source
// DetectStrandedBranches uses, and not through `bd list --label=gt:merge-request`.
// Merge-request beads are created as wisps (gt mq submit, GH#2446), which
// `bd list` does not return — the plain-list version of this scan was
// structurally blind to the entire queue and reported "No state collapse
// found (0 open MRs)" for it, i.e. a failure that serialized as success
// (gt-92ry; the gt-akap fix on the sibling branch detector). Because the
// scan's whole subject matter arrives through that source, a missing or
// failing source sets MRLookupRan=false and yields no findings rather than
// an empty list that reads as an all-clear — callers must surface that
// state (see StateCollapseSummary).
//
// This check only proves the contradiction at the bead-state level (closed
// issue, open MR). It intentionally does not also re-verify the MR's branch
// against a re-fetched origin/<target> via git — that is a stronger, more
// expensive corroboration (see gt-zzd's "verify at source" method note) left
// for a follow-up rather than folded into this scan.
func DetectStateCollapse(bd *BdCli, refs *BranchRefSource, workDir, rigName string, router *mail.Router) *DetectStateCollapseResult {
	result := &DetectStateCollapseResult{}
	if bd == nil {
		return result
	}
	if refs == nil || refs.ListOpenMRs == nil {
		result.Errors = append(result.Errors, errNoOpenMRSource)
		return result
	}

	mrs, err := refs.ListOpenMRs()
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("open merge-request lookup unavailable: %w", err))
		return result
	}
	result.MRLookupRan = true
	result.OpenMRsSeen = len(mrs)
	openMRs := NewOpenMRSet(mrs)

	for _, mr := range mrs {
		if mr.SourceIssue == "" {
			continue
		}
		result.Checked++

		record, ok := getBeadRecord(bd, workDir, mr.SourceIssue)
		if !ok || record.Status == "" {
			continue // source issue not found/unreachable — can't assert collapse
		}
		if record.Status != string(beads.StatusClosed) {
			continue // still open, or a non-close terminal state (e.g. tombstone) — no collapse
		}

		// gt done closes the source issue as "pending_mr: <mr>" the moment it
		// submits (beads.PendingMergeCloseReason), so a closed issue naming an
		// MR that is still open in the queue is the ordinary in-flight
		// workflow, not a collapse — the same false-positive class gt-akap
		// fixed on the branch-driven side, and the dominant one here: the
		// first un-blinded run against the live gastown rig flagged 7 of 10
		// queued MRs this way, every one of them a healthy submit.
		//
		// The claim is honored only when the MR it names is actually open. A
		// close reason is a claim about the queue, not the queue itself, so a
		// pending_mr naming a purged or already-closed MR leaves the issue
		// closed with nothing in flight — still a collapse candidate, exactly
		// as for the branch-driven scan.
		if named := pendingMRFromCloseReason(record.CloseReason); named != "" && openMRs.byID[named] {
			continue
		}

		mrStatus := mr.Status
		if mrStatus == "" {
			// The source is defined as the rig's *open* queue, so a ref that
			// names no status is open by construction — say so rather than
			// printing an empty state into the finding.
			mrStatus = string(beads.StatusOpen)
		}

		finding := StateCollapseFinding{
			IssueID:  mr.SourceIssue,
			MRID:     mr.ID,
			MRStatus: mrStatus,
			Branch:   mr.Branch,
			Target:   mr.Target,
		}
		result.Findings = append(result.Findings, finding)
		reportStateCollapse(bd, workDir, rigName, finding, router)
	}

	return result
}

// stateCollapseRenotifyAfter bounds how long one state-collapse finding stays
// reported before a later patrol says it again.
//
// Both detectors run on every patrol, so a condition that persists would
// otherwise append an identical comment — and send an identical urgent mail to
// the mayor — once per cycle, leaving one durable record per firing with
// nothing to tell the eleventh from the first (gt-vwry). Reporting is
// therefore suppressed while a matching report is already on the bead, and
// resumes once that report is older than this window: a genuine collapse that
// nobody has acted on yet must not go quiet just because the first notice is
// old, and the mail copy is a wisp that a reaper can collect.
const stateCollapseRenotifyAfter = 12 * time.Hour

// alreadyReportedRecently reports whether the issue already carries a comment
// identifying this same condition, added within the renotify window.
//
// A read failure reports "not reported" — the cost of a duplicate comment is a
// redundant line, while the cost of a silent skip is a collapse nobody hears
// about, and only one of those is recoverable.
func alreadyReportedRecently(bd *BdCli, workDir, issueID, marker string, now time.Time) bool {
	out, err := bd.Exec(workDir, "comments", issueID, "--json")
	if err != nil {
		return false
	}
	var comments []beads.Comment
	if err := json.Unmarshal([]byte(out), &comments); err != nil {
		return false
	}
	for _, c := range comments {
		if !strings.Contains(c.Text, marker) {
			continue
		}
		createdAt, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil {
			// Unparseable timestamp: the record exists and cannot be aged
			// out, so treat it as current rather than re-reporting forever.
			return true
		}
		if now.Sub(createdAt) < stateCollapseRenotifyAfter {
			return true
		}
	}
	return false
}

// reportStateCollapse persists a state-collapse finding to the source issue
// (bead comment) and escalates it to the rig's mayor by mail, falling back
// to a tmux nudge if mail delivery fails.
func reportStateCollapse(bd *BdCli, workDir, rigName string, f StateCollapseFinding, router *mail.Router) {
	// The MR id is what makes this finding this finding: the status it is stuck
	// in, and the branch, can change between patrols without the condition
	// itself being new.
	marker := fmt.Sprintf("merge request %s is still", f.MRID)
	if alreadyReportedRecently(bd, workDir, f.IssueID, marker, time.Now()) {
		return
	}

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
//
// The "no MR" half of that claim is only made after DetectStrandedBranches
// has actually resolved the rig's open merge-request queue (gt-akap): a
// finding means no open MR covers this issue, not that the lookup was
// skipped.
type BranchStrandFinding struct {
	IssueID     string // The closed source issue
	Branch      string // The polecat branch name (e.g. "polecat/pyrite/gt-hsg+mttuo7sf")
	CloseReason string // The bead's recorded close reason, if any
}

// SupersededBranch is a candidate the scan declined to report because the
// issue's own record already adjudicated it: a rejected attempt, followed by
// a closure through the merge queue. Reported alongside the findings so a
// suppressed branch is visible rather than silently dropped.
type SupersededBranch struct {
	IssueID string `json:"issue"`        // The closed source issue
	Branch  string `json:"branch"`       // The branch recorded as rejected
	MRID    string `json:"mr,omitempty"` // MR or later issue named by the close reason as carrying the fix forward
}

// DetectStrandedBranchesResult holds the aggregate result of a stranded-branch scan.
type DetectStrandedBranchesResult struct {
	Checked     int  // number of closed-issue polecat branches examined
	MRLookupRan bool // whether the open merge-request queue was successfully resolved
	OpenMRsSeen int  // number of open MRs in the resolved queue
	Findings    []BranchStrandFinding
	Superseded  []SupersededBranch // candidates suppressed as adjudicated, not stranded
	Errors      []error
}

// errNoMRLookup is reported when DetectStrandedBranches is wired without an
// open-MR source. Without the lookup the scan cannot assert that a branch
// "has no MR", so it produces no findings at all rather than unqualified
// ones (gt-akap).
var errNoMRLookup = errors.New("open merge-request lookup unavailable: cannot assert a branch has no MR")

// BranchRefSource abstracts the git remote and merge-queue queries
// DetectStrandedBranches needs, so the scan is unit-testable without a live
// git remote or Dolt. Production code uses DefaultBranchRefSource plus a
// ListOpenMRs wired from the rig's beads database; tests provide a fake.
type BranchRefSource struct {
	// ListPolecatBranches returns polecat branch names present on the rig's
	// remote (e.g. "polecat/pyrite/gt-hsg+mttuo7sf", without the
	// "refs/heads/" prefix).
	ListPolecatBranches func() ([]string, error)
	// TargetHasCommitReferencing reports whether any commit reachable from
	// the target branch's remote-tracking ref mentions issueID in its
	// message.
	TargetHasCommitReferencing func(target, issueID string) (bool, error)
	// ListOpenMRs returns the merge-request beads currently open in the
	// rig's queue, used to suppress the false strand (gt-akap): a polecat
	// that closes its source issue with "pending_mr: <mr>" while the MR
	// waits in the queue leaves exactly the on-disk signature of a strand.
	// Required — see DetectStrandedBranches for why a nil/erroring lookup
	// produces no findings rather than unqualified ones.
	ListOpenMRs func() ([]OpenMRRef, error)
}

// OpenMRRef is one open merge-request bead as the MR-driven scans need to
// see it: the MR's id and status, and the identities other beads record for
// it (beads.ParseMRFields), any of which may be absent on old MR beads.
type OpenMRRef struct {
	ID          string
	Status      string // MR bead status (e.g. "open"); empty means open by construction
	SourceIssue string // source_issue field of the MR description, if any
	Branch      string // branch field of the MR description, if any
	Target      string // target field of the MR description, if any
}

// OpenMRSet is a rig's open merge-request queue indexed by every identity a
// stranded-branch candidate can be matched on. Its only job is to answer
// "is this closed issue's fix still in flight?" before the scan calls it
// stranded (gt-akap).
type OpenMRSet struct {
	byIssue  map[string]string
	byBranch map[string]string
	byID     map[string]bool
}

// NewOpenMRSet indexes open MRs for lookup. First MR wins per key, so the
// reported MR id is at least deterministic.
func NewOpenMRSet(mrs []OpenMRRef) *OpenMRSet {
	s := &OpenMRSet{
		byIssue:  make(map[string]string, len(mrs)),
		byBranch: make(map[string]string, len(mrs)),
		byID:     make(map[string]bool, len(mrs)),
	}
	for _, mr := range mrs {
		mr.ID = strings.TrimSpace(mr.ID)
		if mr.ID == "" {
			continue
		}
		s.byID[mr.ID] = true
		if issue := strings.TrimSpace(mr.SourceIssue); issue != "" {
			if _, seen := s.byIssue[issue]; !seen {
				s.byIssue[issue] = mr.ID
			}
		}
		if branch := normalizeBranchRef(mr.Branch); branch != "" {
			if _, seen := s.byBranch[branch]; !seen {
				s.byBranch[branch] = mr.ID
			}
		}
	}
	return s
}

// covers returns the id of an open MR that will land the fix of a candidate
// identified by issueID, branch, and close reason, or "" if none does. A
// pending_mr close reason is honored only when the MR it names is in fact
// open — a close reason is a claim about the queue, not the queue itself, so
// a purged or merged-but-closed MR named there must not suppress a finding.
func (s *OpenMRSet) covers(issueID, branch, closeReason string) string {
	if s == nil {
		return ""
	}
	if mrID, ok := s.byIssue[strings.TrimSpace(issueID)]; ok {
		return mrID
	}
	if mrID, ok := s.byBranch[normalizeBranchRef(branch)]; ok {
		return mrID
	}
	if mrID := pendingMRFromCloseReason(closeReason); mrID != "" && s.byID[mrID] {
		return mrID
	}
	return ""
}

// normalizeBranchRef canonicalizes a branch name for comparison: MR beads
// record the short form the polecat pushed, but a stored value may carry a
// refs/heads/ prefix or stray whitespace.
func normalizeBranchRef(branch string) string {
	branch = strings.TrimSpace(branch)
	branch = strings.TrimPrefix(branch, "refs/heads/")
	return branch
}

// pendingMRFromCloseReason returns the MR id named by a "pending_mr: <id>"
// close reason (beads.PendingMergeCloseReason, the reason gt done writes
// when it self-closes a source issue immediately after submitting its MR),
// or "" for any other reason.
func pendingMRFromCloseReason(closeReason string) string {
	const prefix = "pending_mr:"
	trimmed := strings.TrimSpace(closeReason)
	if !strings.HasPrefix(strings.ToLower(trimmed), prefix) {
		return ""
	}
	// Only a bare id: gt done writes nothing after it, so interior prose or a
	// second line means this is not that shape and must not be matched.
	// Surrounding whitespace is trimmed, which keeps the parse equivalent to
	// beads.IsPendingMergeCloseReason's exact-equality test (that one trims
	// the whole reason before comparing).
	mrID := strings.TrimSpace(trimmed[len(prefix):])
	if mrID == "" || strings.IndexFunc(mrID, unicode.IsSpace) >= 0 {
		return ""
	}
	return mrID
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
// Three false-positive classes were measured against the live gastown branch
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
//   - Queued MRs (gt-akap): the ordinary healthy workflow closes the source
//     issue as soon as the polecat submits — "pending_mr: <mr>" — while the
//     MR waits in the queue, so a closed issue with an unmerged branch and
//     a queued MR is indistinguishable from a strand unless the MR queue is
//     consulted. The first shipped version never consulted it, and duly
//     reported "has no MR" for 11 of 13 findings against the live rig, every
//     one of them an MR sitting in the open queue.
//   - Superseded attempts (gt-hsum): an attempt the issue records as rejected,
//     reworked, and re-landed under a different branch leaves its own rejected
//     branch behind. Nothing on the target references the issue (the rework's
//     commit carries no issue token) and the landing MR is purged, so git
//     state cannot tell it from a strand; the issue's own record can, and
//     those candidates are returned in Superseded rather than dropped.
//
// Because the finding asserts the absence of an MR, an unavailable MR
// lookup cannot produce a finding at all: refs.ListOpenMRs is required, and
// if it is missing or fails, the scan reports the error and returns
// MRLookupRan=false with no findings rather than unqualified ones. Callers
// must surface that state (see `gt patrol state-collapse`) — an empty
// finding list from a scan that never ran the lookup is not an all-clear.
func DetectStrandedBranches(bd *BdCli, refs *BranchRefSource, workDir, rigName, targetBranch string, router *mail.Router) *DetectStrandedBranchesResult {
	result := &DetectStrandedBranchesResult{}
	if bd == nil || refs == nil || refs.ListPolecatBranches == nil || refs.TargetHasCommitReferencing == nil {
		return result
	}
	if targetBranch == "" {
		targetBranch = "main"
	}

	if refs.ListOpenMRs == nil {
		result.Errors = append(result.Errors, errNoMRLookup)
		return result
	}
	openMRRefs, err := refs.ListOpenMRs()
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("listing open merge requests: %w", err))
		return result
	}
	openMRs := NewOpenMRSet(openMRRefs)
	result.MRLookupRan = true
	result.OpenMRsSeen = len(openMRs.byID)

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

		record, found := getBeadRecord(bd, workDir, meta.Issue)
		if !found || record.Status != string(beads.StatusClosed) {
			continue // bead unreachable, or not closed — no collapse to report
		}
		result.Checked++

		if isDeliberateDiscard(record.CloseReason) {
			continue // closed with an explicit "no-changes:" abandonment, not a strand
		}

		// The fix may still be in flight: an open MR that names this issue,
		// carries this branch, or is named by a pending_mr close reason means
		// the landing vehicle exists and the branch is simply not merged yet
		// (gt-akap). Reported as a strand only if the queue does not cover it.
		if mrID := openMRs.covers(meta.Issue, branch, record.CloseReason); mrID != "" {
			continue
		}

		// An attempt the issue itself records as rejected, followed by a
		// closure through the merge queue, is a superseded branch rather than
		// an unnoticed strand (gt-hsum). Checked before the target grep: it is
		// pure record inspection, and a recorded rejection is the more
		// specific adjudication when both hold.
		if supersededByRecord(record, branch) {
			result.Superseded = append(result.Superseded, SupersededBranch{
				IssueID: meta.Issue,
				Branch:  branch,
				MRID:    pendingMRFromCloseReason(record.CloseReason),
			})
			continue
		}

		// The close reason may record the supersession directly ("Superseded
		// by <later-issue>: ...") instead of through the pending_mr +
		// rejection-note pair supersededByRecord reads (gt-xpro): a sub-issue
		// in a series closed by hand as folded into a later sub-issue, whose
		// landing commit on the target carries only the later issue's own
		// token. supersededByRecord requires a rejection note that this shape
		// never writes, so it falls through to the target grep below and, not
		// finding this issue's own token there, reports a false strand.
		//
		// Honored only when the named issue's fix is verifiably in force —
		// naming it in a close reason is a claim, not proof — checked with
		// the same target grep the strand check below runs for this issue,
		// just keyed on the later one instead.
		if laterIssue := supersededIssueFromCloseReason(record.CloseReason); laterIssue != "" && laterIssue != meta.Issue {
			inForce, err := refs.TargetHasCommitReferencing(targetBranch, laterIssue)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("checking supersession %s->%s against origin/%s: %w", meta.Issue, laterIssue, targetBranch, err))
			} else if inForce {
				result.Superseded = append(result.Superseded, SupersededBranch{
					IssueID: meta.Issue,
					Branch:  branch,
					MRID:    laterIssue,
				})
				continue
			}
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
			CloseReason: record.CloseReason,
		}
		result.Findings = append(result.Findings, finding)
		reportBranchStrand(bd, workDir, rigName, targetBranch, finding, result.OpenMRsSeen, router)
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

// beadRecord is the slice of a source bead's state the scans reason over.
type beadRecord struct {
	Status      string
	CloseReason string
	Notes       string
}

// getBeadRecord returns a bead's status, close reason, and notes, and whether
// the lookup succeeded. The close reason separates a genuine strand from a
// deliberate discard (gt-3ii); the notes carry the refinery's merge-rejection
// record, which adjudicates rejected branches (gt-hsum).
//
// A field bd does not return reads as empty, which every caller treats as
// "unknown" rather than as evidence. Verified 2026-09-21 against the live
// town: `bd show --json` returns both fields for a bead that has them, checked
// on om-i0p and gt-2ok0 (gt-n899 probed an absent field, not this path).
func getBeadRecord(bd *BdCli, workDir, beadID string) (beadRecord, bool) {
	if beadID == "" {
		return beadRecord{}, false
	}
	output, err := bd.Exec(workDir, "show", beadID, "--json")
	if err != nil || output == "" {
		return beadRecord{}, false
	}
	var issues []struct {
		Status      string `json:"status"`
		CloseReason string `json:"close_reason"`
		Notes       string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(output), &issues); err != nil || len(issues) == 0 {
		// Valid response but no results — bead was reaped/deleted.
		return beadRecord{}, true
	}
	return beadRecord{
		Status:      issues[0].Status,
		CloseReason: issues[0].CloseReason,
		Notes:       issues[0].Notes,
	}, true
}

// supersededByRecord reports whether the issue's own record already adjudicates
// this branch as a rejected attempt rather than an unnoticed strand (gt-hsum):
// a merge-rejection record naming the branch, plus a closure through the merge
// queue afterwards.
//
// Both halves are load-bearing. A rejection alone can still leave a collapse —
// the attempt failed and the issue sits closed with nothing in force — so it
// suppresses only when the close reason shows the issue went back through the
// merge queue. The pair is the system's own record of "this branch was passed
// over"; deriving that verdict from git state instead is the false positive
// this closes.
//
// The record is read rather than the MR because MR beads are purged on merge,
// so a landed MR cannot be looked up once its fix is in force — exactly the
// state being suppressed.
func supersededByRecord(record beadRecord, branch string) bool {
	if pendingMRFromCloseReason(record.CloseReason) == "" {
		return false
	}
	return rejectedBranchFromNotes(record.Notes, branch)
}

// supersededIssueFromCloseReason returns the later issue (or MR) id a close
// reason names as carrying the fix forward ("Superseded by <id>: ...", case
// insensitive), or "" if the reason has no such shape.
//
// This is a distinct adjudication path from supersededByRecord: that one
// reads a rejection note keyed off a "pending_mr:" close reason, the shape
// `gt done` and the refinery write when an attempt is rejected and resubmitted
// through the merge queue. A polecat closing a sub-issue by hand because a
// later sub-issue's commit already supersedes it (gt-xpro) writes neither —
// the supersession is prose in the close reason itself, naming the id that
// replaces this one.
func supersededIssueFromCloseReason(closeReason string) string {
	const marker = "superseded by"
	lower := strings.ToLower(closeReason)
	idx := strings.Index(lower, marker)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(closeReason[idx+len(marker):])
	end := strings.IndexFunc(rest, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ':' || r == ','
	})
	if end == 0 {
		return ""
	}
	if end > 0 {
		rest = rest[:end]
	}
	return strings.TrimRight(rest, ".")
}

// rejectedBranchFromNotes reports whether the issue's notes carry a
// merge-rejection record naming branch.
//
// The record is the refinery's merge-rejection note
// (refinery.MergeRejectionNoteMarker), whose "Branch:" line names the rejected
// branch. The polecat work formula's resume path already greps for that
// marker, so the field is an existing contract rather than one this scan
// invents. The line is compared against the candidate by value, so unrelated
// prose in the notes cannot suppress a finding.
func rejectedBranchFromNotes(notes, branch string) bool {
	want := normalizeBranchRef(branch)
	if want == "" || !strings.Contains(notes, refinery.MergeRejectionNoteMarker) {
		return false
	}
	// Notes accumulate one record per attempt, so only the text following a
	// marker can belong to a rejection.
	for _, block := range strings.Split(notes, refinery.MergeRejectionNoteMarker)[1:] {
		for _, line := range strings.Split(block, "\n") {
			key, value, found := strings.Cut(line, ":")
			if !found || !strings.EqualFold(strings.TrimSpace(key), "branch") {
				continue
			}
			if normalizeBranchRef(value) == want {
				return true
			}
		}
	}
	return false
}

// reportBranchStrand persists a branch-strand finding to the source issue
// (bead comment) and escalates it to the rig's mayor by mail, falling back
// to a tmux nudge if mail delivery fails. Mirrors reportStateCollapse.
//
// openMRsSeen is the size of the open MR queue the scan resolved before
// concluding no MR covers this issue; it is quoted in both the comment and
// the escalation so the reader can tell an absence-of-MR that was actually
// established from one that was assumed (gt-akap).
func reportBranchStrand(bd *BdCli, workDir, rigName, targetBranch string, f BranchStrandFinding, openMRsSeen int, router *mail.Router) {
	// The stranded branch is what identifies this finding; the queue size
	// quoted in the body moves between patrols without the strand being new.
	marker := fmt.Sprintf("branch %q was never merged", f.Branch)
	if alreadyReportedRecently(bd, workDir, f.IssueID, marker, time.Now()) {
		return
	}

	comment := fmt.Sprintf(
		"STATE-COLLAPSE (stranded branch): this issue is closed but branch %q was never merged into origin/%s "+
			"and no open merge-request covers it (checked against the rig's open MR queue: %d MR(s)). "+
			"The fix is recorded as done but may not be in force — verify at source "+
			"(git fetch + git log origin/%s --grep=%s -F) before trusting this close.",
		f.Branch, targetBranch, openMRsSeen, targetBranch, f.IssueID,
	)
	if err := bd.Run(workDir, "comments", "add", f.IssueID, comment); err != nil {
		fmt.Fprintf(os.Stderr, "witness: failed to comment branch-strand on %s: %v\n", f.IssueID, err)
	}

	if router == nil {
		return
	}

	subject := fmt.Sprintf("STATE_COLLAPSE %s closed, branch %s never merged", f.IssueID, f.Branch)
	body := fmt.Sprintf(`Issue %s is CLOSED but its branch %s was never merged into origin/%s — no commit there
references it, and none of the %d open merge request(s) in the queue covers
it (no MR names this issue as source_issue, carries this branch, or is named
by this issue's close reason).

This is the gt-3ii branch-driven state-collapse pattern: strictly worse than
the MR-driven case (gt-zzd), because there is no queue entry to catch it — if
this is a genuine strand, nothing in the system will ever land the fix.
Confirm at source before trusting the close:
  git fetch origin && git log origin/%s --grep=%s -F

If nothing is found, the issue was closed with the fix NOT in force — reopen
it and re-dispatch.`, f.IssueID, f.Branch, targetBranch, openMRsSeen, targetBranch, f.IssueID)

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

// StateCollapseSummary renders the one-line verdict for a completed
// state-collapse scan, and reports whether it is an all-clear.
//
// The all-clear is deliberately conjunctive: zero findings only means "no
// state collapse found" when both detectors actually ran. Both detectors
// reason over the rig's open merge-request queue, and a scan that never
// resolved that queue examined nothing — reporting it as "no state collapse
// found" is precisely the failure this scan exists to catch, a failure
// serializing as success (gt-92ry for the MR-driven half, gt-akap for the
// branch-driven half, where the same wording mistake reported "has no MR"
// for 11 of 13 live findings). allClear=false therefore means "do not trust
// this as a clean bill of health", and the summary names which lookup was
// missing so the operator can tell "checked and clean" from "never ran".
func StateCollapseSummary(mr *DetectStateCollapseResult, branches *DetectStrandedBranchesResult, rigName string) (summary string, allClear bool) {
	mrChecked, mrRan, findings := 0, false, 0
	if mr != nil {
		mrChecked, mrRan = mr.Checked, mr.MRLookupRan
		findings += len(mr.Findings)
	}
	branchChecked, branchRan, superseded := 0, false, 0
	if branches != nil {
		branchChecked, branchRan = branches.Checked, branches.MRLookupRan
		findings += len(branches.Findings)
		superseded = len(branches.Superseded)
	}

	// Suppressions are named, never silent: "0 findings" and "0 findings, 3
	// branches adjudicated" are different claims about the rig.
	suppressed := ""
	if superseded > 0 {
		suppressed = fmt.Sprintf("; %d superseded branch(es) suppressed as already adjudicated", superseded)
	}

	if findings > 0 {
		return fmt.Sprintf("Found %d state collapse(s) in %s (%d open MR(s), %d closed-issue branch(es) checked%s)",
			findings, rigName, mrChecked, branchChecked, suppressed), false
	}
	switch {
	case !mrRan && !branchRan:
		return fmt.Sprintf("No all-clear for %s: MR lookup unavailable — the open merge-request queue could not be resolved, "+
			"so neither check examined anything. Unchecked is not a clean result.", rigName), false
	case !mrRan:
		return fmt.Sprintf("No all-clear for %s: MR lookup unavailable — the open merge-request queue could not be resolved, "+
			"so the MR-driven check examined nothing (%d closed-issue branch(es) were checked by the branch-driven check). "+
			"Unchecked is not a clean result.", rigName, branchChecked), false
	case !branchRan:
		return fmt.Sprintf("No all-clear for %s: MR lookup unavailable — the open merge-request queue could not be resolved, "+
			"so the branch-driven check judged no branch (%d open MR(s) were checked by the MR-driven check). "+
			"Unchecked is not a clean result.", rigName, mrChecked), false
	}
	return fmt.Sprintf("No state collapse found (checked %d open MR(s) — lookup ran; %d closed-issue branch(es) checked%s in %s)",
		mrChecked, branchChecked, suppressed, rigName), true
}
