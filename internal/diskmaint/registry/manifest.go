package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

// maxIndexRequests bounds the requests one GetManifest spends walking an
// index, so a registry that names a great many children cannot decide how many
// requests the client sends. A real index names a handful of platforms for a
// few dozen requests; 256 leaves room for a large one and still bounds the walk.
const maxIndexRequests = 256

// maxDocumentBytes bounds one JSON body the client decodes. Registry software
// commonly caps a manifest at 4 MiB, so a body past that is refused rather than
// read into memory.
const maxDocumentBytes = 4 << 20

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
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
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
// config and layers of every child, counting a blob that several children share
// once. A repo or ref that fails validation returns ErrBadName and sends no
// request; a missing created time is an error, never a zero time.
func (c *Client) GetManifest(ctx context.Context, repo, ref string) (Manifest, error) {
	if !ValidRepo(repo) {
		return Manifest{}, fmt.Errorf("manifest %q: %w", repo, ErrBadName)
	}
	if !ValidTag(ref) && !ValidDigest(ref) {
		return Manifest{}, fmt.Errorf("manifest %q: %w", ref, ErrBadName)
	}
	read, err := c.getManifest(ctx, repo, ref, 0, &manifestWalk{})
	if err != nil {
		return Manifest{}, err
	}
	return read.manifest, nil
}

// manifestRead is one manifest's answer: the Manifest the caller sees and the
// blobs it holds, keyed by blob digest. An index merges its children's sets, so
// a layer two children share adds its size once.
type manifestRead struct {
	manifest Manifest
	blobs    map[string]int64
}

// manifestWalk is the state one GetManifest carries through an index's
// children: how many requests it has spent, so the walk stops at a budget
// instead of reading whatever number of children the registry names.
type manifestWalk struct {
	requests int
}

// spend charges one request to the walk and refuses past the budget.
func (w *manifestWalk) spend() error {
	w.requests++
	if w.requests > maxIndexRequests {
		return fmt.Errorf("the manifest walk spends more than %d requests", maxIndexRequests)
	}
	return nil
}

// getManifest is GetManifest with a nesting depth, so an index can resolve its
// children through the same path.
func (c *Client) getManifest(ctx context.Context, repo, ref string, depth int, walk *manifestWalk) (manifestRead, error) {
	if depth > maxIndexDepth {
		return manifestRead{}, fmt.Errorf("manifest %q in %q: index nests deeper than %d", ref, repo, maxIndexDepth)
	}
	path := "/v2/" + repo + "/manifests/" + ref
	var doc manifestDoc
	header, err := c.getJSON(ctx, path, manifestAccept, &doc, walk)
	if err != nil {
		return manifestRead{}, err
	}
	digest := header.Get("Docker-Content-Digest")
	if !ValidDigest(digest) {
		return manifestRead{}, fmt.Errorf("GET %s: the response carries no valid Docker-Content-Digest", path)
	}
	if doc.isIndex() {
		return c.indexManifest(ctx, repo, digest, doc, depth, walk)
	}
	return c.imageManifest(ctx, repo, digest, doc, walk)
}

// imageManifest reads the created time of doc's config blob and collects the
// blobs the manifest names.
func (c *Client) imageManifest(ctx context.Context, repo, digest string, doc manifestDoc, walk *manifestWalk) (manifestRead, error) {
	if !ValidDigest(doc.Config.Digest) {
		return manifestRead{}, fmt.Errorf("manifest %s: the config digest fails validation", digest)
	}
	created, err := c.blobCreated(ctx, repo, doc.Config.Digest, walk)
	if err != nil {
		return manifestRead{}, err
	}
	blobs := map[string]int64{doc.Config.Digest: doc.Config.Size}
	for i, layer := range doc.Layers {
		blobs[layerKey(digest, layer.Digest, i)] = layer.Size
	}
	return manifestRead{
		manifest: Manifest{Digest: digest, Created: created, Bytes: totalBytes(blobs)},
		blobs:    blobs,
	}, nil
}

// layerKey is the key a layer adds to a manifest's blobs: its digest, which a
// second child naming the same content shares. A layer without a valid digest
// is keyed by its manifest and position, so it is still counted, once, and
// never matched to an unrelated layer.
func layerKey(manifestDigest, layerDigest string, position int) string {
	if ValidDigest(layerDigest) {
		return layerDigest
	}
	return fmt.Sprintf("#%s/layer-%d", manifestDigest, position)
}

// totalBytes adds up the sizes of a blob set.
func totalBytes(blobs map[string]int64) int64 {
	var total int64
	for _, size := range blobs {
		total += size
	}
	return total
}

// indexManifest reads every child of an index: the newest child's created time
// dates the index, and the children's blobs add up to its size.
func (c *Client) indexManifest(ctx context.Context, repo, digest string, doc manifestDoc, depth int, walk *manifestWalk) (manifestRead, error) {
	if len(doc.Manifests) == 0 {
		return manifestRead{}, fmt.Errorf("index %s: no child manifests", digest)
	}
	read := manifestRead{manifest: Manifest{Digest: digest}, blobs: map[string]int64{}}
	for _, child := range doc.Manifests {
		if !ValidDigest(child.Digest) {
			return manifestRead{}, fmt.Errorf("index %s: a child digest fails validation", digest)
		}
		childRead, err := c.getManifest(ctx, repo, child.Digest, depth+1, walk)
		if err != nil {
			return manifestRead{}, err
		}
		if childRead.manifest.Created.After(read.manifest.Created) {
			read.manifest.Created = childRead.manifest.Created
		}
		for blobDigest, size := range childRead.blobs {
			read.blobs[blobDigest] = size
		}
	}
	read.manifest.Bytes = totalBytes(read.blobs)
	return read, nil
}

// blobCreated reads the created time from a config blob.
func (c *Client) blobCreated(ctx context.Context, repo, digest string, walk *manifestWalk) (time.Time, error) {
	path := "/v2/" + repo + "/blobs/" + digest
	var blob struct {
		Created string `json:"created"`
	}
	if _, err := c.getJSON(ctx, path, "", &blob, walk); err != nil {
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

// ManifestDigest reads the manifest ref names in repo and returns the digest of
// the bytes it read: the sha256 of the body, which is the digest a pull by digest
// verifies the manifest against. The registry's Docker-Content-Digest must name
// those same bytes, so a reply whose header names another digest, or none, is
// refused. A repo or ref that fails validation returns ErrBadName and sends no
// request.
func (c *Client) ManifestDigest(ctx context.Context, repo, ref string) (string, error) {
	if !ValidRepo(repo) {
		return "", fmt.Errorf("manifest %q: %w", repo, ErrBadName)
	}
	if !ValidTag(ref) && !ValidDigest(ref) {
		return "", fmt.Errorf("manifest %q: %w", ref, ErrBadName)
	}
	path := "/v2/" + repo + "/manifests/" + ref
	header, body, err := c.getBody(ctx, path, manifestAccept, &manifestWalk{})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if header.Get("Docker-Content-Digest") != digest {
		return "", fmt.Errorf("GET %s: the Docker-Content-Digest does not name the body, which hashes to %s", path, digest)
	}
	return digest, nil
}

// getJSON sends one GET carrying accept and decodes the body into out. It
// returns the response headers so the caller can read Docker-Content-Digest.
func (c *Client) getJSON(ctx context.Context, path, accept string, out any, walk *manifestWalk) (http.Header, error) {
	header, body, err := c.getBody(ctx, path, accept, walk)
	if err != nil {
		return nil, err
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(out); err != nil {
		return nil, fmt.Errorf("%s %s: decode: %w", http.MethodGet, path, err)
	}
	return header, nil
}

// getBody sends one GET carrying accept and returns the response headers and the
// body, reading at most maxDocumentBytes of it.
func (c *Client) getBody(ctx context.Context, path, accept string, walk *manifestWalk) (http.Header, []byte, error) {
	if err := walk.spend(); err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", http.MethodGet, path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return nil, nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil, fmt.Errorf("%s %s: %w", http.MethodGet, path, ErrNotFound)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, nil, &statusError{code: resp.StatusCode, method: http.MethodGet, path: path}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: read: %w", http.MethodGet, path, err)
	}
	if len(body) > maxDocumentBytes {
		return nil, nil, fmt.Errorf("%s %s: the body is larger than %d bytes", http.MethodGet, path, maxDocumentBytes)
	}
	return resp.Header, body, nil
}
