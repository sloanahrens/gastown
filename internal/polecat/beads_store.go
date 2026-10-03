package polecat

import "github.com/steveyegge/gastown/internal/beads"

// polecatBeads is the bead store the Manager reads and writes: the work-bead
// operations of beads.Client plus the merge-request and reassignment helpers.
// The bd-backed store implements it and so does the test fake; the
// construction in openPolecatBeads is what pins the two.
type polecatBeads interface {
	Show(id string) (*beads.Issue, error)
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	ListByAssignee(assignee string) ([]*beads.Issue, error)
	GetAssignedIssue(assignee string) (*beads.Issue, error)
	ListIssueStatuses(statuses ...beads.IssueStatus) ([]*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	ReleaseIfAssignee(id, expected string) (released bool, err error)
	RecordReassignment(id, from, to, requester string, branches []string) error
	FindMRForBranchAny(branch string) (*beads.Issue, error)
}

// polecatStore is what an injected opener hands back: a database that is both
// the work-bead store and the Client the agent-bead helpers read.
type polecatStore interface {
	polecatBeads
	beads.Client
}

// beadsSite is where the Manager opens its bead store: bd's working
// directory and the resolved .beads it reads.
type beadsSite struct {
	workDir, beadsDir string
}

// openPolecatBeads opens site through open, or bd when open is nil. The
// second store is the one the agent-bead helpers read: for bd it is the
// dual-scope wrapper (ForAgentBead), for an injected store the same store.
// Everything the Manager does through it is a free function over
// beads.Client (gt-7iwy0.4.6).
func openPolecatBeads(open func(beadsSite) polecatStore, site beadsSite) (store polecatBeads, agents beads.Client) {
	if open != nil {
		s := open(site)
		return s, s
	}
	b := beads.NewWithBeadsDir(site.workDir, site.beadsDir)
	return b, beads.ForAgentBead(b)
}
