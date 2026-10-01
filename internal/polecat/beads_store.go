package polecat

import "github.com/steveyegge/gastown/internal/beads"

// polecatBeads is the bead store the Manager reads and writes: the work-bead
// operations of beads.Client plus the agent-bead and merge-request helpers
// only *beads.Beads carries. *beads.Beads implements it; unit tests answer
// from beadsfake.
type polecatBeads interface {
	Show(id string) (*beads.Issue, error)
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	ListByAssignee(assignee string) ([]*beads.Issue, error)
	GetAssignedIssue(assignee string) (*beads.Issue, error)
	ListIssueStatuses(statuses ...beads.IssueStatus) ([]*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	ReleaseIfAssignee(id, expected string) (released bool, err error)
	RecordReassignment(id, from, to, requester string, branches []string) error
	FindMRForBranch(branch string) (*beads.Issue, error)
	FindMRForBranchAny(branch string) (*beads.Issue, error)

	GetAgentBead(id string) (*beads.Issue, *beads.AgentFields, error)
	CreateOrReopenAgentBead(id, title string, fields *beads.AgentFields) (*beads.Issue, error)
	ResetAgentBeadForReuse(id, reason string) error
	UpdateAgentState(id, state string) error
	ListAgentBeads() (map[string]*beads.Issue, error)
}

var _ polecatBeads = (*beads.Beads)(nil)

// beadsSite is where the Manager opens its bead store: bd's working
// directory and the resolved .beads it reads.
type beadsSite struct {
	workDir, beadsDir string
}

// openPolecatBeads opens site through open, or bd when open is nil. The
// second store is the one agent beads go through: for bd it is the
// dual-scope wrapper (ForAgentBead), for an injected store the same store.
func openPolecatBeads(open func(beadsSite) polecatBeads, site beadsSite) (store, agents polecatBeads) {
	if open != nil {
		s := open(site)
		return s, s
	}
	b := beads.NewWithBeadsDir(site.workDir, site.beadsDir)
	return b, b.ForAgentBead()
}
