package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// ReadyzStatus is the service's answer to GET /readyz: it reports ready only
// when the database answers a ping and accepts a write, and carries the
// server's schema level.
type ReadyzStatus struct {
	Status      string `json:"status"`
	SchemaLevel int    `json:"schema_level"`
}

// NotReady is the service's own answer that it is not ready: it is up, and its
// 503 said why. It reports as api.ErrUnavailable, since a caller that treats an
// unavailable tracker as retryable is still right; what it adds is that there
// *was* an answer, so a status screen can show it instead of reporting an
// unreachable server.
type NotReady struct {
	// Reason is the service's own text, cleaned by the client.
	Reason string
}

func (e *NotReady) Error() string { return string(api.KindUnavailable) + ": " + e.Reason }

// Unwrap makes errors.Is(err, api.ErrUnavailable) hold for a not-ready answer,
// as it did before the answer had a type of its own.
func (e *NotReady) Unwrap() error { return api.ErrUnavailable }

// Readyz asks the service whether it is ready. It is one attempt bounded by
// the client's timeout, never a retry: readiness is a point in time, so a
// second answer says nothing the first one did not.
//
// An unready service answers 503 with the reason, which arrives as a *NotReady
// error; a service that cannot be reached arrives as an api.KindUnavailable
// error of some other type. Callers that only care that the tracker is not
// ready read both as api.ErrUnavailable. Neither is retried here.
//
// Readyz judges the answer itself rather than going through once, because only
// here does the difference between "answered 503" and "did not answer" carry
// meaning.
func (c *Client) Readyz(ctx context.Context) (ReadyzStatus, error) {
	status, data, err := c.exchange(ctx, http.MethodGet, "/readyz", nil)
	if err != nil {
		return ReadyzStatus{}, err
	}
	if status/100 == 2 {
		var out ReadyzStatus
		if err := json.Unmarshal(data, &out); err != nil {
			return ReadyzStatus{}, fmt.Errorf("client: tracker answered %d with a body that is not the expected JSON", status)
		}
		return out, nil
	}
	answer := c.decodeError(status, data)
	if status != http.StatusServiceUnavailable {
		return ReadyzStatus{}, answer
	}
	reason := answer.Error()
	var e *api.Error
	if errors.As(answer, &e) {
		reason = e.Message
	}
	return ReadyzStatus{}, &NotReady{Reason: reason}
}
