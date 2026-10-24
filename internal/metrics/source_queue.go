package metrics

import "context"

// queueSource is a stub until the queue source lands: it reports no columns.
type queueSource struct{}

func (queueSource) Group() string { return "queue" }

func (queueSource) Collect(context.Context, Env) (map[string]any, error) {
	return map[string]any{}, nil
}
