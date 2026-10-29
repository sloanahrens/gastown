package client

import (
	"context"
	"net/http"
)

// Metrics returns the service's /metrics page as the Prometheus text it is,
// unparsed. It is one attempt bounded by the client's timeout, like Readyz: a
// point-in-time read is not retried. A server without the route answers 404,
// which arrives as an api.Invalid error.
func (c *Client) Metrics(ctx context.Context) (string, error) {
	status, data, err := c.exchange(ctx, http.MethodGet, "/metrics", nil)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", c.decodeError(status, data)
	}
	return string(data), nil
}
