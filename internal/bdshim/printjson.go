package bdshim

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/beads"
)

// This file is the shim's bd-shaped JSON printer: the shared plumbing every
// tracker handler writes its answer through, so a verb added later prints the
// same envelope and the same rows as the ones already answered.
//
// The shapes below are bd's: a {"data": [...], "schema_version": 1} envelope,
// the members of each row in the order bd printed them in the recorded goldens
// (cmd/bdshim/testdata/golden), and no member bd left out. The rows carry the
// fields a golden holds; a bead field no golden holds yet (assignee, notes,
// design) is left out until the bead that records it adds a row.

// bdSchemaVersion is the schema_version bd writes on every --json answer.
const bdSchemaVersion = 1

// parentChild is the dependency type bd leaves out of the dependency and
// dependent counts a list prints, while keeping the link in the dependencies
// array. A parent-child link is structure, not work waiting on work.
const parentChild = "parent-child"

// bdListAnswer is bd's answer to list and children: the rows, then the schema
// version.
type bdListAnswer struct {
	Data          []bdListIssue `json:"data"`
	SchemaVersion int           `json:"schema_version"`
}

// bdListIssue is one issue as bd's list and children print it.
type bdListIssue struct {
	ID              string      `json:"id"`
	Title           string      `json:"title"`
	Description     string      `json:"description"`
	Status          string      `json:"status"`
	Priority        int         `json:"priority"`
	Type            string      `json:"issue_type"`
	CreatedAt       string      `json:"created_at"`
	UpdatedAt       string      `json:"updated_at"`
	ClosedAt        string      `json:"closed_at,omitempty"`
	CloseReason     string      `json:"close_reason,omitempty"`
	Labels          []string    `json:"labels,omitempty"`
	Dependencies    []bdListDep `json:"dependencies,omitempty"`
	DependencyCount int         `json:"dependency_count"`
	DependentCount  int         `json:"dependent_count"`
	CommentCount    int         `json:"comment_count"`
	Parent          string      `json:"parent,omitempty"`
}

// bdListDep is one link in a listed issue's dependencies array, as bd stores
// it: the relation's own row, not the target's fields.
type bdListDep struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	CreatedAt   string `json:"created_at"`
	CreatedBy   string `json:"created_by"`
	Metadata    string `json:"metadata"`
}

// bdShowAnswer is bd's answer to show: the issues named, then the schema
// version.
type bdShowAnswer struct {
	Data          []bdShowIssue `json:"data"`
	SchemaVersion int           `json:"schema_version"`
}

// bdShowIssue is one issue as bd's show prints it. bd's show carries more than
// its list does: each dependency with its target's fields, the comment count
// with the comments themselves left out, and a revision.
type bdShowIssue struct {
	ID              string      `json:"id"`
	Title           string      `json:"title"`
	Description     string      `json:"description"`
	Status          string      `json:"status"`
	Priority        int         `json:"priority"`
	Type            string      `json:"issue_type"`
	CreatedAt       string      `json:"created_at"`
	UpdatedAt       string      `json:"updated_at"`
	ClosedAt        string      `json:"closed_at,omitempty"`
	CloseReason     string      `json:"close_reason,omitempty"`
	Labels          []string    `json:"labels,omitempty"`
	Dependencies    []bdShowDep `json:"dependencies,omitempty"`
	Parent          string      `json:"parent,omitempty"`
	DependentCount  int         `json:"dependent_count"`
	DependencyCount int         `json:"dependency_count"`
	CommentCount    int         `json:"comment_count"`
	CommentsOmitted bool        `json:"comments_omitted"`
	Revision        int         `json:"revision"`
}

// bdShowDep is one dependency as bd's show prints it: the target's fields with
// the relation between them.
type bdShowDep struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	Status         string `json:"status"`
	Priority       int    `json:"priority"`
	Type           string `json:"issue_type"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	DependencyType string `json:"dependency_type"`
}

// bdListRow renders one issue as a list or children row. The counts bd prints
// leave parent-child links out, as bd does; the dependencies array keeps them.
func bdListRow(is *beads.Issue, commentCount int) bdListIssue {
	deps := make([]bdListDep, 0, len(is.Dependencies))
	for _, d := range is.Dependencies {
		deps = append(deps, bdListDep{
			IssueID: is.ID, DependsOnID: d.ID, Type: d.DependencyType,
			CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy, Metadata: bdMetadata(d.Metadata),
		})
	}
	return bdListIssue{
		ID: is.ID, Title: is.Title, Description: is.Description, Status: is.Status,
		Priority: is.Priority, Type: is.Type, CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt,
		ClosedAt: is.ClosedAt, CloseReason: is.CloseReason, Labels: is.Labels, Dependencies: deps,
		DependencyCount: countWorkRelations(is.Dependencies), DependentCount: countWorkRelations(is.Dependents),
		CommentCount: commentCount, Parent: is.Parent,
	}
}

// bdShowRow renders one issue as a show row. Every dependency's target must be
// among targets: bd prints the target's own fields, and a row with an empty
// description would be an answer the shim guessed.
//
// show's counts are the raw link counts and keep a parent-child link, where the
// list's use countWorkRelations. The recorded rows agree on dependent_count (0)
// and differ on dependency_count: show says 2 for fx-1, list says 1, the
// difference being the link to its parent.
func bdShowRow(is *beads.Issue, targets map[string]*beads.Issue, commentCount int) (bdShowIssue, error) {
	deps := make([]bdShowDep, 0, len(is.Dependencies))
	for _, d := range is.Dependencies {
		t, ok := targets[d.ID]
		if !ok {
			return bdShowIssue{}, fmt.Errorf("show %s: the store did not answer the bead %s it depends on", is.ID, d.ID)
		}
		deps = append(deps, bdShowDep{
			ID: t.ID, Title: t.Title, Description: t.Description, Status: t.Status,
			Priority: t.Priority, Type: t.Type, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			DependencyType: d.DependencyType,
		})
	}
	return bdShowIssue{
		ID: is.ID, Title: is.Title, Description: is.Description, Status: is.Status,
		Priority: is.Priority, Type: is.Type, CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt,
		ClosedAt: is.ClosedAt, CloseReason: is.CloseReason, Labels: is.Labels, Dependencies: deps,
		Parent: is.Parent, DependentCount: len(is.Dependents), DependencyCount: len(is.Dependencies),
		CommentCount: commentCount,
		// The shim answers without fetching the comments, as bd does unless
		// --comments is given, and the tracker keeps no revision counter, so
		// the row carries bd's initial revision.
		CommentsOmitted: true, Revision: 0,
	}, nil
}

// countWorkRelations counts the links a list's count prints: every relation but
// parent-child. show does not use it: bdShowRow prints the raw link counts.
func countWorkRelations(deps []beads.IssueDep) int {
	n := 0
	for _, d := range deps {
		if d.DependencyType != parentChild {
			n++
		}
	}
	return n
}

// bdMetadata renders a link's stored metadata as bd prints it: its JSON text,
// and "{}" when the link carries none.
func bdMetadata(metadata string) string {
	if metadata == "" {
		return "{}"
	}
	return metadata
}

// printBDAnswer writes one bd-shaped answer: indented JSON, then the newline
// bd ends its answer with.
func printBDAnswer(w io.Writer, answer any) error {
	b, err := json.MarshalIndent(answer, "", "  ")
	if err != nil {
		return fmt.Errorf("rendering bd's answer: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("writing bd's answer: %w", err)
	}
	return nil
}
