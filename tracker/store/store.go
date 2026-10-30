package store

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

// ErrUnimplemented is what a Store method returns until its bead lands. It is
// not an API error kind: a server maps it to a 500, which is a bug to fix by
// implementing the method, never a condition a client handles.
var ErrUnimplemented = errors.New("store: not implemented")

// Store is the tracker's data access: everything trackerd does to the
// database. Postgres is the real implementation; storetest.Fake is the
// in-memory one for the unit tier. contract.RunStoreContract holds every
// implementation to the same behavior.
//
// Every write is one transaction that also appends its events. Failures are
// api.Error values (errors.Is against api.ErrNotFound and the other
// sentinels); anything else is an internal error.
type Store interface {
	// CreateIssue creates an issue or wisp and returns it.
	CreateIssue(ctx context.Context, p api.CreateParams) (api.Issue, error)
	// GetIssues returns the issues that exist among ids, keyed by id. A
	// missing id is absent from the map, not an error.
	GetIssues(ctx context.Context, ids []string) (map[string]api.Issue, error)
	// ListIssues returns the page of issues matching f.
	ListIssues(ctx context.Context, f api.ListFilter) (api.IssuePage, error)
	// CountIssues returns how many issues match f, the whole match: a filter
	// that carries a limit or a cursor is refused rather than counted.
	CountIssues(ctx context.Context, f api.ListFilter) (int, error)
	// Assigned returns one page of the issues whose assignee is in
	// req.Assignees and whose status is in req.Statuses, keyed by assignee.
	Assigned(ctx context.Context, req api.AssignedRequest) (api.AssignedPage, error)
	// UpdateIssue applies patch to the issue while g holds; a guard that does
	// not hold returns api.ErrGuardFailed and writes nothing.
	UpdateIssue(ctx context.Context, id string, patch api.UpdatePatch, g api.Guard) error
	// Claim moves the issue to in_progress and assigns it to the actor on the
	// context, so only one of two actors racing for it wins; the other gets
	// api.ErrGuardFailed and nothing is written. The holder re-claiming a live
	// claim writes nothing, and a closed issue is refused.
	Claim(ctx context.Context, id string) error
	// Release returns the issue to open with no assignee, but only while the
	// actor on the context holds it and it is not closed; otherwise
	// api.ErrGuardFailed, no write.
	Release(ctx context.Context, id string) error
	// Transfer assigns the issue to assignee at status (the current status
	// when empty), but only while the actor on the context holds it and it is
	// not closed; otherwise api.ErrGuardFailed, no write. A transfer that
	// changes nothing writes nothing, and a status of closed is refused:
	// closing belongs to CloseIssues and its fences.
	Transfer(ctx context.Context, id, status, assignee string) error
	// CloseIssues closes all of ids with reason, or none: one transaction.
	// force closes past open children and live blockers.
	CloseIssues(ctx context.Context, ids []string, reason string, force bool) error
	// DeleteIssues deletes all of ids with their labels, comments and links
	// in both directions, or none. Their events remain.
	DeleteIssues(ctx context.Context, ids []string) error
	// AddComment appends a comment as author.
	AddComment(ctx context.Context, id, author, text string) error
	// Comments returns the issue's comments, oldest first.
	Comments(ctx context.Context, id string) ([]api.Comment, error)
	// AddDependency records that issue depends on dependsOn with relation
	// depType. dependsOn may be external:<rig>:<id>.
	AddDependency(ctx context.Context, issue, dependsOn, depType string) error
	// RemoveDependency removes that link, whatever its type.
	RemoveDependency(ctx context.Context, issue, dependsOn string) error
	// Dependencies returns what id depends on with relation depType ("" for
	// any), each carrying its relation. External targets are left out.
	Dependencies(ctx context.Context, id, depType string) ([]api.IssueDep, error)
	// Ready returns open, unblocked, dispatchable issues matching f.
	Ready(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error)
	// Blocked returns issues held by an open blocker, matching f.
	Blocked(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error)
	// ChildrenOf returns the direct children of each of parents, wisps
	// included, keyed by parent; a parent with no children is absent.
	ChildrenOf(ctx context.Context, parents []string) (map[string][]api.Issue, error)
	// Batch applies req's operations in order in one transaction: every write
	// commits together, or the first failure rolls back the ones before it.
	// On success the result holds one entry per operation, in request order.
	// A failure answers no results, and its error names the failing
	// operation's index when that operation raised an api.Error.
	Batch(ctx context.Context, req api.BatchRequest) (api.BatchResult, error)
	// CreateGraph creates every node of req and every link between them in one
	// transaction, or nothing: a cycle, a missing target or a bad node leaves no
	// issue behind. Ephemeral nodes get wisp ids. The result maps node keys to
	// the ids created.
	CreateGraph(ctx context.Context, req api.GraphRequest) (api.GraphResult, error)
	// DispatchBoard returns the dispatch tick's reads in one consistent
	// snapshot: per rig the ready issues (no page limit), the held issues and
	// the children of the ready issues, and the status of the probed ids.
	DispatchBoard(ctx context.Context, req api.DispatchBoardRequest) (api.DispatchBoardResponse, error)
	// TownSnapshot returns one page of the town's durable issues with their
	// blocked verdicts, and the counts over the whole set, all read in one
	// repeatable-read transaction so the page and the counts cannot disagree.
	TownSnapshot(ctx context.Context, req api.SnapshotRequest) (api.TownSnapshot, error)
	// Stats counts the durable issues of rig (every rig when empty) by status,
	// and its wisps as one total.
	Stats(ctx context.Context, rig string) (api.Stats, error)
	// ConfigGet returns a configuration key's value; ok is false when unset.
	ConfigGet(ctx context.Context, key string) (value string, ok bool, err error)
	// ConfigSet sets a configuration key, replacing any earlier value.
	ConfigSet(ctx context.Context, key, value string) error
	// IssueTokenScoped creates a token for actor with scope, valid until
	// expires (never, when nil), keeps its hash and returns it: this is the
	// one time the token exists in the clear.
	IssueTokenScoped(ctx context.Context, actor string, scope api.Scope, expires *time.Time) (string, error)
	// Identify returns who token belongs to and its scope, or
	// api.ErrUnknownToken when it is not known, was revoked or has expired.
	Identify(ctx context.Context, token string) (api.Identity, error)
	// RevokeActorTokens revokes every live token of actor and reports how many
	// it ended; an actor with none is a success that ended zero.
	RevokeActorTokens(ctx context.Context, actor string) (int, error)
	// RevokeTokenID revokes the live token id names, reporting false when id
	// names none that is live.
	RevokeTokenID(ctx context.Context, id string) (bool, error)
	// EventsAfter returns up to limit events with seq greater than after
	// (limit 0 means the server's page size), oldest first.
	EventsAfter(ctx context.Context, after int64, limit int) (api.EventsPage, error)
	// Export calls emit for each record of rig's export, in stream order: its
	// issues (wisps included) by id, then their labels, dependencies and
	// comments. Records are read as they are emitted, never collected first.
	// An error from emit or from the read ends the export and is returned.
	Export(ctx context.Context, rig string, emit func(api.ExportRecord) error) error
}
