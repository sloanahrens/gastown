package polecat

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// IssueReader is the subset of beads lookup needed to classify active_mr.
type IssueReader interface {
	Show(issueID string) (*beads.Issue, error)
}

// ActiveMRInput describes the active merge-request context for a polecat.
type ActiveMRInput struct {
	ActiveMR        string
	SourceIssueHint string
	RequireGitSafe  bool
	GitSafe         bool
	// WorkLandedOnMain is caller-supplied evidence that this polecat's work is
	// already contained in the integration branch on origin (see
	// ProbeWorkLandedOnRef). It is the second half of the dangling-pointer
	// gate: an active_mr whose wisp is gone or closed frees the slot only when
	// the work it pointed at is independently proven to have landed.
	WorkLandedOnMain bool
	// WorkLandedRef labels the ref WorkLandedOnMain was measured against.
	WorkLandedRef string
}

// ActiveMRAssessment is the shared active_mr classification used by recovery,
// reuse, and witness paths. Pending is fail-closed: an MR still in the queue,
// an unreadable lookup, or a stale MR with neither a terminal source issue nor
// landed work behind it keeps holding its slot.
type ActiveMRAssessment struct {
	ActiveMR       string
	Pending        bool
	Reason         string
	MRStatus       string
	SourceIssue    string
	SourceTerminal bool
	Stale          bool
	// WorkLandedOnMain records that the stale MR was cleared by landed-work
	// evidence rather than by a terminal source issue.
	WorkLandedOnMain bool
	// WorkLandedRef is the ref that evidence was measured against.
	WorkLandedRef string
}

// AssessActiveMR returns whether active_mr still represents work pending in the
// merge queue. Missing/terminal MRs are stale only when the source issue is
// known terminal or the work itself is proven to be on the integration branch,
// and, if requested, direct git state is safe.
func AssessActiveMR(reader IssueReader, in ActiveMRInput) ActiveMRAssessment {
	mrID := strings.TrimSpace(in.ActiveMR)
	if mrID == "" {
		return ActiveMRAssessment{}
	}
	result := ActiveMRAssessment{ActiveMR: mrID, Pending: true}
	if reader == nil {
		result.Reason = fmt.Sprintf("active_mr=%s status=unverified", mrID)
		return result
	}

	mr, err := reader.Show(mrID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return assessStaleActiveMR(reader, in, result, "missing", nil)
		}
		result.Reason = fmt.Sprintf("active_mr=%s status=lookup_error: %v", mrID, err)
		return result
	}
	if mr == nil {
		return assessStaleActiveMR(reader, in, result, "missing", nil)
	}

	result.MRStatus = mr.Status
	if !beads.IssueStatus(mr.Status).IsTerminal() {
		result.Reason = fmt.Sprintf("active_mr=%s status=%s", mrID, mr.Status)
		return result
	}
	return assessStaleActiveMR(reader, in, result, mr.Status, mr)
}

// AssessActiveMRWithLandedEvidence runs AssessActiveMR and, only when the MR
// itself is verifiably gone or closed and the assessment is still pending,
// retries with git evidence that the work that MR carried is already on the
// remote integration branch.
//
// The probe is lazy because it costs a git call per polecat: an MR that is
// still in the queue is decided without it, and so is a dangling MR already
// cleared by a terminal source issue. A nil probe (a caller with no worktree
// to measure) leaves the assessment exactly as AssessActiveMR returned it.
//
// Fail-closed: the probe is consulted only for a *stale* MR, and a probe that
// cannot prove the work landed changes nothing. A lookup error is not stale,
// so an unreadable MR never reaches the probe at all.
func AssessActiveMRWithLandedEvidence(reader IssueReader, in ActiveMRInput, probe LandedEvidenceProbe) ActiveMRAssessment {
	assessment := AssessActiveMR(reader, in)
	if !assessment.Pending || !assessment.Stale || probe == nil {
		return assessment
	}
	evidence := probe()
	if !evidence.Verified {
		return assessment
	}
	in.WorkLandedOnMain = true
	in.WorkLandedRef = evidence.Ref
	return AssessActiveMR(reader, in)
}

func assessStaleActiveMR(reader IssueReader, in ActiveMRInput, result ActiveMRAssessment, mrStatus string, mr *beads.Issue) ActiveMRAssessment {
	result.MRStatus = mrStatus
	result.Stale = true
	result.WorkLandedOnMain = in.WorkLandedOnMain
	result.WorkLandedRef = strings.TrimSpace(in.WorkLandedRef)
	sourceIssue := sourceIssueForActiveMR(in.SourceIssueHint, mr)
	result.SourceIssue = sourceIssue
	terminal, reason := terminalSourceIssue(reader, sourceIssue)
	result.SourceTerminal = terminal
	// Two independent ways to know a closed MR holds nothing: the source issue
	// reached a terminal state, or the work itself is already contained in the
	// integration branch on origin. Either one alone clears the MR; neither is
	// assumed, so a dangling pointer with no landed proof still blocks.
	//
	// The landed proof also stands in for RequireGitSafe: a clean local tree is
	// weaker evidence than "this work is on origin/main" (a dirty tree can be
	// leftover junk next to work that already landed). Clearing the MR blocker
	// never hides that dirt — with the blocker gone the classifier falls
	// through to the git predicates, so the verdict becomes NEEDS_RECOVERY
	// instead of a PENDING_MR that outlives its MR.
	if !terminal && !result.WorkLandedOnMain {
		result.Reason = fmt.Sprintf("active_mr=%s status=%s %s", result.ActiveMR, mrStatus, reason)
		return result
	}
	if in.RequireGitSafe && !in.GitSafe && !result.WorkLandedOnMain {
		result.Reason = fmt.Sprintf("active_mr=%s status=%s source_issue=%s git_state=unsafe", result.ActiveMR, mrStatus, sourceIssue)
		return result
	}
	result.Pending = false
	result.Reason = ""
	return result
}

func sourceIssueForActiveMR(hint string, mr *beads.Issue) string {
	if mr != nil {
		if fields := beads.ParseMRFields(mr); fields != nil {
			if source := normalizeSourceIssue(fields.SourceIssue); source != "" {
				return source
			}
		}
	}
	return normalizeSourceIssue(hint)
}

func normalizeSourceIssue(source string) string {
	source = strings.TrimSpace(source)
	if strings.EqualFold(source, "null") {
		return ""
	}
	return source
}

func terminalSourceIssue(reader IssueReader, sourceIssue string) (bool, string) {
	if sourceIssue == "" {
		return false, "source_issue=<missing>"
	}
	if reader == nil {
		return false, fmt.Sprintf("source_issue=%s source_status=unverified", sourceIssue)
	}
	issue, err := reader.Show(sourceIssue)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return false, fmt.Sprintf("source_issue=%s source_status=missing", sourceIssue)
		}
		return false, fmt.Sprintf("source_issue=%s source_status=lookup_error: %v", sourceIssue, err)
	}
	if issue == nil {
		return false, fmt.Sprintf("source_issue=%s source_status=missing", sourceIssue)
	}
	if beads.IssueStatus(issue.Status).IsTerminal() {
		return true, ""
	}
	return false, fmt.Sprintf("source_issue=%s source_status=%s", sourceIssue, issue.Status)
}
