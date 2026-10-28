package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Client is a client for the registry's v2 HTTP API.
type Client struct {
	Base string // "http://localhost:5001"
	HTTP *http.Client
}

// Catalog lists the repositories the registry holds.
func (c *Client) Catalog(ctx context.Context) ([]string, error) {
	var out struct {
		Repositories []string `json:"repositories"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/_catalog", &out); err != nil {
		return nil, err
	}
	if out.Repositories == nil {
		out.Repositories = []string{}
	}
	return out.Repositories, nil
}

// Tags lists the tags repo holds. A repo that fails validation returns
// ErrBadName and sends no request.
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error) {
	if !ValidRepo(repo) {
		return nil, fmt.Errorf("tags %q: %w", repo, ErrBadName)
	}
	var out struct {
		Tags []string `json:"tags"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/"+repo+"/tags/list", &out); err != nil {
		return nil, err
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	return out.Tags, nil
}

// DeleteManifest deletes one manifest by digest; the registry removes every
// tag pointing at it. Arguments that fail validation return ErrBadName and
// send no request.
func (c *Client) DeleteManifest(ctx context.Context, repo, digest string) error {
	if !ValidRepo(repo) {
		return fmt.Errorf("delete %q: %w", repo, ErrBadName)
	}
	if !ValidDigest(digest) {
		return fmt.Errorf("delete %q: %w", digest, ErrBadName)
	}
	err := c.do(ctx, http.MethodDelete, "/v2/"+repo+"/manifests/"+digest, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusMethodNotAllowed {
		return fmt.Errorf("delete %q: %w", digest, ErrDeletesDisabled)
	}
	return err
}

// statusError is a response the client has no sentinel for.
type statusError struct {
	code         int
	method, path string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d", e.method, e.path, e.code)
}

func (c *Client) base() string {
	return strings.TrimRight(c.Base, "/")
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// do sends one request and decodes a JSON body into out when out is non-nil.
func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &statusError{code: resp.StatusCode, method: method, path: path}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}
