package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// CopyManifestTo copies everything the manifest ref names in repo on c needs on
// dst to be pulled from dst under dstRepo: each child manifest of an index, each
// config and layer blob the manifests name, all verified against their digests
// on the way. It returns the top manifest as read from c. The top manifest is
// not stored on dst: the caller writes it under the tag it means, after the
// blobs it names are there. A repo or ref that fails validation returns
// ErrBadName and sends no request.
func (c *Client) CopyManifestTo(ctx context.Context, dst *Client, repo, ref, dstRepo string) (ManifestBody, error) {
	if !ValidRepo(dstRepo) {
		return ManifestBody{}, fmt.Errorf("copy to %q: %w", dstRepo, ErrBadName)
	}
	top, err := c.ReadManifestBody(ctx, repo, ref)
	if err != nil {
		return ManifestBody{}, err
	}
	if err := c.copyManifestContent(ctx, dst, repo, dstRepo, top, 0); err != nil {
		return ManifestBody{}, err
	}
	return top, nil
}

// copyManifestContent copies the blobs and child manifests body names, depth
// levels below the top. An index's children are stored on dst by digest, so
// the index's entries resolve there; a child is copied with the same rules.
func (c *Client) copyManifestContent(ctx context.Context, dst *Client, repo, dstRepo string, body ManifestBody, depth int) error {
	var doc manifestDoc
	if err := json.Unmarshal(body.Body, &doc); err != nil {
		return fmt.Errorf("manifest %s in %q: decode: %w", body.Digest, repo, err)
	}
	if doc.isIndex() {
		if depth >= maxIndexDepth {
			return fmt.Errorf("manifest %s in %q nests indexes deeper than %d", body.Digest, repo, maxIndexDepth)
		}
		for _, m := range doc.Manifests {
			if !ValidDigest(m.Digest) {
				return fmt.Errorf("manifest %s in %q names a child with no sha256 digest", body.Digest, repo)
			}
			child, err := c.ReadManifestBody(ctx, repo, m.Digest)
			if err != nil {
				return err
			}
			if err := c.copyManifestContent(ctx, dst, repo, dstRepo, child, depth+1); err != nil {
				return err
			}
			if err := dst.PutManifestBody(ctx, dstRepo, child.Digest, child.ContentType, child.Body); err != nil {
				return err
			}
		}
		return nil
	}
	blobs := []string{doc.Config.Digest}
	for _, l := range doc.Layers {
		blobs = append(blobs, l.Digest)
	}
	for _, digest := range blobs {
		if err := c.CopyBlobTo(ctx, dst, repo, dstRepo, digest); err != nil {
			return err
		}
	}
	return nil
}

// CopyBlobTo copies the blob digest from repo on c to dstRepo on dst. A blob dst
// already holds is left alone. Otherwise the copy opens an upload on dst with a
// mount attempt from the source repository first: a registry that can mount the
// blob answers 201 and nothing moves; one that cannot answers 202 with an upload
// session, and the bytes are streamed from c into that session. Either way the
// blob is checked on dst afterwards. A name that fails validation returns
// ErrBadName and sends no request.
func (c *Client) CopyBlobTo(ctx context.Context, dst *Client, repo, dstRepo, digest string) error {
	if !ValidRepo(repo) || !ValidRepo(dstRepo) || !ValidDigest(digest) {
		return fmt.Errorf("copy blob %q: %w", digest, ErrBadName)
	}
	// Each base is parsed once here; every request after this is built from the
	// parsed URL, so a malformed base is refused before any request is sent.
	base, err := url.Parse(dst.base())
	if err != nil {
		return fmt.Errorf("copy blob %s: the destination %s is not a URL: %w", digest, dst.base(), err)
	}
	present, err := dst.blobPresent(ctx, base, dstRepo, digest)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	location, mounted, err := dst.mountOrOpenUpload(ctx, base, dstRepo, repo, digest)
	if err != nil {
		return err
	}
	if !mounted {
		if err := c.streamBlob(ctx, dst, base, repo, digest, location); err != nil {
			return err
		}
	}
	present, err = dst.blobPresent(ctx, base, dstRepo, digest)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("copy blob %s to %q: the registry does not hold it after the copy", digest, dstRepo)
	}
	return nil
}

// blobPresent reports whether the registry at base holds the blob digest in repo.
// A 404 is an answer, not an error; any other failure is one.
func (c *Client) blobPresent(ctx context.Context, base *url.URL, repo, digest string) (bool, error) {
	path := "/v2/" + repo + "/blobs/" + digest
	resp, done, err := c.exchange(ctx, http.MethodHead, pathURL(base, path), nil, "", 0)
	if err != nil {
		return false, fmt.Errorf("HEAD %s: %w", path, err)
	}
	defer done()
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return false, &statusError{code: resp.StatusCode, method: http.MethodHead, path: path}
	}
	return true, nil
}

// mountOrOpenUpload asks the registry at base to mount digest from srcRepo into
// dstRepo. It returns mounted true when the registry did, or the upload location
// the registry opened for the bytes.
func (c *Client) mountOrOpenUpload(ctx context.Context, base *url.URL, dstRepo, srcRepo, digest string) (location string, mounted bool, err error) {
	u := pathURL(base, "/v2/"+dstRepo+"/blobs/uploads/")
	q := url.Values{}
	q.Set("mount", digest)
	q.Set("from", srcRepo)
	u.RawQuery = q.Encode()
	path := u.Path
	resp, done, err := c.exchange(ctx, http.MethodPost, u, nil, "", 0)
	if err != nil {
		return "", false, fmt.Errorf("POST %s: %w", path, err)
	}
	defer done()
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusCreated:
		return "", true, nil
	case http.StatusAccepted:
		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", false, fmt.Errorf("POST %s: the registry opened no upload session", path)
		}
		return loc, false, nil
	}
	return "", false, &statusError{code: resp.StatusCode, method: http.MethodPost, path: path}
}

// streamBlob reads digest from repo on c and sends its bytes to the upload
// session at location on dst, which completes the upload under digest. The
// bytes are spooled and hashed before the send, and a source whose bytes do not
// hash to digest is an error before any byte reaches dst, so a mismatch never
// passes as a copy. dstBase is dst's parsed base, which the upload location
// resolves against.
func (c *Client) streamBlob(ctx context.Context, dst *Client, dstBase *url.URL, repo, digest, location string) error {
	srcBase, err := url.Parse(c.base())
	if err != nil {
		return fmt.Errorf("copy blob %s: the source %s is not a URL: %w", digest, c.base(), err)
	}
	path := "/v2/" + repo + "/blobs/" + digest
	resp, done, err := c.exchange(ctx, http.MethodGet, pathURL(srcBase, path), nil, "", 0)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer done()
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("GET %s: %w", path, ErrNotFound)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &statusError{code: resp.StatusCode, method: http.MethodGet, path: path}
	}

	// The bytes are spooled and hashed before the PUT, so a source whose bytes do
	// not hash to digest never reaches the destination's upload session. The PUT
	// then sends exactly the bytes that were hashed.
	spool, err := os.CreateTemp(c.spoolDir, "registry-blob-*")
	if err != nil {
		return fmt.Errorf("copy blob %s: spool: %w", digest, err)
	}
	defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(spool, sum), resp.Body)
	if err != nil {
		return fmt.Errorf("copy blob %s: %w", digest, err)
	}
	if got := "sha256:" + hex.EncodeToString(sum.Sum(nil)); got != digest {
		return fmt.Errorf("GET %s: the bytes hash to %s, not the digest asked", path, got)
	}

	target, err := uploadURL(dstBase, location, digest)
	if err != nil {
		return err
	}
	body := io.NewSectionReader(spool, 0, n)
	put, putDone, err := dst.exchange(ctx, http.MethodPut, target, body, "application/octet-stream", n)
	if err != nil {
		return fmt.Errorf("PUT blob %s: %w", digest, err)
	}
	defer putDone()
	defer func() { _ = put.Body.Close() }()
	if put.StatusCode < 200 || put.StatusCode > 299 {
		return &statusError{code: put.StatusCode, method: http.MethodPut, path: "/blobs/" + digest}
	}
	return nil
}

// uploadURL is the upload session location with digest set as the query
// parameter that completes it. A relative location is resolved against dst's
// base, and a location on another origin than dst is refused: the bytes are
// only sent to the registry the copy was opened on.
func uploadURL(base *url.URL, location, digest string) (*url.URL, error) {
	ref, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("upload location %q: %w", location, err)
	}
	u := base.ResolveReference(ref)
	if u.Scheme != base.Scheme || u.Host != base.Host {
		return nil, fmt.Errorf("upload location %q names another origin", location)
	}
	q := u.Query()
	q.Set("digest", digest)
	u.RawQuery = q.Encode()
	return u, nil
}

// pathURL is the URL of path under base, which may carry a prefix a proxy puts in
// front of the registry. It cannot fail: the base is already parsed.
func pathURL(base *url.URL, path string) *url.URL {
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + path
	u.RawPath = ""
	u.RawQuery = ""
	return &u
}

// exchange sends one request for u with the client's http client, bounded by the
// client's wait. The request is built from a parsed URL, so it cannot fail to
// build. body, when set, is sent with contentType and length, where a length of
// zero means unknown. done releases the wait and must be called once the response
// is no longer read.
func (c *Client) exchange(ctx context.Context, method string, u *url.URL, body io.Reader, contentType string, length int64) (resp *http.Response, done func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, c.waitBound())
	req := (&http.Request{Method: method, URL: u, Header: http.Header{}}).WithContext(ctx)
	if body != nil {
		req.Body = io.NopCloser(body)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if length > 0 {
		req.ContentLength = length
	}
	resp, err = c.httpClient().Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}
