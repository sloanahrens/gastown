package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// catalog the client stops reading early must not pass as whole.
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

	// pageLimit and waitLimit narrow the package bounds so a test reaches the
	// page bound and the request deadline without a listing of a thousand
	// pages or a wait of thirty seconds. A zero field takes the package
	// default, which is what the town's runs take.
	pageLimit int
	waitLimit time.Duration
}

// pageBound is the number of pages one listing reads.
func (c *Client) pageBound() int {
	if c.pageLimit > 0 {
		return c.pageLimit
	}
	return maxPages
}

// waitBound bounds one request.
func (c *Client) waitBound() time.Duration {
	if c.waitLimit > 0 {
		return c.waitLimit
	}
	return requestTimeout
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
// and stops where a registry places no next link. The pages it reads hold at
// most pageSize entries each, so a listing past that many entries is an error
// rather than a slice that grows whatever the registry sends.
func (c *Client) eachPage(ctx context.Context, path string, add func(page)) error {
	next, err := c.requestURL(path + "?n=" + strconv.Itoa(pageSize))
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	pages := c.pageBound()
	entryBound := pageSize * pages
	seen := make(map[string]bool, 1)
	entries := 0
	for read := 0; ; read++ {
		if read == pages {
			return fmt.Errorf("GET %s: the listing runs past %d pages", path, pages)
		}
		if seen[next.String()] {
			return fmt.Errorf("GET %s: the Link header names %q again", path, next.RequestURI())
		}
		seen[next.String()] = true
		var p page
		answered, err := c.do(ctx, http.MethodGet, next, &p)
		if err != nil {
			return err
		}
		entries += len(p.Repositories) + len(p.Tags)
		if entries > entryBound {
			return fmt.Errorf("GET %s: the listing holds more than %d entries", path, entryBound)
		}
		add(p)
		if next, err = c.nextPage(answered, path); err != nil {
			return err
		}
		if next == nil {
			return nil
		}
	}
}

// nextPage returns the URL the answered page links to as next, or nil at the
// last page. A link to another origin is an error: only the origin that
// answered this listing is read.
func (c *Client) nextPage(answered reply, path string) (*url.URL, error) {
	target := nextLink(answered.header)
	if target == "" {
		return nil, nil
	}
	ref, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("GET %s: Link %q: %w", path, target, err)
	}
	if !ref.IsAbs() && !strings.HasPrefix(target, "/") {
		return nil, fmt.Errorf("GET %s: Link %q is not a path", path, target)
	}
	next := answered.url.ResolveReference(ref)
	if next.Scheme != answered.url.Scheme || next.Host != answered.url.Host {
		return nil, fmt.Errorf("GET %s: Link %q names another origin", path, target)
	}
	// The base carries any prefix a proxy puts in front of the registry. A
	// link that already names that prefix is read as it stands, and one that
	// leaves it out is a path under the base, so the prefix goes back on.
	if base := c.basePath(); base != "" && !underPath(next.Path, base) {
		next.Path = base + next.Path
	}
	return next, nil
}

// underPath reports whether p is base or lies under it.
func underPath(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+"/")
}

// nextLink returns the target of the first Link header entry whose rel is next,
// or "" when the header carries none.
func nextLink(header http.Header) string {
	for _, value := range header.Values("Link") {
		for _, entry := range linkEntries(value) {
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

// linkEntries returns the entries one Link header value carries: the commas
// that separate entries are the ones outside a target's angle brackets and
// outside a quoted parameter, so a target or a title that holds a comma stays
// part of its entry.
func linkEntries(value string) []string {
	var entries []string
	start, angle, quote := 0, false, false
	for i := 0; i < len(value); i++ {
		switch char := value[i]; {
		case quote && char == '\\' && i+1 < len(value):
			i++ // a quoted-pair keeps the byte after it inside the parameter
		case char == '"' && !angle:
			quote = !quote
		case char == '<' && !quote:
			angle = true
		case char == '>' && !quote:
			angle = false
		case char == ',' && !angle && !quote:
			entries = append(entries, value[start:i])
			start = i + 1
		}
	}
	return append(entries, value[start:])
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
	u, err := c.requestURL("/v2/" + repo + "/manifests/" + digest)
	if err != nil {
		return fmt.Errorf("delete %q: %w", digest, err)
	}
	_, err = c.do(ctx, http.MethodDelete, u, nil)
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

// base is the base URL without the trailing slash a path is joined onto.
func (c *Client) base() string {
	return strings.TrimRight(c.Base, "/")
}

// basePath is the path the base names, or "" when it names the host alone.
func (c *Client) basePath() string {
	u, err := url.Parse(c.base())
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// requestURL returns the URL of a path under the base.
func (c *Client) requestURL(path string) (*url.URL, error) {
	return url.Parse(c.base() + path)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTPClient
}

// reply is one answered request: the header, which carries a listing's next
// link, and the URL that was asked.
type reply struct {
	header http.Header
	url    *url.URL
}

// do sends the request for u and decodes a JSON body into out when out is
// non-nil. The request itself carries the wait bound whether or not its http
// client does, because the client a caller supplies may set no timeout at all.
func (c *Client) do(ctx context.Context, method string, u *url.URL, out any) (reply, error) {
	ctx, cancel := context.WithTimeout(ctx, c.waitBound())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return reply{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	path := u.RequestURI()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return reply{}, fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return reply{}, &statusError{code: resp.StatusCode, method: method, path: path}
	}
	answered := reply{header: resp.Header, url: u}
	if out == nil {
		return answered, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return reply{}, fmt.Errorf("%s %s: read: %w", method, path, err)
	}
	if len(body) > maxBodyBytes {
		return reply{}, fmt.Errorf("%s %s: the body is larger than %d bytes", method, path, maxBodyBytes)
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(out); err != nil {
		return reply{}, fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return answered, nil
}
