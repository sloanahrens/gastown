package metrics

import "context"

// hostSource is a stub until the host source lands: it reports no columns.
type hostSource struct{}

func (hostSource) Group() string { return "host" }

func (hostSource) Collect(context.Context, Env) (map[string]any, error) {
	return map[string]any{}, nil
}
