package refinery

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// terminalMRCloseOptions carries the close-time fields that closeTerminalMR
// needs so postMergeMR can pass its verified merge_commit. MergeCommit is the
// SHA verified to contain the landed content at close time; AgentBeadHint is
// the agent bead that owned this MR (used to clear its active_mr), and
// ExpectedMR is the fully-loaded MR snapshot taken before merge so close can
// CAS branch/source_issue/commit_sha (and target when present) against the
// values the merge proof verified (gt-5k1g).
//
// InferredCommitSHA is the recovered branch head for an MR whose bead recorded
// no commit_sha at submission (gt-6o1u). When set, the bead's commit_sha is
// written to that value and commit_sha_inferred is set to true at close, so
// the record names the head that the merge proof actually verified; an
// already-recorded commit_sha must match it, not merely be empty.
type terminalMRCloseOptions struct {
	Reason            string
	MergeCommit       string
	AgentBeadHint     string
	MissingOK         bool
	ExpectedMR        *MergeRequest
	InferredCommitSHA string
}

type terminalMRCloseResult struct {
	MRID                  string
	SourceIssue           string
	AgentBead             string
	Closed                bool
	AlreadyTerminal       bool
	AgentActiveMRCleared  bool
	AgentActiveMRClearErr error
}

func closeTerminalMR(b *beads.Beads, mrID string, opts terminalMRCloseOptions) (*terminalMRCloseResult, error) {
	mrID = strings.TrimSpace(mrID)
	result := &terminalMRCloseResult{MRID: mrID}
	if b == nil || mrID == "" {
		return result, nil
	}

	issue, err := b.Show(mrID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) && opts.MissingOK {
			return result, nil
		}
		return result, fmt.Errorf("fetch MR for close: %w", err)
	}
	if issue == nil {
		return result, nil
	}

	fields := beads.ParseMRFields(issue)
	if fields == nil {
		fields = &beads.MRFields{}
	}
	result.SourceIssue = strings.TrimSpace(fields.SourceIssue)
	result.AgentBead = firstNonEmpty(opts.AgentBeadHint, fields.AgentBead)
	if err := validateTerminalMRCloseSnapshot(mrID, fields, opts.ExpectedMR); err != nil {
		return result, err
	}
	if err := validateInferredCommitSHA(mrID, fields, opts.InferredCommitSHA, opts.ExpectedMR); err != nil {
		return result, err
	}
	// The CAS above compared against the bead as it was before close. Only
	// now may the inferred head be recorded, so a post-proof change lands as
	// a rejected close rather than a silently rewritten audit record. The
	// fill is gated on the same snapshot attestation as the guard above: a
	// plain (non-inferred) snapshot never rewrites the bead's commit_sha.
	if opts.InferredCommitSHA != "" && opts.ExpectedMR != nil && opts.ExpectedMR.CommitSHAInferred {
		fields.CommitSHA = opts.InferredCommitSHA
		fields.CommitSHAInferred = true
	}

	status := beads.IssueStatus(strings.TrimSpace(issue.Status))
	switch {
	case status == beads.StatusOpen:
		if opts.MergeCommit != "" {
			fields.MergeCommit = opts.MergeCommit
		}
		if closeReason := normalizedMRCloseReason(opts.Reason); closeReason != "" {
			fields.CloseReason = closeReason
		}
		if result.AgentBead != "" && strings.TrimSpace(fields.AgentBead) == "" {
			fields.AgentBead = result.AgentBead
		}

		newDesc := beads.SetMRFields(issue, fields)
		if err := b.Update(mrID, beads.UpdateOptions{Description: &newDesc}); err != nil {
			return result, fmt.Errorf("record MR close metadata: %w", err)
		}
		if err := b.CloseWithReason(opts.Reason, mrID); err != nil {
			return result, fmt.Errorf("close MR: %w", err)
		}
		result.Closed = true
	case status.IsTerminal():
		result.AlreadyTerminal = true
	default:
		return result, nil
	}

	if result.AgentBead != "" {
		cleared, clearErr := b.ForAgentBead().ClearAgentActiveMRIfMatches(result.AgentBead, mrID)
		result.AgentActiveMRCleared = cleared
		result.AgentActiveMRClearErr = clearErr
	}
	return result, nil
}

func validateTerminalMRCloseSnapshot(mrID string, fields *beads.MRFields, expected *MergeRequest) error {
	if expected == nil || fields == nil {
		return nil
	}
	checks := []struct {
		name string
		got  string
		want string
	}{
		{name: "branch", got: fields.Branch, want: expected.Branch},
		{name: "source_issue", got: fields.SourceIssue, want: expected.IssueID},
		{name: "commit_sha", got: fields.CommitSHA, want: expected.CommitSHA},
	}
	if strings.TrimSpace(expected.TargetBranch) != "" {
		checks = append(checks, struct {
			name string
			got  string
			want string
		}{name: "target", got: fields.Target, want: expected.TargetBranch})
	}
	for _, check := range checks {
		got := strings.TrimSpace(check.got)
		want := strings.TrimSpace(check.want)
		if want != "" && got != want {
			return fmt.Errorf("MR %s changed after merge proof: %s=%q, verified %q", mrID, check.name, got, want)
		}
	}
	return nil
}

// validateInferredCommitSHA is the CAS for a head recovered from the branch
// because the MR bead recorded no commit_sha at submission (gt-6o1u). The
// expected snapshot must have been marked inferred; otherwise the recorded
// commit_sha is the submission identity and the regular snapshot CAS is the
// right tool. When the bead recorded nothing the close fills it; when the
// bead (or a race) recorded a different head, the close is refused so the
// divergence is surfaced instead of papered over.
func validateInferredCommitSHA(mrID string, fields *beads.MRFields, inferred string, expected *MergeRequest) error {
	if inferred == "" || fields == nil || expected == nil || !expected.CommitSHAInferred {
		return nil
	}
	if got := strings.TrimSpace(fields.CommitSHA); got != "" && got != inferred {
		return fmt.Errorf("MR %s changed after merge proof: recorded commit_sha %q differs from the verified inferred head %q", mrID, got, inferred)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
