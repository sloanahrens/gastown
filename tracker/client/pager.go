package client

import (
	"context"
	"fmt"
)

// All collects every page of a cursor-paged list. fetch is called with the
// empty cursor first and then with each NextCursor until one comes back empty.
// A cursor the server repeats is an error, not an endless loop.
func All[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) (items []T, next string, err error)) ([]T, error) {
	var out []T
	seen := map[string]bool{}
	cursor := ""
	for {
		items, next, err := fetch(ctx, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		if seen[next] || next == cursor {
			return nil, fmt.Errorf("client: the server repeated the page cursor %q", next)
		}
		seen[next] = true
		cursor = next
	}
}
