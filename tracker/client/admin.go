package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/steveyegge/gastown/tracker/api"
)

// Stats counts the durable issues of rig (every rig when empty) by status.
func (c *Client) Stats(ctx context.Context, rig string) (api.Stats, error) {
	path := "/v1/stats"
	if rig != "" {
		path += "?rig=" + url.QueryEscape(rig)
	}
	var out api.Stats
	if err := c.do(ctx, http.MethodGet, path, nil, &out, true); err != nil {
		return api.Stats{}, err
	}
	if out.ByStatus == nil {
		out.ByStatus = map[string]int{}
	}
	return out, nil
}

// ConfigGet returns a configuration key's value; ok is false when it is unset.
func (c *Client) ConfigGet(ctx context.Context, key string) (value string, ok bool, err error) {
	var out api.ConfigValue
	if err := c.do(ctx, http.MethodGet, "/v1/config/"+url.PathEscape(key), nil, &out, true); err != nil {
		return "", false, err
	}
	return out.Value, out.Set, nil
}

// ConfigSet sets a configuration key.
func (c *Client) ConfigSet(ctx context.Context, key, value string) error {
	return c.do(ctx, http.MethodPut, "/v1/config/"+url.PathEscape(key), api.ConfigSetRequest{Value: value}, nil, false)
}
