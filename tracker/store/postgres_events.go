package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/steveyegge/gastown/tracker/api"
)

const (
	defaultEventsPage = 1000
	maxEventsPage     = 5000
)

// EventsAfter returns up to limit events with seq greater than after, oldest
// first (limit 0 means a default page; the server caps it). NextAfter is the
// seq to ask for next: the last seq returned, or after when nothing is newer.
// FirstSeq is the oldest seq still stored (0 when there are none), so a
// reader that asked from before it knows the feed was pruned past it.
func (p *Postgres) EventsAfter(ctx context.Context, after int64, limit int) (api.EventsPage, error) {
	switch {
	case limit <= 0:
		limit = defaultEventsPage
	case limit > maxEventsPage:
		limit = maxEventsPage
	}
	rows, err := p.pool.Query(ctx, `SELECT seq, ts, op, issue_id, actor, old_value, new_value, comment, payload::text
FROM tracker.events WHERE seq > $1 ORDER BY seq LIMIT $2`, after, limit)
	if err != nil {
		return api.EventsPage{}, fmt.Errorf("store: events: %w", err)
	}
	defer rows.Close()
	page := api.EventsPage{Events: []api.Event{}, NextAfter: after}
	for rows.Next() {
		var e api.Event
		var payload *string
		if err := rows.Scan(&e.Seq, &e.TS, &e.Op, &e.IssueID, &e.Actor, &e.OldValue, &e.NewValue, &e.Comment, &payload); err != nil {
			return api.EventsPage{}, fmt.Errorf("store: events: %w", err)
		}
		if payload != nil {
			e.Payload = json.RawMessage(*payload)
		}
		page.Events = append(page.Events, e)
		page.NextAfter = e.Seq
	}
	if err := rows.Err(); err != nil {
		return api.EventsPage{}, fmt.Errorf("store: events: %w", err)
	}
	if err := p.pool.QueryRow(ctx, "SELECT coalesce(min(seq), 0) FROM tracker.events").Scan(&page.FirstSeq); err != nil {
		return api.EventsPage{}, fmt.Errorf("store: events: %w", err)
	}
	return page, nil
}
