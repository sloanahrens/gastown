package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/steveyegge/gastown/tracker/api"
)

// Stats counts the durable issues of rig (every rig when empty) by status, and
// its wisps as one total.
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

// IssueToken returns a new token for actor with scope, a seat's when empty;
// this answer is the one copy the tracker ever makes in the clear, and a
// repeated call issues another token rather than reusing one.
func (c *Client) IssueToken(ctx context.Context, actor string, scope api.Scope) (api.IssuedToken, error) {
	var out api.IssuedToken
	err := c.do(ctx, http.MethodPost, "/v1/tokens", api.IssueTokenRequest{Actor: actor, Scope: scope}, &out, false)
	return out, err
}

// RevokeActorTokens ends every live token of actor and reports how many it
// ended; an actor with none is a success that ended zero.
func (c *Client) RevokeActorTokens(ctx context.Context, actor string) (int, error) {
	var out api.RevokeTokensResult
	if err := c.do(ctx, http.MethodPost, "/v1/tokens:revoke", api.RevokeTokensRequest{Actor: actor}, &out, false); err != nil {
		return 0, err
	}
	return out.Revoked, nil
}

// RevokeTokenID ends the live token with id; an id naming none is an error
// satisfying errors.Is(err, api.ErrNotFound).
func (c *Client) RevokeTokenID(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/tokens:revoke", api.RevokeTokensRequest{TokenID: id}, nil, false)
}

// PurgeWisps deletes the expired wisps among ids (admin scope). The wisp policy
// keeps the rest, and each kept id comes back with its reason. Not retried: a
// purge that answered with an error may have landed, and a retry would then
// report the deleted ids as not found.
func (c *Client) PurgeWisps(ctx context.Context, ids []string) (api.PurgeWispsResult, error) {
	var out api.PurgeWispsResult
	err := c.do(ctx, http.MethodPost, "/v1/wisps:purge", api.PurgeWispsRequest{IDs: ids}, &out, false)
	return out, err
}
