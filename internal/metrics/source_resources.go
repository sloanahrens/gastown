package metrics

import "context"

// resourcesSource is a stub until the resources source lands: it reports no columns.
type resourcesSource struct{}

func (resourcesSource) Group() string { return "resources" }

func (resourcesSource) Collect(context.Context, Env) (map[string]any, error) {
	return map[string]any{}, nil
}
