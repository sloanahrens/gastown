package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// maxBodyBytes bounds a request body: a larger one is refused, not read.
const maxBodyBytes = 1 << 20

// decodeJSON reads r's body into v. The body must be present, one JSON value,
// with no unknown fields and no trailing data; anything else is an
// api.Invalid error, so a handler returns it as is.
func decodeJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return api.Invalid("request body unreadable")
	}
	if len(data) > maxBodyBytes {
		return api.Invalid("request body larger than %d bytes", maxBodyBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return api.Invalid("a JSON request body is required")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return api.Invalid("request body is not valid for this route: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return api.Invalid("request body has data after the first JSON value")
	}
	return nil
}
