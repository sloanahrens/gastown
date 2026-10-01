package beads

import "time"

// Client is the issue-store surface that code outside this package uses
// most: reading, listing, creating, updating and closing issues, their
// comments and dependencies, the ready queue, and claim release. *Beads
// implements it against bd; internal/beads/beadsfake implements it in memory
// for unit tests, and beadsfake.RunClientContract pins the two to the same
// behavior.
//
// The interface is deliberately small. A method joins it only when consumers
// in several packages call it and the fake can model it faithfully; the rest
// of *Beads (agent, merge-request, channel and other domain helpers built on
// these primitives) stays on the concrete type. Consumers that need less
// should declare their own narrower interface.
type Client interface {
	// Show returns one issue, or an error wrapping ErrNotFound.
	Show(id string) (*Issue, error)
	// ShowMultiple returns the issues that exist among ids, keyed by ID.
	ShowMultiple(ids []string) (map[string]*Issue, error)
	// List returns the issues matching opts.
	List(opts ListOptions) ([]*Issue, error)
	// ListByAssignee returns every issue, open or closed, assigned to assignee.
	ListByAssignee(assignee string) ([]*Issue, error)
	// GetAssignedIssue returns assignee's issue, preferring open over
	// in_progress over hooked; nil when there is none.
	GetAssignedIssue(assignee string) (*Issue, error)
	// ListIssueStatuses returns the durable issues in any of statuses.
	ListIssueStatuses(statuses ...IssueStatus) ([]*Issue, error)
	// ListAssignedIssueStatuses returns assignee's issues and wisps in any
	// of statuses.
	ListAssignedIssueStatuses(assignee string, statuses ...IssueStatus) ([]*Issue, error)
	// Ready returns open, unblocked, dispatchable issues.
	Ready() ([]*Issue, error)
	// ReadyAll returns every issue Ready would, without bd's default page
	// cap; a page bd reports as cut is an error.
	ReadyAll() ([]*Issue, error)
	// Children returns the direct children of parentID.
	Children(parentID string) ([]*Issue, error)
	// Comments returns the comments on an issue, oldest first.
	Comments(id string) ([]Comment, error)

	// Create creates an issue and returns it.
	Create(opts CreateOptions) (*Issue, error)
	// Update changes the fields opts sets.
	Update(id string, opts UpdateOptions) error
	// Close closes issues.
	Close(ids ...string) error
	// CloseWithReason closes issues and records reason.
	CloseWithReason(reason string, ids ...string) error
	// ForceCloseWithReason is CloseWithReason past bd's close fences.
	ForceCloseWithReason(reason string, ids ...string) error
	// Release returns a claimed issue to open and clears its assignee.
	Release(id string) error
	// ReleaseWithReason is Release, recording reason in the notes.
	ReleaseWithReason(id, reason string) error
	// AddComment appends a comment.
	AddComment(id, comment string) error
	// AddDependency makes issue depend on (be blocked by) dependsOn.
	AddDependency(issue, dependsOn string) error
	// AddTypedDependency records that issue depends on dependsOn with
	// relation depType (bd's dependency_type: "tracks", "blocks", ...). An
	// external:<rig>:<id> target need not exist in this database.
	AddTypedDependency(issue, dependsOn, depType string) error
	// RemoveDependency removes that dependency, whatever its type.
	RemoveDependency(issue, dependsOn string) error
	// AppendNotes appends note to the issue's notes, on a new line when
	// there are notes already.
	AppendNotes(id, note string) error

	// ReleaseIfAssignee returns the issue to open with no assignee, but only
	// while expected still holds it. released=false with a nil error means
	// the guard no longer held and nothing was written.
	ReleaseIfAssignee(id, expected string) (released bool, err error)
	// TransferIfAssignee sets the issue's status and assignee, but only
	// while expected still holds it; the guard replaces bd's claim fence.
	// transferred=false with a nil error means nothing was written.
	TransferIfAssignee(id, expected, status, assignee string) (transferred bool, err error)
}

var _ Client = (*Beads)(nil)

// Admin is the maintenance surface beadsfake models: config keys, table
// probes, counts, stats, the wisp list, gc's dry-run candidates and the
// events journal.
// beadsfake.RunAdminContract pins the fake to *Beads on it. (SQLCSV and
// InitDatabase are on *Beads too, but a fake can only script or record them.)
type Admin interface {
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error
	CountIssues() (int, error)
	TableExists(name string) bool
	StatsJSON() ([]byte, error)
	MolWispList() ([]*Issue, error)
	WispGCCandidates(age time.Duration) ([]string, error)
	// EventsTail reads the events journal after since, at most limit
	// records (0 = all); a pruned-past since is *EventsTruncatedError.
	EventsTail(since int64, limit int) (*EventsPage, error)
}

var _ Admin = (*Beads)(nil)
