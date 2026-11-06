package forgejoprov

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Fixed phrases the scoped-token calls quote in place of a reply. A server can echo
// the value it was asked to create back inside its error body, in an escaped form no
// mask reliably catches, so nothing from a reply reaches an error.
const (
	scopedNotDone       = "the request did not complete"
	scopedMintForbidden = "the admin token may not mint a token for this account; it needs site-administrator rights"
	scopedMintRefused   = "the server refused to mint the token"
	scopedNoValue       = "the reply held no token value"
	scopedRevokeRefused = "the server refused to revoke the token"
)

// MintToken asks Forgejo for a new token named name for user, holding scopes, and
// returns its value. The value is read from the reply's "token" field, then "sha1",
// the two names Forgejo versions use. A reply that holds no value is an error, and
// the token it made is revoked, since its value was not kept and no one can use it.
// An error names the request's path and carries the status and a fixed phrase, never
// the reply.
func (c *Client) MintToken(user, name string, scopes []string) (string, error) {
	path := "/admin/users/" + url.PathEscape(user) + "/tokens"
	status, resp, err := c.Do(http.MethodPost, path, map[string]any{"name": name, "scopes": scopes})
	if err != nil {
		return "", fmt.Errorf("POST %s: %s", path, scopedNotDone)
	}
	switch status {
	case http.StatusCreated:
	case http.StatusForbidden:
		return "", fmt.Errorf("POST %s: HTTP %d: %s", path, status, scopedMintForbidden)
	default:
		return "", fmt.Errorf("POST %s: HTTP %d: %s", path, status, scopedMintRefused)
	}
	if token := tokenFromReply(resp); token != "" {
		return token, nil
	}
	if err := c.RevokeToken(user, name); err != nil {
		return "", fmt.Errorf("POST %s: %s; the token %s could not be revoked, so it is still live: %w", path, scopedNoValue, name, err)
	}
	return "", fmt.Errorf("POST %s: %s; the token %s was revoked", path, scopedNoValue, name)
}

// tokenFromReply returns the value a token-create reply carries, or "" when the reply
// is not a JSON object or holds no string under either name.
func tokenFromReply(resp []byte) string {
	var reply map[string]any
	if err := json.Unmarshal(resp, &reply); err != nil {
		return ""
	}
	if token, _ := reply["token"].(string); token != "" {
		return token
	}
	token, _ := reply["sha1"].(string)
	return token
}

// RevokeToken deletes the token named name that user holds. It succeeds on 204 and
// returns an error built from the path, the status and a fixed phrase otherwise.
func (c *Client) RevokeToken(user, name string) error {
	path := "/admin/users/" + url.PathEscape(user) + "/tokens/" + url.PathEscape(name)
	status, _, err := c.Do(http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("DELETE %s: %s", path, scopedNotDone)
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("DELETE %s: HTTP %d: %s", path, status, scopedRevokeRefused)
	}
	return nil
}

// WithToken returns a copy of c that sends value as its bearer token, so the calls
// made with a minted token go through the same client settings. The copy holds value
// as a Secret, so printing it shows no value.
func (c *Client) WithToken(value string) *Client {
	scoped := *c
	scoped.Token = newSecret(value)
	return &scoped
}
