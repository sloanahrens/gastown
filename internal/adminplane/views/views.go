// Package views is the operator console's read views of a hosted workspace
// (plan row V2): the list of workspaces and one workspace's detail. Each view
// is built by copying the fields it may show out of a Source's records, so a
// view has no field an operator could not see filled.
//
// Nothing calls it yet. Its callers are the console handlers and the view log
// (plan rows V3 and V4), which supply a Source over the admin plane's records.
// A customer-shared item (plan row V3) is not one of these views and is never
// shown by them. The package holds no HTTP and no database, so both views are
// testable without either.
package views

import (
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
)

// WorkspaceRow is one workspace as the list view shows it: its identity, its
// state, and the counts of what it holds.
type WorkspaceRow struct {
	ID           string         `json:"id"`
	Organization string         `json:"organization"`
	State        registry.State `json:"state"`
	Owner        string         `json:"owner"`
	Created      time.Time      `json:"created"`
	Counts       Counts         `json:"counts"`
}

// Counts are the tallies a view shows for a workspace.
type Counts struct {
	Beads   int `json:"beads"`
	Members int `json:"members"`
}

// BeadRef is a bead as a view names it: its id and title, never its body.
type BeadRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// MeteringSummary is a workspace's monthly metering figures, shaped as the M8
// export computes them: seat hours by profile, storage GB-months, and unpriced
// token counts.
type MeteringSummary struct {
	SeatHours       []ProfileHours `json:"seat_hours"`
	StorageGBMonths float64        `json:"storage_gb_months"`
	Tokens          Tokens         `json:"tokens"`
}

// ProfileHours is one profile's seat hours for the month. It is the view's own
// type, not the export's, so a field the export gains cannot reach a view.
type ProfileHours struct {
	Profile string
	Hours   float64
}

// Tokens is a workspace's token counts for the month. It is the view's own
// type, for the same reason as ProfileHours.
type Tokens struct {
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
}

// WorkspaceDetail is one workspace as the detail view shows it. No field it
// holds can carry a tool's code, an API key, a transcript or a bead body, so
// the filter is structural: a caller cannot widen it by forgetting a check.
type WorkspaceDetail struct {
	WorkspaceRow
	Health   string          `json:"health"`
	Beads    []BeadRef       `json:"beads"`
	Metering MeteringSummary `json:"metering"`
}

// Bead is a bead as a Source holds it: the id and title a view shows, and the
// body it never shows.
type Bead struct {
	ID    string
	Title string
	Body  string
}

// Record is one workspace as a Source holds it. It carries every field a view
// may show and whatever else the admin plane's records hold, so it may also
// hold a tool's code, an API key and a transcript that no view shows; a bead's
// body is held with the bead, in Bead. Detail and List copy only the shown
// fields and drop the rest.
type Record struct {
	ID           string
	Organization string
	Owner        string
	State        registry.State
	Created      time.Time
	Health       string
	Counts       Counts
	Beads        []Bead
	Metering     MeteringSummary

	// Code, APIKey and Transcript are the kinds of field a view refuses. They
	// are here so a record source and its fake carry them on purpose and a
	// test can prove none of them reaches a view.
	Code       string
	APIKey     string
	Transcript string
}

// Source is what the views read: the admin plane's records, as the console
// handler's adapter supplies them. A Source's error means the view cannot be
// built, so no error may read as an empty record.
type Source interface {
	// Workspaces returns every workspace's record.
	Workspaces() ([]Record, error)
	// Workspace returns the record with the given id.
	Workspace(id string) (Record, error)
}

var (
	// ErrNoSource is a view's refusal of a nil Source, which has no record to
	// read.
	ErrNoSource = errors.New("views: a source is required")
	// ErrEmptyID is Detail's refusal of an empty id, which names no workspace.
	ErrEmptyID = errors.New("views: a workspace id is required")
	// ErrWrongWorkspace is Detail's refusal of a record the Source returned
	// under another id, which would show one workspace's data as another's.
	ErrWrongWorkspace = errors.New("views: source returned a different workspace")
)

// Detail returns the detail view of the workspace named by id.
func Detail(src Source, id string) (WorkspaceDetail, error) {
	if src == nil {
		return WorkspaceDetail{}, ErrNoSource
	}
	if id == "" {
		return WorkspaceDetail{}, ErrEmptyID
	}
	rec, err := src.Workspace(id)
	if err != nil {
		return WorkspaceDetail{}, fmt.Errorf("views: detail of %q: %w", id, err)
	}
	if rec.ID != id {
		return WorkspaceDetail{}, fmt.Errorf("%w: asked for %q, got %q", ErrWrongWorkspace, id, rec.ID)
	}
	return WorkspaceDetail{
		WorkspaceRow: row(rec),
		Health:       rec.Health,
		Beads:        beadRefs(rec.Beads),
		Metering:     metering(rec.Metering),
	}, nil
}

// List returns every workspace as a list row, in the order the Source gives
// them.
func List(src Source) ([]WorkspaceRow, error) {
	if src == nil {
		return nil, ErrNoSource
	}
	recs, err := src.Workspaces()
	if err != nil {
		return nil, fmt.Errorf("views: workspace list: %w", err)
	}
	rows := make([]WorkspaceRow, 0, len(recs))
	for _, rec := range recs {
		rows = append(rows, row(rec))
	}
	return rows, nil
}

// row copies the fields a row shows, dropping everything else the record holds.
func row(rec Record) WorkspaceRow {
	return WorkspaceRow{
		ID:           rec.ID,
		Organization: rec.Organization,
		State:        rec.State,
		Owner:        rec.Owner,
		Created:      rec.Created,
		Counts:       rec.Counts,
	}
}

// beadRefs copies each bead's id and title, dropping its body.
func beadRefs(beads []Bead) []BeadRef {
	if beads == nil {
		return nil
	}
	refs := make([]BeadRef, 0, len(beads))
	for _, b := range beads {
		refs = append(refs, BeadRef{ID: b.ID, Title: b.Title})
	}
	return refs
}

// metering copies a summary and its seat-hours slice, so a later change to the
// source's record cannot change a view already returned.
func metering(m MeteringSummary) MeteringSummary {
	out := MeteringSummary{StorageGBMonths: m.StorageGBMonths, Tokens: m.Tokens}
	out.SeatHours = append([]ProfileHours(nil), m.SeatHours...)
	return out
}
