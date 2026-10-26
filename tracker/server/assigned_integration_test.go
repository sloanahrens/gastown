//go:build integration

package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

func assignedIDs(page api.AssignedPage, assignee string) []string {
	out := make([]string, len(page.Assigned[assignee]))
	for i, is := range page.Assigned[assignee] {
		out[i] = is.ID
	}
	return out
}

func statusPtr(s string) *string { return &s }

// postAssigned sends body as a raw request, so the HTTP status itself is
// asserted and not only the client's mapping of the error kind.
func postAssigned(t *testing.T, base, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/assigned", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(api.SchemaHeader, strconv.Itoa(store.SchemaLevel()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

// Integration tier: the real server over a migrated Postgres clone, driven
// through the real client, ~250 ms.
func TestAssignedAcrossTheWire(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := pgtest.New(t, "migrated")
	if _, err := pool.Exec(ctx, "INSERT INTO tracker.rigs (prefix, name) VALUES ('gt', 'gastown'), ('hq', 'town')"); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool)
	srv := httptest.NewServer(New(Config{Store: pg, Auth: pg, Health: pg, SchemaLevel: store.SchemaLevel()}).Handler())
	t.Cleanup(srv.Close)
	tok, err := pg.IssueToken(ctx, "town/crew/dipper", nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(client.Config{BaseURL: srv.URL, Token: tok, SchemaLevel: store.SchemaLevel()})
	if err != nil {
		t.Fatal(err)
	}

	create := func(p api.CreateParams) api.Issue {
		t.Helper()
		is, err := c.Create(ctx, p)
		if err != nil {
			t.Fatalf("create %q: %v", p.Title, err)
		}
		return is
	}
	open := create(api.CreateParams{Rig: "gt", Title: "a open", Assignee: "seat-a", Priority: 1})
	hook := create(api.CreateParams{Rig: "gt", Title: "a hooked", Assignee: "seat-a", Priority: 2})
	if err := c.Update(ctx, hook.ID, api.UpdatePatch{Status: statusPtr("hooked")}, api.Guard{}); err != nil {
		t.Fatal(err)
	}
	closed := create(api.CreateParams{Rig: "gt", Title: "a closed", Assignee: "seat-a"})
	// force: closing a seat's issue is normally refused without it.
	if err := c.Close(ctx, []string{closed.ID}, "r", true); err != nil {
		t.Fatal(err)
	}
	hqOpen := create(api.CreateParams{Rig: "hq", Title: "hq open", Assignee: "seat-a", Priority: 2})
	wisp := create(api.CreateParams{Rig: "gt", Title: "a wisp", Assignee: "seat-a", Ephemeral: true, Priority: 3})
	if err := c.Update(ctx, wisp.ID, api.UpdatePatch{Status: statusPtr("hooked")}, api.Guard{}); err != nil {
		t.Fatal(err)
	}
	create(api.CreateParams{Rig: "gt", Title: "b open", Assignee: "seat-b"})

	page, err := c.Assigned(ctx, api.AssignedRequest{Assignees: []string{"seat-a"}, Statuses: []string{"hooked", "open"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := assignedIDs(page, "seat-a"), []string{open.ID, hook.ID, hqOpen.ID}; !slices.Equal(got, want) {
		t.Errorf("seat-a = %v, want %v in list order (priority, then creation)", got, want)
	}
	if page.Count != 3 || page.NextCursor != "" {
		t.Errorf("count/cursor = %d/%q, want 3 and none", page.Count, page.NextCursor)
	}
	if rows := page.Assigned["seat-a"]; len(rows) == 3 && rows[0].Title != "a open" {
		t.Errorf("row title = %q, want the issue to cross the wire whole", rows[0].Title)
	}
	if _, ok := page.Assigned["seat-b"]; ok {
		t.Errorf("the answer holds seat-b, which was not asked for")
	}

	wisps, err := c.Assigned(ctx, api.AssignedRequest{Assignees: []string{"seat-a"}, Statuses: []string{"hooked"}, IncludeWisps: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(assignedIDs(wisps, "seat-a"), []string{hook.ID, wisp.ID}) {
		t.Errorf("with include_wisps seat-a hooked = %v, want the issue and the wisp", assignedIDs(wisps, "seat-a"))
	}

	// The refusal names the field, so a caller that sent an empty list is not
	// left reading an empty answer as "the seat holds nothing".
	_, err = c.Assigned(ctx, api.AssignedRequest{Statuses: []string{"open"}})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != api.KindInvalid || !strings.Contains(apiErr.Message, "assignees") {
		t.Errorf("Assigned with no assignee = %v, want an invalid error naming assignees", err)
	}
	code, body := postAssigned(t, srv.URL, tok, `{"statuses":["open"]}`)
	if code != 400 || !strings.Contains(body, "assignees") {
		t.Errorf("POST /v1/assigned with no assignee = %d %q, want 400 naming the field", code, body)
	}

	// Paging: a page smaller than the answer resumes without a repeat or a gap.
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		p, err := c.Assigned(ctx, api.AssignedRequest{Assignees: []string{"seat-a"}, Statuses: []string{"open", "hooked", "closed"}, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		got = append(got, assignedIDs(p, "seat-a")...)
		if p.NextCursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("paging does not end")
		}
		cursor = p.NextCursor
	}
	want := []string{closed.ID, open.ID, hook.ID, hqOpen.ID} // priority 0, then 1, then the two at 2 by creation
	if !slices.Equal(got, want) {
		t.Errorf("paged %v, want %v", got, want)
	}
}
