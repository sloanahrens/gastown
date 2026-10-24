package metrics

import "context"

// reviewSource is a stub until the review source lands: it reports no columns.
type reviewSource struct{}

func (reviewSource) Group() string { return "review" }

func (reviewSource) Collect(context.Context, Env) (map[string]any, error) {
	return map[string]any{}, nil
}
