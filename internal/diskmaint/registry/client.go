package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// pageSize is how many names one listing request asks for. A paginating
	// registry answers with a Link header only to a request that asked for a
	// page, so the client always sends n.
	pageSize = 100
	// maxPages bounds one listing. A listing past it is an error rather than a
	// short read: a Link header that points back cannot loop the client, and a
	// catalogue the client stops reading early must not pass as whole.
	maxPages = 1000
	// maxBodyBytes bounds the response body the client decodes, so a registry
	// cannot make it allocate without limit.
	maxBodyBytes = 8 << 20
	// requestTimeout bounds one request sent through defaultHTTPClient: a
	// registry that accepts a connection and never answers must not hang a run.
	requestTimeout = 30 * time.Second
)

// defaultHTTPClient is the client a Client without one sends through.
var defaultHTTPClient = &http.Client{Timeout: requestTimeout}

// Client is a client for the registry's v2 HTTP API.
type Client struct {
	Base string // "http://localhost:5001"
	HTTP *http.Client
}

// page is the shape both listings share: a catalog page carries repositories
// and a tag page carries tags, and a response carries one of the two.
type page struct {
	Repositories []string `json:"repositories"`
	Tags         []string `json:"tags"`
}

// Catalog lists the repositories the registry holds, reading every page.
func (c *Client) Catalog(ctx context.Context) ([]string, error) {
	repos := []string{}
	err := c.eachPage(ctx, "/v2/_catalog", func(p page) {
		repos = append(repos, p.Repositories...)
	})
	if err != nil {
		return nil, err
	}
	return repos, nil
}

// Tags lists the tags repo holds, reading every page. A repo that fails
// validation returns ErrBadName and sends no request.
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error) {
	if !ValidRepo(repo) {
		return nil, fmt.Errorf("tags %q: %w", repo, ErrBadName)
	}
	tags := []string{}
	err := c.eachPage(ctx, "/v2/"+repo+"/tags/list", func(p page) {
		tags = append(tags, p.Tags...)
	})
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// eachPage reads the listing at path page by page, calling add with each page,
// and stops where a registry places no next link.
func (c *Client) eachPage(ctx context.Context, path string, add func(page)) error {
	next := path + "?n=" + strconv.Itoa(pageSize)
	seen := make(map[string]bool, 1)
	for pages := 0; ; pages++ {
		if pages == maxPages {
			return fmt.Errorf("GET %s: the listing runs past %d pages", path, maxPages)
		}
		if seen[next] {
			return fmt.Errorf("GET %s: the Link header names %q again", path, next)
		}
		seen[next] = true
		var p page
		answered, err := c.do(ctx, http.MethodGet, next, &p)
		if err != nil {
			return err
		}
		add(p)
		if next, err = nextPage(answered, path); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
	}
}

// nextPage returns the path the answered page links to as next, or "" at the
// last page. A link to another origin is an error: only the origin that
// answered this listing is read.
func nextPage(answered reply, path string) (string, error) {
	target := nextLink(answered.header)
	if target == "" {
		return "", nil
	}
	ref, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("GET %s: Link %q: %w", path, target, err)
	}
	if !ref.IsAbs() && !strings.HasPrefix(target, "/") {
		return "", fmt.Errorf("GET %s: Link %q is not a path", path, target)
	}
	next := answered.url.ResolveReference(ref)
	if next.Scheme != answered.url.Scheme || next.Host != answered.url.Host {
		return "", fmt.Errorf("GET %s: Link %q names another origin", path, target)
	}
	return next.RequestURI(), nil
}

// nextLink returns the target of the first Link header entry whose rel is next,
// or "" when the header carries none.
func nextLink(header http.Header) string {
	for _, value := range header.Values("Link") {
		for _, entry := range strings.Split(value, ",") {
			entry = strings.TrimSpace(entry)
			open, end := strings.IndexByte(entry, '<'), strings.IndexByte(entry, '>')
			if open != 0 || end < 0 {
				continue
			}
			if hasRelNext(entry[end+1:]) {
				return entry[open+1 : end]
			}
		}
	}
	return ""
}

// hasRelNext reports whether the parameters after a Link entry's target name
// the next relation.
func hasRelNext(params string) bool {
	for _, param := range strings.Split(params, ";") {
		name, value, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "rel") {
			continue
		}
		for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(value), `"`)) {
			if rel == "next" {
				return true
			}
		}
	}
	return false
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
	_, err := c.do(ctx, http.MethodDelete, "/v2/"+repo+"/manifests/"+digest, nil)
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
	return defaultHTTPClient
}

// reply is one answered request: the header, which carries a listing's next
// link, and the URL that answered.
type reply struct {
	header http.Header
	url    *url.URL
}

// do sends one request and decodes a JSON body into out when out is non-nil.
func (c *Client) do(ctx context.Context, method, path string, out any) (reply, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, nil)
	if err != nil {
		return reply{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return reply{}, fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return reply{}, &statusError{code: resp.StatusCode, method: method, path: path}
	}
	answered := reply{header: resp.Header, url: req.URL}
	if out == nil {
		return answered, nil
	}
	body := http.MaxBytesReader(nil, resp.Body, maxBodyBytes)
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return reply{}, fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return answered, nil
}
