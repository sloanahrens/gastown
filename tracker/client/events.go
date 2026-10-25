package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/steveyegge/gastown/tracker/api"
)

// Events returns one page of the events feed: up to limit events with a seq
// greater than after, oldest first (limit 0 is the server's page size). The
// page's NextAfter is the after to ask for next; FirstSeq is the oldest seq
// the server still holds, so a caller that asked from before it knows the feed
// was pruned past it. Events is never nil.
func (c *Client) Events(ctx context.Context, after int64, limit int) (api.EventsPage, error) {
	path := "/v1/events?after=" + strconv.FormatInt(after, 10)
	if limit > 0 {
		path += "&limit=" + strconv.Itoa(limit)
	}
	var out api.EventsPage
	if err := c.do(ctx, http.MethodGet, path, nil, &out, true); err != nil {
		return api.EventsPage{}, err
	}
	if out.Events == nil {
		out.Events = []api.Event{}
	}
	return out, nil
}

// EventsAll returns every event after the given seq, following NextAfter until
// a page comes back empty. A server whose NextAfter does not move past the
// seq it was asked from, while still returning events, is an error: following
// it would never end.
func (c *Client) EventsAll(ctx context.Context, after int64) ([]api.Event, error) {
	var all []api.Event
	for {
		page, err := c.Events(ctx, after, 0)
		if err != nil {
			return nil, err
		}
		if len(page.Events) == 0 {
			return all, nil
		}
		all = append(all, page.Events...)
		if page.NextAfter <= after {
			return nil, fmt.Errorf("client: the server returned events but next_after stayed at %d", after)
		}
		after = page.NextAfter
	}
}
