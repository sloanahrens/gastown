package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Media types the client asks the registry for, in the order it prefers them.
const (
	mediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaTypeDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// manifestAccept asks for an index first: a registry that holds an index for
// this ref answers with it, and one that holds a single image answers with the
// image.
const manifestAccept = mediaTypeOCIIndex + ", " + mediaTypeDockerList + ", " +
	mediaTypeOCIManifest + ", " + mediaTypeDockerManifest

// maxIndexDepth bounds how deep a manifest index may nest, so a registry that
// points an index at itself cannot loop the client.
const maxIndexDepth = 4

// Manifest is what the client learns about one manifest: its digest, when it
// was built and how many bytes its config and layers hold.
type Manifest struct {
	Digest  string
	Created time.Time
	Bytes   int64
}

// manifestDoc is the part of a manifest body the client reads. It is one shape
// for both an image manifest (config and layers) and an index (manifests).
type manifestDoc struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"config"`
	Layers []struct {
		Size int64 `json:"size"`
	} `json:"layers"`
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests"`
}

// isIndex reports whether the body describes an index rather than an image.
func (d manifestDoc) isIndex() bool {
	switch d.MediaType {
	case mediaTypeOCIIndex, mediaTypeDockerList:
		return true
	case mediaTypeOCIManifest, mediaTypeDockerManifest:
		return false
	}
	// A body with no media type is judged by its shape.
	return len(d.Manifests) > 0
}

// GetManifest reads the manifest ref names in repo. For an image manifest its
// digest comes from Docker-Content-Digest, its created time from the config
// blob and its size from the config and layers. For an index the digest is the
// index's own, the created time is the newest child's, and the size adds up the
// config and layers of every child. A repo or ref that fails validation returns
// ErrBadName and sends no request; a missing created time is an error, never a
// zero time.
func (c *Client) GetManifest(ctx context.Context, repo, ref string) (Manifest, error) {
	if !ValidRepo(repo) {
		return Manifest{}, fmt.Errorf("manifest %q: %w", repo, ErrBadName)
	}
	if !ValidTag(ref) && !ValidDigest(ref) {
		return Manifest{}, fmt.Errorf("manifest %q: %w", ref, ErrBadName)
	}
	return c.getManifest(ctx, repo, ref, 0)
}

// getManifest is GetManifest with a nesting depth, so an index can resolve its
// children through the same path.
func (c *Client) getManifest(ctx context.Context, repo, ref string, depth int) (Manifest, error) {
	if depth > maxIndexDepth {
		return Manifest{}, fmt.Errorf("manifest %q in %q: index nests deeper than %d", ref, repo, maxIndexDepth)
	}
	path := "/v2/" + repo + "/manifests/" + ref
	var doc manifestDoc
	header, err := c.getJSON(ctx, path, manifestAccept, &doc)
	if err != nil {
		return Manifest{}, err
	}
	digest := header.Get("Docker-Content-Digest")
	if !ValidDigest(digest) {
		return Manifest{}, fmt.Errorf("GET %s: the response carries no valid Docker-Content-Digest", path)
	}
	if doc.isIndex() {
		return c.indexManifest(ctx, repo, digest, doc, depth)
	}
	return c.imageManifest(ctx, repo, digest, doc)
}

// imageManifest reads the created time of doc's config blob and adds up the
// sizes the manifest names.
func (c *Client) imageManifest(ctx context.Context, repo, digest string, doc manifestDoc) (Manifest, error) {
	if !ValidDigest(doc.Config.Digest) {
		return Manifest{}, fmt.Errorf("manifest %s: the config digest fails validation", digest)
	}
	created, err := c.blobCreated(ctx, repo, doc.Config.Digest)
	if err != nil {
		return Manifest{}, err
	}
	size := doc.Config.Size
	for _, layer := range doc.Layers {
		size += layer.Size
	}
	return Manifest{Digest: digest, Created: created, Bytes: size}, nil
}

// indexManifest reads every child of an index: the newest child's created time
// dates the index, and the children's configs and layers add up to its size.
func (c *Client) indexManifest(ctx context.Context, repo, digest string, doc manifestDoc, depth int) (Manifest, error) {
	if len(doc.Manifests) == 0 {
		return Manifest{}, fmt.Errorf("index %s: no child manifests", digest)
	}
	manifest := Manifest{Digest: digest}
	for _, child := range doc.Manifests {
		if !ValidDigest(child.Digest) {
			return Manifest{}, fmt.Errorf("index %s: a child digest fails validation", digest)
		}
		childManifest, err := c.getManifest(ctx, repo, child.Digest, depth+1)
		if err != nil {
			return Manifest{}, err
		}
		if childManifest.Created.After(manifest.Created) {
			manifest.Created = childManifest.Created
		}
		manifest.Bytes += childManifest.Bytes
	}
	return manifest, nil
}

// blobCreated reads the created time from a config blob.
func (c *Client) blobCreated(ctx context.Context, repo, digest string) (time.Time, error) {
	path := "/v2/" + repo + "/blobs/" + digest
	var blob struct {
		Created string `json:"created"`
	}
	if _, err := c.getJSON(ctx, path, "", &blob); err != nil {
		return time.Time{}, err
	}
	if blob.Created == "" {
		return time.Time{}, fmt.Errorf("GET %s: the config blob has no created time", path)
	}
	created, err := time.Parse(time.RFC3339Nano, blob.Created)
	if err != nil {
		return time.Time{}, fmt.Errorf("GET %s: created is not an RFC3339 time: %w", path, err)
	}
	return created.UTC(), nil
}

// getJSON sends one GET carrying accept and decodes the body into out. It
// returns the response headers so the caller can read Docker-Content-Digest.
func (c *Client) getJSON(ctx context.Context, path, accept string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s %s: %w", http.MethodGet, path, ErrNotFound)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &statusError{code: resp.StatusCode, method: http.MethodGet, path: path}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("%s %s: decode: %w", http.MethodGet, path, err)
	}
	return resp.Header, nil
}
