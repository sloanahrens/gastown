package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
	"github.com/steveyegge/gastown/tracker/store"
)

func agreeClient(t *testing.T, url string) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{BaseURL: url, Token: "good-token", SchemaLevel: 3})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Unit tier: the real client against the real routes with a stub store.
func TestClientReadsAgreeWithTheRoutes(t *testing.T) {
	t.Parallel()
	all := map[string]api.Issue{"gt-a": {ID: "gt-a", Title: "a"}, "gt-b": {ID: "gt-b", Title: "b"}}
	ts := stubServer(t, issueStub{
		get: func(_ context.Context, ids []string) (map[string]api.Issue, error) {
			out := map[string]api.Issue{}
			for _, id := range ids {
				if is, ok := all[id]; ok {
					out[id] = is
				}
			}
			return out, nil
		},
		list: func(_ context.Context, f api.ListFilter) (api.IssuePage, error) {
			// Two pages: cursor "" gives gt-a, cursor "p2" gives gt-b.
			switch f.Cursor {
			case "":
				return api.IssuePage{Issues: []api.Issue{all["gt-a"]}, NextCursor: "p2"}, nil
			case "p2":
				return api.IssuePage{Issues: []api.Issue{all["gt-b"]}}, nil
			}
			return api.IssuePage{}, api.Invalid("bad cursor %q", f.Cursor)
		},
	})
	c := agreeClient(t, ts.URL)
	ctx := context.Background()
	if is, err := c.Show(ctx, "gt-a"); err != nil || is.Title != "a" {
		t.Errorf("Show = %+v, %v", is, err)
	}
	if _, err := c.Show(ctx, "gt-zzz"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Show missing = %v, want api.ErrNotFound", err)
	}
	m, err := c.ShowMany(ctx, []string{"gt-a", "gt-zzz", "gt-b"})
	if err != nil || len(m) != 2 || m["gt-b"].Title != "b" {
		t.Errorf("ShowMany = %v, %v; want the two that exist", m, err)
	}
	if m, err := c.ShowMany(ctx, []string{"gt-zzz"}); err != nil || m == nil || len(m) != 0 {
		t.Errorf("ShowMany none found = %v, %v; want an empty non-nil map", m, err)
	}
	got, err := c.ListAll(ctx, api.ListFilter{})
	if err != nil || len(got) != 2 || fmt.Sprintf("%s %s", got[0].ID, got[1].ID) != "gt-a gt-b" {
		t.Errorf("ListAll = %v, %v; want both pages in order", got, err)
	}
	if _, err := c.List(ctx, api.ListFilter{Cursor: "junk"}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("List bad cursor = %v, want api.ErrInvalid", err)
	}
}

// wispFixture is the set of rows both reads in the agreement test serve,
// chosen so every field the filter sets excludes at least one of them: an
// open wisp, a closed wisp inside the age cutoff, two closed wisps past it,
// and a durable issue at the same status and age that no wisp read may return.
func wispFixture(now time.Time) []api.Issue {
	closedAt := func(d time.Duration) *time.Time {
		at := now.Add(-d)
		return &at
	}
	return []api.Issue{
		{ID: "gt-wisp-open", Title: "open wisp", Status: "open", Priority: 1, Ephemeral: true, CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "gt-wisp-fresh", Title: "freshly closed wisp", Status: "closed", Priority: 1, Ephemeral: true, CreatedAt: now.Add(-4 * time.Hour), ClosedAt: closedAt(5 * time.Minute)},
		{ID: "gt-wisp-old", Title: "old wisp", Status: "closed", Priority: 2, Ephemeral: true, CreatedAt: now.Add(-20 * time.Hour), ClosedAt: closedAt(10 * time.Hour)},
		{ID: "gt-wisp-older", Title: "older wisp", Status: "closed", Priority: 3, Ephemeral: true, CreatedAt: now.Add(-40 * time.Hour), ClosedAt: closedAt(30 * time.Hour)},
		{ID: "gt-durable", Title: "durable issue", Status: "closed", Priority: 2, CreatedAt: now.Add(-5 * time.Hour), ClosedAt: closedAt(2 * time.Hour)},
	}
}

// wispMatch applies the filter to a row the way the store does over these
// rows: the ephemeral flag exactly, statuses ORed (none means not closed,
// "all" means every status), and each upper age bound strictly before a time
// the row must have.
func wispMatch(f api.ListFilter, is api.Issue) bool {
	if is.Ephemeral != f.Ephemeral {
		return false
	}
	switch {
	case len(f.Statuses) == 0:
		if is.Status == "closed" {
			return false
		}
	case !slices.Contains(f.Statuses, "all") && !slices.Contains(f.Statuses, is.Status):
		return false
	}
	if !f.ClosedBefore.IsZero() && (is.ClosedAt == nil || !is.ClosedAt.Before(f.ClosedBefore)) {
		return false
	}
	if !f.CreatedBefore.IsZero() && !is.CreatedAt.Before(f.CreatedBefore) {
		return false
	}
	return true
}

// wispStore serves the list and count routes from wispFixture through
// wispMatch, so the two only answer alike when the client asks them the same
// question. A page is the row offset the cursor names, at most f.Limit rows.
func wispStore(rows []api.Issue) store.Store {
	matched := func(f api.ListFilter) []api.Issue {
		var out []api.Issue
		for _, is := range rows {
			if wispMatch(f, is) {
				out = append(out, is)
			}
		}
		slices.SortFunc(out, func(a, b api.Issue) int {
			return cmp.Or(cmp.Compare(a.Priority, b.Priority), a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
		})
		return out
	}
	return issueStub{
		list: func(_ context.Context, f api.ListFilter) (api.IssuePage, error) {
			all := matched(f)
			start := 0
			if f.Cursor != "" {
				n, err := strconv.Atoi(f.Cursor)
				if err != nil || n > len(all) {
					return api.IssuePage{}, api.Invalid("bad cursor %q", f.Cursor)
				}
				start = n
			}
			end := len(all)
			if f.Limit > 0 && start+f.Limit < end {
				end = start + f.Limit
			}
			page := api.IssuePage{Issues: all[start:end]}
			if end < len(all) {
				page.NextCursor = strconv.Itoa(end)
			}
			return page, nil
		},
		count: func(_ context.Context, f api.ListFilter) (int, error) {
			if err := api.ValidateCount(f); err != nil {
				return 0, err
			}
			return len(matched(f)), nil
		},
	}
}

// TestClientCountAgreesWithTheListItCounts pins the pair the wisp reaper sizes
// its work with: over one store, Count answers for every row List walks. The
// fixture separates on each field the filter sets, so a count asked a
// different question than the list — a dropped ephemeral flag, status or
// cutoff — is a number the walking list does not reach.
func TestClientCountAgreesWithTheListItCounts(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	ts := stubServer(t, wispStore(wispFixture(now)))
	c := agreeClient(t, ts.URL)

	f := api.ListFilter{Ephemeral: true, Statuses: []string{"closed"}, ClosedBefore: now.Add(-time.Hour)}
	walked := f
	walked.Limit = 1
	got, err := c.ListAll(t.Context(), walked)
	if err != nil || len(got) != 2 || got[0].ID != "gt-wisp-old" || got[1].ID != "gt-wisp-older" {
		t.Fatalf("ListAll = %v, %v; want gt-wisp-old then gt-wisp-older: two pages, the closed wisps past the cutoff", got, err)
	}
	n, err := c.Count(t.Context(), f)
	if err != nil || n != len(got) {
		t.Errorf("Count = %d, %v; want %d, the length of the list it counts", n, err, len(got))
	}

	// The fixture separates on the ephemeral flag: the same read with it
	// dropped counts the one durable issue at that status and age instead of
	// the two wisps. A count that dropped the flag would answer 1 against a
	// list of 2, which is the disagreement this test is for.
	durableOnly := f
	durableOnly.Ephemeral = false
	if n, err := c.Count(t.Context(), durableOnly); err != nil || n != 1 {
		t.Errorf("Count of the same read without the ephemeral flag = %d, %v; want 1, so the flag is what the agreement above turns on", n, err)
	}
}
