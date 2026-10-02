package convoy

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// IssueSource is the read surface the convoy operations need from an issue
// store, in beads types: one issue, many issues, its comments, and its raw
// dependency edges. ClientSource is the production implementation, over the
// bd-backed Store a Town hands out. The daemon reaches the same surface
// through a temporary in-process adapter until its own store slice lands
// (gt-7iwy0.3.1).
type IssueSource interface {
	// Show returns one issue, or an error wrapping beads.ErrNotFound when id
	// is absent.
	Show(id string) (*beads.Issue, error)

	// ShowMultiple returns the issues that exist among ids, keyed by ID.
	ShowMultiple(ids []string) (map[string]*beads.Issue, error)

	// Comments returns the comments on an issue, oldest first.
	Comments(id string) ([]beads.Comment, error)

	// Deps returns the ids related to issueID by a depType edge, any
	// external:<prefix>:<id> wrapper still on them: with direction "up", the
	// ids that depend on issueID; otherwise the ids issueID depends on.
	// depType "" is every relation. It is the raw read bd's joined views
	// cannot answer, because they drop every target outside the issue's own
	// database — every cross-rig edge (gt-j02xy).
	Deps(issueID, direction, depType string) ([]string, error)

	// TrackedBy returns the ids holding a "tracks" edge to target: the
	// convoys tracking an issue. It reads external targets, which is what
	// lets a town convoy track a rig's bead.
	TrackedBy(target string) ([]string, error)

	// Close releases the source's store, when the source owns one. A store
	// the caller handed in is the caller's to close.
	Close() error
}

// ClientSource is the IssueSource over a bd-backed Store: the issue and
// comment reads go through beads.Client, and the raw dependency read through
// bd's own sql, which skips the join that hides cross-rig targets.
func ClientSource(store Store) IssueSource { return clientSource{store} }

type clientSource struct{ store Store }

func (s clientSource) Show(id string) (*beads.Issue, error) { return s.store.Show(id) }

func (s clientSource) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	return s.store.ShowMultiple(ids)
}

func (s clientSource) Comments(id string) ([]beads.Comment, error) { return s.store.Comments(id) }

// Deps reads the raw dependency rows of issueID. The id column carries the
// relation's direction — bd names the dependent side issue_id and the target
// side depends_on_id — so the parser key follows it.
func (s clientSource) Deps(issueID, direction, depType string) ([]string, error) {
	key := "depends_on_id"
	if direction == "up" {
		key = "issue_id"
	}
	rows, err := s.store.SQLCSV(beadsql.RawDeps(issueID, direction, depType))
	if err != nil {
		return nil, fmt.Errorf("bd sql for deps of %s: %w", issueID, err)
	}
	return parseRawDepRows(rows, key)
}

// TrackedBy is the up/"tracks" read that names a bead's tracking convoys.
func (s clientSource) TrackedBy(target string) ([]string, error) {
	return s.Deps(target, "up", "tracks")
}

// Close is a no-op: bd runs as a subprocess, so there is nothing to release,
// and the Store is the caller's.
func (s clientSource) Close() error { return nil }
