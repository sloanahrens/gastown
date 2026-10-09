package beads

// issueSnapshot is the issues-table rows one PreloadBeads read returned, and
// what that read covers: every issue carrying any of labels (in any status),
// and every issue in any of statuses. A reader may answer from it only when its
// question falls inside that coverage; every method reports ok=false otherwise
// so the caller runs its own bd subprocess as before. All methods are safe on a
// nil *issueSnapshot, which means "not warmed".
type issueSnapshot struct {
	labels   map[string]bool
	statuses map[IssueStatus]bool
	issues   []*Issue
	details  *preloadDetails
}

// detail returns the issue row the read returned for id, with the dependency
// rows the same read carried, for hydration (see preloadDetails). ok is false
// for an id this read did not return — including on a nil snapshot, which
// means "not warmed".
func (s *issueSnapshot) detail(id string) (*Issue, bool) {
	if s == nil {
		return nil, false
	}
	return s.details.detail(id)
}

// list answers a listIssues(opts) call — `bd list` over the issues table —
// for a label the snapshot was warmed for, with the filters that read used:
// none beyond the label and a status of "", "open", "closed" or "all". Any
// other filter (priority, parent, assignee, another label, a created-after
// bound, a limit, another status spelling) is not covered, so the caller runs
// bd list itself.
//
// Two of bd list's defaults are reproduced. An empty status means "not
// closed", and ephemeral issues are hidden. bd list also hides infrastructure
// issue types unless asked; that only matters for a label whose issues can
// have such a type, and the labels this snapshot is warmed for (gt:agent,
// which has its own reader below, and gt:merge-request) do not.
func (s *issueSnapshot) list(opts ListOptions) ([]*Issue, bool) {
	if s == nil || opts.Label == "" || !s.labels[opts.Label] {
		return nil, false
	}
	if _, filtersPriority := opts.Priority.bd(); filtersPriority || opts.Parent != "" || opts.Assignee != "" || opts.NoAssignee || opts.Limit != 0 || opts.Ephemeral || opts.IssueType != "" || !opts.ClosedAfter.IsZero() || !opts.CreatedAfter.IsZero() || len(opts.Labels) > 0 {
		return nil, false
	}
	switch opts.Status {
	case "", "open", "closed", "all":
	default:
		return nil, false
	}

	var out []*Issue
	for _, issue := range s.issues {
		if issue.Ephemeral || !HasLabel(issue, opts.Label) {
			continue
		}
		switch opts.Status {
		case "":
			if IssueStatus(issue.Status) == StatusClosed {
				continue
			}
		case "all":
		default:
			if issue.Status != opts.Status {
				continue
			}
		}
		out = append(out, issue)
	}
	return out, true
}

// agentBeads answers ListAgentBeads' issues-table read: `bd list
// --label=gt:agent --include-infra`, i.e. every not-closed gt:agent issue,
// ephemeral or not.
func (s *issueSnapshot) agentBeads() ([]*Issue, bool) {
	const label = "gt:agent"
	if s == nil || !s.labels[label] {
		return nil, false
	}
	var out []*Issue
	for _, issue := range s.issues {
		if HasLabel(issue, label) && IssueStatus(issue.Status) != StatusClosed {
			out = append(out, issue)
		}
	}
	return out, true
}

// byStatus answers ListIssueStatuses: the durable (non-ephemeral) issues in any
// of statuses. It is only covered when every one of statuses was warmed.
func (s *issueSnapshot) byStatus(statuses []IssueStatus) ([]*Issue, bool) {
	if s == nil {
		return nil, false
	}
	want := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		if !s.statuses[status] {
			return nil, false
		}
		want[string(status)] = true
	}
	var out []*Issue
	for _, issue := range s.issues {
		if !issue.Ephemeral && want[issue.Status] {
			out = append(out, issue)
		}
	}
	return out, true
}
