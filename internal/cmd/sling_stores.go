package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/workspace"
)

// slingStore is the bead store surface sling's reads, rollback and molecule
// burn use: the shared Client, the raw molecule-bond query and the audited
// molecule detach. *beads.Beads implements it.
type slingStore interface {
	beads.Client
	SQLCSV(query beadsql.Query) ([][]string, error)
	DetachMoleculeWithAudit(id string, opts beads.DetachOptions) (*beads.Issue, error)
}

// slingStores opens the bead stores sling works in. The zero value is bd;
// unit tests answer from beadsfake databases.
type slingStores struct {
	// pinned opens the database in beadsDir alone, with no prefix routing;
	// nil is beads.NewPinned.
	pinned func(beadsDir string) slingStore
	// routed opens a store at dir that routes each ID by its prefix; nil is
	// beads.NewWithBeadsDir.
	routed func(dir string) slingStore
}

// pinnedDB is the database in beadsDir alone.
func (s slingStores) pinnedDB(beadsDir string) slingStore {
	if s.pinned != nil {
		return s.pinned(beadsDir)
	}
	return beads.NewPinned(beadsDir)
}

// pinnedAt is dir's resolved database alone.
func (s slingStores) pinnedAt(dir string) slingStore {
	return s.pinnedDB(beads.ResolveBeadsDir(dir))
}

// routedFrom routes each ID by prefix from dir.
func (s slingStores) routedFrom(dir string) slingStore {
	if s.routed != nil {
		return s.routed(dir)
	}
	return beads.NewWithBeadsDir(dir, "")
}

// show reads beadID from its own rig's database, then, when that fails, by
// prefix routing from the town root. An empty townRoot is the cwd's town.
func (s slingStores) show(townRoot, beadID string) (*beads.Issue, error) {
	if townRoot == "" {
		if root, err := workspace.FindFromCwdOrError(); err == nil {
			townRoot = root
		}
	}
	issue, err := s.pinnedAt(resolveBeadDirFromTownRoot(townRoot, beadID)).Show(beadID)
	if err == nil {
		return issue, nil
	}
	if townRoot != "" {
		if routed, routedErr := s.routedFrom(townRoot).Show(beadID); routedErr == nil {
			return routed, nil
		}
	}
	return nil, err
}

// beadInfo is show as the sling guards read it.
func (s slingStores) beadInfo(townRoot, beadID string) (*beadInfo, error) {
	issue, err := s.show(townRoot, beadID)
	if err != nil {
		return nil, fmt.Errorf("bead '%s' not found", beadID)
	}
	return beadInfoOf(issue), nil
}

// beadInfoOf is the part of issue the sling guards read.
func beadInfoOf(issue *beads.Issue) *beadInfo {
	return &beadInfo{
		Title:        issue.Title,
		Status:       issue.Status,
		Assignee:     issue.Assignee,
		Description:  issue.Description,
		Design:       issue.Design,
		Notes:        issue.Notes,
		Labels:       issue.Labels,
		Dependencies: issue.Dependencies,
		IssueType:    issue.Type,
	}
}

// showBead is slingStores.show over bd.
func showBead(townRoot, beadID string) (*beads.Issue, error) {
	return slingStores{}.show(townRoot, beadID)
}
