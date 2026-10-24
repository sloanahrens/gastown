package metrics

import "context"

// moneySource is a stub until the money source lands: it reports no columns.
type moneySource struct{}

func (moneySource) Group() string { return "money" }

func (moneySource) Collect(context.Context, Env) (map[string]any, error) {
	return map[string]any{}, nil
}
