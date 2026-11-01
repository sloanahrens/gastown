//go:build integration

package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// TestGetIssuesCarriesTheLinkRecord drives the Postgres read the bd shim's
// list rows are printed from: each link in Dependencies carries the relation's
// own row (who wrote it, when, and the JSON stored with it), not the target's
// fields. GetIssues is where gt reads those links, so the columns it selects
// are asserted here against the real store.
func TestGetIssuesCarriesTheLinkRecord(t *testing.T) {
	ctx := api.WithActor(context.Background(), "town/crew/fixture")
	pool := pgtest.New(t, "migrated")
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown')"); err != nil {
		t.Fatal(err)
	}
	p := NewPostgres(pool)
	owner := createLinkRecordIssue(t, p, ctx, "owner")
	blocker := createLinkRecordIssue(t, p, ctx, "blocker")
	tracker := createLinkRecordIssue(t, p, ctx, "tracker")
	if err := p.AddDependency(ctx, owner.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	// A link written with metadata: AddDependency takes none, so the row is
	// written directly. The read must return it as stored, not as empty.
	const metadata = `{"note": "written by hand"}`
	const writer = "town/crew/handwritten"
	if _, err := pool.Exec(ctx, `INSERT INTO tracker.dependencies (issue_id, depends_on, type, metadata, created_by)
VALUES ($1, $2, 'tracks', $3::jsonb, $4)`, owner.ID, tracker.ID, metadata, writer); err != nil {
		t.Fatal(err)
	}

	got, err := p.GetIssues(ctx, []string{owner.ID})
	if err != nil {
		t.Fatalf("GetIssues: %v", err)
	}
	links := map[string]api.IssueDep{}
	for _, dep := range got[owner.ID].Dependencies {
		links[dep.ID] = dep
	}
	if len(links) != 2 {
		t.Fatalf("Dependencies = %+v, want both links", got[owner.ID].Dependencies)
	}

	blocked := links[blocker.ID]
	if blocked.CreatedBy != "town/crew/fixture" {
		t.Errorf("link to %s created_by = %q, want the actor that wrote it", blocker.ID, blocked.CreatedBy)
	}
	var written time.Time
	if err := pool.QueryRow(ctx, "SELECT created_at FROM tracker.dependencies WHERE issue_id = $1 AND depends_on = $2",
		owner.ID, blocker.ID).Scan(&written); err != nil {
		t.Fatal(err)
	}
	if !blocked.CreatedAt.Equal(written) {
		t.Errorf("link to %s created_at = %v, want the relation's own %v", blocker.ID, blocked.CreatedAt, written)
	}
	if len(blocked.Metadata) != 0 {
		t.Errorf("link to %s metadata = %s, want none for a link written without one", blocker.ID, blocked.Metadata)
	}

	handwritten := links[tracker.ID]
	if handwritten.CreatedBy != writer {
		t.Errorf("link to %s created_by = %q, want %q", tracker.ID, handwritten.CreatedBy, writer)
	}
	var read map[string]string
	if err := json.Unmarshal(handwritten.Metadata, &read); err != nil {
		t.Fatalf("link to %s metadata = %q, want the stored JSON: %v", tracker.ID, handwritten.Metadata, err)
	}
	if read["note"] != "written by hand" {
		t.Errorf("link to %s metadata = %v, want the note it was written with", tracker.ID, read)
	}
}

// createLinkRecordIssue creates one issue on the Postgres store.
func createLinkRecordIssue(t *testing.T, p *Postgres, ctx context.Context, title string) api.Issue {
	t.Helper()
	is, err := p.CreateIssue(ctx, api.CreateParams{Rig: "gt", Title: title})
	if err != nil {
		t.Fatalf("CreateIssue(%q): %v", title, err)
	}
	return is
}
