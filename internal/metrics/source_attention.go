package metrics

import "context"

// attentionSource is a stub until the attention source lands: it reports no columns.
type attentionSource struct{}

func (attentionSource) Group() string { return "attention" }

func (attentionSource) Collect(context.Context, Env) (map[string]any, error) {
	return map[string]any{}, nil
}
