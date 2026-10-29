package client

import (
	"context"
	"net/http"
)

// ReadyzStatus is the service's answer to GET /readyz: it reports ready only
// when the database answers a ping and accepts a write, and carries the
// server's schema level.
type ReadyzStatus struct {
	Status      string `json:"status"`
	SchemaLevel int    `json:"schema_level"`
}

// Readyz asks the service whether it is ready. It is one attempt bounded by
// the client's timeout, never a retry: readiness is a point in time, so a
// second answer says nothing the first one did not.
//
// An unready service answers 503 with the reason, which arrives as an
// api.ErrUnavailable error; a service that cannot be reached arrives as the
// same kind. Neither is retried here.
func (c *Client) Readyz(ctx context.Context) (ReadyzStatus, error) {
	var out ReadyzStatus
	if err := c.once(ctx, http.MethodGet, "/readyz", nil, &out); err != nil {
		return ReadyzStatus{}, err
	}
	return out, nil
}
