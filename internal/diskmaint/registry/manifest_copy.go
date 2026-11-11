package registry

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
)

// ManifestBody is a manifest the registry holds as its bytes: the media type it
// was sent as, the digest those bytes hash to, and the body itself. A copy of a
// tag to another tag of the same repository is a PutManifestBody of these bytes,
// which moves no layer.
type ManifestBody struct {
	ContentType string
	Digest      string
	Body        []byte
}

// ReadManifestBody reads the manifest ref names in repo as bytes. The body is
// checked against the digest the registry names it by, so a registry cannot
// answer with bytes other than the ones that digest pins. A repo or ref that
// fails validation returns ErrBadName and sends no request.
func (c *Client) ReadManifestBody(ctx context.Context, repo, ref string) (ManifestBody, error) {
	if !ValidRepo(repo) {
		return ManifestBody{}, fmt.Errorf("manifest %q: %w", repo, ErrBadName)
	}
	if !ValidTag(ref) && !ValidDigest(ref) {
		return ManifestBody{}, fmt.Errorf("manifest %q: %w", ref, ErrBadName)
	}
	path := "/v2/" + repo + "/manifests/" + ref
	header, body, err := c.getBody(ctx, path, manifestAccept, &manifestWalk{})
	if err != nil {
		return ManifestBody{}, err
	}
	top := hashOf(body)
	if header.Get("Docker-Content-Digest") != top {
		return ManifestBody{}, fmt.Errorf("GET %s: the Docker-Content-Digest does not name the body, which hashes to %s", path, top)
	}
	return ManifestBody{ContentType: header.Get("Content-Type"), Digest: top, Body: body}, nil
}

// PutManifestBody stores body as the manifest of tag in repo, sent as
// contentType. tag may be a digest, which is how an index's child manifests are
// stored. The registry answers 2xx once it holds the bytes. A repo or tag
// that fails validation returns ErrBadName and sends no request.
func (c *Client) PutManifestBody(ctx context.Context, repo, tag, contentType string, body []byte) error {
	if !ValidRepo(repo) {
		return fmt.Errorf("put %q: %w", repo, ErrBadName)
	}
	if !ValidTag(tag) && !ValidDigest(tag) {
		return fmt.Errorf("put %q: %w", tag, ErrBadName)
	}
	path := "/v2/" + repo + "/manifests/" + tag
	ctx, cancel := context.WithTimeout(ctx, c.waitBound())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base()+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &statusError{code: resp.StatusCode, method: http.MethodPut, path: path}
	}
	return nil
}
