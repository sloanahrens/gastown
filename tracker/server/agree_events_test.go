package server

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// Unit tier: the real client against the real route with a stub store.
func TestClientEventsAgreeWithTheRoute(t *testing.T) {
	t.Parallel()
	const total = 5
	ts := eventsServer(t, eventsStub{after: func(_ context.Context, after int64, limit int) (api.EventsPage, error) {
		if limit == 0 {
			limit = 2 // a small server page size forces several pages
		}
		page := api.EventsPage{NextAfter: after, FirstSeq: 3}
		for seq := after + 1; seq <= total && len(page.Events) < limit; seq++ {
			page.Events = append(page.Events, api.Event{Seq: seq, Op: "created", IssueID: "gt-a"})
			page.NextAfter = seq
		}
		return page, nil
	}})
	c, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page, err := c.Events(ctx, 1, 0)
	if err != nil || len(page.Events) != 2 || page.Events[0].Seq != 2 || page.NextAfter != 3 || page.FirstSeq != 3 {
		t.Errorf("Events = %+v, %v; want seq 2 and 3, next_after 3, first_seq carried unchanged", page, err)
	}
	all, err := c.EventsAll(ctx, 0)
	if err != nil || len(all) != total || all[total-1].Seq != total {
		t.Errorf("EventsAll = %d events, %v; want all %d across three pages", len(all), err, total)
	}
	if empty, err := c.Events(ctx, 40, 0); err != nil || empty.Events == nil || len(empty.Events) != 0 || empty.NextAfter != 40 {
		t.Errorf("Events past the end = %+v, %v; want an empty non-nil slice and next_after 40", empty, err)
	}
	if _, err := c.Events(ctx, -1, 0); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("Events with a negative after = %v, want api.ErrInvalid", err)
	}
}
