package bdshim

import (
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/backend"
)

// bdView is the store as bd answers it: a dependency on a target outside this
// database (external:<rig>:<id>) is left out of Show and ShowMultiple, as bd's
// own join leaves it out. The tracker keeps the whole dependency set and lists
// such a link id-only; the shim answers as bd, and bdShowRow refuses to print
// target fields for a row the store does not have, so this view drops the
// id-only entries before a handler renders them.
//
// gt's own reads use the seam directly and see the whole set. Dependents need
// no view: a dependent is an issue in this database, so it is never id-only.
type bdView struct{ backend.Store }

// Show returns one issue with the links bd would print.
func (v bdView) Show(id string) (*beads.Issue, error) {
	is, err := v.Store.Show(id)
	if err != nil {
		return nil, err
	}
	return withoutExternalDeps(is), nil
}

// ShowMultiple returns the issues that exist among ids, keyed by ID, each with
// the links bd would print.
func (v bdView) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	m, err := v.Store.ShowMultiple(ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*beads.Issue, len(m))
	for id, is := range m {
		out[id] = withoutExternalDeps(is)
	}
	return out, nil
}

// withoutExternalDeps returns is without its id-only links, as a copy that
// leaves the store's issue alone.
func withoutExternalDeps(is *beads.Issue) *beads.Issue {
	if is == nil || !slices.ContainsFunc(is.Dependencies, isExternalDep) {
		return is
	}
	out := *is
	out.Dependencies = slices.DeleteFunc(slices.Clone(is.Dependencies), isExternalDep)
	return &out
}

// isExternalDep reports whether d names a target outside this database. The
// store writes such a link as external:<rig>:<id> and lists it id-only.
func isExternalDep(d beads.IssueDep) bool { return strings.HasPrefix(d.ID, "external:") }
