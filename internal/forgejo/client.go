// Package forgejo is the Forgejo API client for the landing path.
//
// The landing worker posts and polls commit statuses, opens and merges the
// land PR, and reads workflow runs and their job logs; the mirror monitor
// reads push mirrors. One client addresses one instance, and its token comes
// from the role's file (~/.config/gt/forgejo-<role>.env), never from config.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// defaultBaseURL is the local Forgejo instance's API root. Per-rig config
// (slice 3) overrides it through WithBaseURL.
const defaultBaseURL = "http://127.0.0.1:3000/api/v1"

// Client is an authenticated Forgejo API client.
type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
	tokenFile  string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client the requests go through.
func WithHTTPClient(c *http.Client) Option { return func(cl *Client) { cl.httpClient = c } }

// WithBaseURL sets the API root, such as https://forgejo.example/api/v1.
func WithBaseURL(u string) Option { return func(cl *Client) { cl.baseURL = strings.TrimRight(u, "/") } }

// WithToken sets the token directly, skipping the role's token file.
func WithToken(t string) Option { return func(cl *Client) { cl.token = t } }

// WithTokenFile reads the token from path instead of the role's token file.
func WithTokenFile(path string) Option { return func(cl *Client) { cl.tokenFile = path } }

// NewClient builds a client for role, reading the role's token unless an
// option supplies one.
func NewClient(role string, opts ...Option) (*Client, error) {
	c := &Client{httpClient: http.DefaultClient, baseURL: defaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	if c.token != "" {
		return c, nil
	}
	var (
		token string
		err   error
	)
	if c.tokenFile != "" {
		token, err = ReadTokenFile(c.tokenFile)
	} else {
		token, err = ReadToken(role)
	}
	if err != nil {
		return nil, err
	}
	c.token = token
	return c, nil
}

// APIError is a non-2xx response from the Forgejo API.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

// Error reports the request, its status and the server's message.
func (e *APIError) Error() string {
	return fmt.Sprintf("forgejo: %s %s returned %d: %s", e.Method, e.Path, e.StatusCode, strings.TrimSpace(e.Body))
}

// IsConflict reports the 409 a moved base branch produces, which the worker
// answers by rebuilding the candidate rather than failing the landing.
func (e *APIError) IsConflict() bool { return e.StatusCode == http.StatusConflict }

// repoPath builds /repos/{owner}/{repo} followed by suffix.
func repoPath(owner, repo, suffix string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + suffix
}

// call sends one JSON request and decodes a JSON response into out, which may
// be nil for a call whose response the caller ignores.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	data, err := c.do(ctx, method, path, query, body, nil)
	if err != nil {
		return err
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("forgejo: decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// do sends one request and returns the whole response body. A non-2xx
// response becomes an *APIError, which never carries the token.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, header http.Header) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("forgejo: marshal %s %s request: %w", method, path, err)
		}
		reqBody = bytes.NewReader(b)
	}

	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reqBody)
	if err != nil {
		return nil, fmt.Errorf("forgejo: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// A caller's header replaces the default, so the job-log call can ask for
	// text/plain rather than send two Accept values.
	for k, vs := range header {
		req.Header[k] = vs
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forgejo: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("forgejo: read %s %s response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}

// User is the subset of a Forgejo account the landing path checks.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
