package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

// wispQuery keys a filter by the fields a wisp read sets, so a stub answer is
// served only to the exact question it was written for; an instant is keyed by
// value, not by its rendering.
func wispQuery(f api.ListFilter) string {
	return fmt.Sprintf("ephemeral=%v statuses=%v closed_before=%d created_before=%d",
		f.Ephemeral, f.Statuses, f.ClosedBefore.UnixNano(), f.CreatedBefore.UnixNano())
}

// wispStore answers the list and count routes from rows keyed by wispQuery: a
// filter it holds no answer for is api.ErrInvalid, so a count asked a
// different question than the list it stands beside fails here rather than
// agreeing with itself. Which rows a filter matches is the store's question,
// and the contract suite proves it against Postgres. A page is the row offset
// its cursor names, at most f.Limit rows.
func wispStore(answers map[string][]api.Issue) store.Store {
	rows := func(f api.ListFilter) ([]api.Issue, error) {
		got, ok := answers[wispQuery(f)]
		if !ok {
			return nil, api.Invalid("the test holds no answer for filter %+v", f)
		}
		return got, nil
	}
	return issueStub{
		list: func(_ context.Context, f api.ListFilter) (api.IssuePage, error) {
			all, err := rows(f)
			if err != nil {
				return api.IssuePage{}, err
			}
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
			all, err := rows(f)
			if err != nil {
				return 0, err
			}
			return len(all), nil
		},
	}
}

// TestClientCountAgreesWithTheListItCounts pins the pair the wisp reaper sizes
// its work with: asked one filter, Count answers for every row List walks. The
// stub holds a separate answer per filter, so a count sent a filter the list
// was not — a dropped ephemeral flag, status or cutoff — is a different
// question, and the numbers below stop lining up.
func TestClientCountAgreesWithTheListItCounts(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	closedAt := func(d time.Duration) *time.Time {
		at := now.Add(-d)
		return &at
	}
	old := api.Issue{ID: "gt-wisp-old", Title: "old wisp", Status: "closed", Priority: 2, Ephemeral: true,
		CreatedAt: now.Add(-20 * time.Hour), ClosedAt: closedAt(10 * time.Hour)}
	older := api.Issue{ID: "gt-wisp-older", Title: "older wisp", Status: "closed", Priority: 3, Ephemeral: true,
		CreatedAt: now.Add(-40 * time.Hour), ClosedAt: closedAt(30 * time.Hour)}
	durable := api.Issue{ID: "gt-durable", Title: "durable issue", Status: "closed", Priority: 2,
		CreatedAt: now.Add(-5 * time.Hour), ClosedAt: closedAt(2 * time.Hour)}

	f := api.ListFilter{Ephemeral: true, Statuses: []string{"closed"}, ClosedBefore: now.Add(-time.Hour)}
	byCreation := f
	byCreation.CreatedBefore = now.Add(-25 * time.Hour)
	durableOnly := f
	durableOnly.Ephemeral = false

	ts := stubServer(t, wispStore(map[string][]api.Issue{
		wispQuery(f):           {old, older},
		wispQuery(byCreation):  {older},
		wispQuery(durableOnly): {durable},
	}))
	c := agreeClient(t, ts.URL)

	// Two rows over two one-row pages: the count of the unpaged read is the
	// length of the walk, and the durable issue at that status and age is in
	// neither.
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

	// The creation bound is part of the question too: the same read narrowed
	// by created_before walks one row, and its count says one.
	byCreationWalked := byCreation
	byCreationWalked.Limit = 1
	got, err = c.ListAll(t.Context(), byCreationWalked)
	if err != nil || len(got) != 1 || got[0].ID != "gt-wisp-older" {
		t.Fatalf("ListAll narrowed by created_before = %v, %v; want only gt-wisp-older", got, err)
	}
	if n, err := c.Count(t.Context(), byCreation); err != nil || n != len(got) {
		t.Errorf("Count narrowed by created_before = %d, %v; want %d, the length of the list it counts", n, err, len(got))
	}

	// The same read without the ephemeral flag asks about the durable issue at
	// that status and age instead: one row, not the two wisps.
	if n, err := c.Count(t.Context(), durableOnly); err != nil || n != 1 {
		t.Errorf("Count of the same read without the ephemeral flag = %d, %v; want 1, the one durable issue at that status and age", n, err)
	}
}
