package metrics

import "context"

// placementSource is a stub until the placement source lands: it reports no columns.
type placementSource struct{}

func (placementSource) Group() string { return "placement" }

func (placementSource) Collect(context.Context, Env) (map[string]any, error) {
	return map[string]any{}, nil
}
