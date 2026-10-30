package forgejoprov

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// bodyTailLimit is how many response bytes an error message quotes, as the
// script's body_tail cuts them.
const bodyTailLimit = 300

// requestTimeout bounds one request on the default client, so a stalled Forgejo
// cannot hold the run open forever.
const requestTimeout = 30 * time.Second

// defaultHTTP sends the requests of a Client whose HTTP is nil.
var defaultHTTP = &http.Client{Timeout: requestTimeout}

// sensitiveField matches a JSON string value under a key that carries a
// credential: a minted token's sha1, a token, or a password. A reply can echo
// one, so BodyTail masks it before quoting the reply.
var sensitiveField = regexp.MustCompile(`("(?:sha1|token|access_token|password)"\s*:\s*)"[^"]*"`)

// Client is the Forgejo API the provisioning run writes through. Every request
// carries Token in its Authorization header and nowhere else.
type Client struct {
	// API is the API root, joined to each request path as given.
	API string
	// HTTP sends the requests; nil means a default client with a timeout.
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
	resp, err := c.httpClient().Do(req)
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

// httpClient returns the client a request goes through: the caller's HTTP when
// it set one, otherwise the default client that times out.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

// BodyTail renders the start of the client's reply for an error message: a
// hyphen and the first 300 bytes with newlines as spaces, or "" for no body.
// The client's own token is scrubbed from the whole reply first, then the JSON
// credential fields are masked, and only then is the reply cut, so a token that
// straddles the cut cannot leave its first part behind. The cut never splits a
// UTF-8 sequence.
func (c *Client) BodyTail(resp []byte) string {
	if len(resp) == 0 {
		return ""
	}
	return bodyTail([]byte(c.Token.Scrub(string(resp))))
}

// Unexpected is the error for a status the run did not expect, quoting the
// reply through the client's BodyTail so the token never reaches the message.
func (c *Client) Unexpected(method, path string, status int, resp []byte) error {
	return fmt.Errorf("%s %s: unexpected HTTP %d%s", method, path, status, c.BodyTail(resp))
}

// bodyTail masks the JSON credential fields of resp and cuts it for an error
// message. It knows no token, so callers go through Client.BodyTail.
func bodyTail(resp []byte) string {
	if len(resp) == 0 {
		return ""
	}
	body := sensitiveField.ReplaceAll(resp, []byte(`${1}"`+redacted+`"`))
	cut := len(body)
	if cut > bodyTailLimit {
		cut = bodyTailLimit
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
	}
	return " - " + strings.ReplaceAll(string(body[:cut]), "\n", " ")
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
