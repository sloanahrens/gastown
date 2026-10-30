package forgejoprov

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// bodyTailLimit is how many response bytes an error message quotes, as the
// script's body_tail cuts them.
const bodyTailLimit = 300

// Client is the Forgejo API the provisioning run writes through. Every request
// carries Token in its Authorization header and nowhere else.
type Client struct {
	// API is the API root, joined to each request path as given.
	API string
	// HTTP sends the requests; nil means http.DefaultClient.
	HTTP *http.Client
	// Token is the site-admin token, or a bot token once minted.
	Token Secret
	// DryRun makes every write a no-op, so a dry run reads and reports only.
	DryRun bool
	// Log receives the run's progress lines. Do itself logs nothing.
	Log func(string)
}

// Do sends one request and returns its status and body. A dry run answers
// every non-GET request with status 0 and no body without sending it.
func (c *Client) Do(method, path string, body any) (int, []byte, error) {
	if c.DryRun && method != http.MethodGet {
		return 0, nil, nil
	}
	url := c.API + path
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("%s %s: encode body: %w", method, url, err)
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	req.Header.Set("Authorization", "token "+c.Token.Reveal())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// The cause is left out: it can name the request in more detail than
		// the run's log should carry.
		return 0, nil, fmt.Errorf("%s %s: request failed", method, url)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: read reply: %w", method, url, err)
	}
	return resp.StatusCode, data, nil
}

// BodyTail renders the start of a response body for an error message: an
// em dash and the first 300 bytes with newlines as spaces, or "" for no body.
func BodyTail(resp []byte) string {
	if len(resp) == 0 {
		return ""
	}
	head := resp
	if len(head) > bodyTailLimit {
		head = head[:bodyTailLimit]
	}
	return " — " + strings.ReplaceAll(string(head), "\n", " ")
}

// Unexpected is the error for a status the run did not expect, quoting the
// reply through BodyTail.
func Unexpected(method, path string, status int, resp []byte) error {
	return fmt.Errorf("%s %s: unexpected HTTP %d%s", method, path, status, BodyTail(resp))
}

// FindObject returns the first object of the JSON array list whose key is the
// string value, and false when there is none or list is not a JSON array of
// objects. Malformed JSON is no match, not a panic.
func FindObject(list []byte, key, value string) (map[string]any, bool) {
	var items []any
	if err := json.Unmarshal(list, &items); err != nil {
		return nil, false
	}
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := obj[key].(string); ok && s == value {
			return obj, true
		}
	}
	return nil, false
}
