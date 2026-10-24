package metrics

import "context"

// configSource is a stub until the config source lands: it reports no columns.
type configSource struct{}

func (configSource) Group() string { return "config" }

func (configSource) Collect(context.Context, Env) (map[string]any, error) {
	return map[string]any{}, nil
}
