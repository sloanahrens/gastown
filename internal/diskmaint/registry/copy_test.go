package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
)

// copyFake is an in-memory registry for the copy tests. It answers the blob,
// upload and manifest routes the copy uses, and records what the copy asked of
// it: the blobs it read, the mounts it answered and the uploads it stored.
type copyFake struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]copyManifest
	// mountable holds the blobs a mount attempt answers 201 for: the registry
	// can mount them, so nothing is uploaded.
	mountable map[string][]byte
	// location, when set, is the Location an upload attempt answers with; a
	// noLocation fake answers 202 with none.
	location   string
	noLocation bool
	// manifestPutStatus, when set, is the status a manifest PUT answers, and the
	// manifest is not stored. forgetful answers every blob HEAD 404, as a registry
	// that drops an upload would. trustUploads stores an upload without checking
	// its bytes against the digest it is sent under.
	manifestPutStatus int
	forgetful         bool
	trustUploads      bool
	blobReads         []string
	mounts            int
	uploads           int
}

// methodFailingTransport answers the requests for method whose path contains
// pathPart with a transport error, after the first passFirst of them, and sends
// every other request to inner. seen counts the matching requests so far.
type methodFailingTransport struct {
	inner     http.RoundTripper
	method    string
	pathPart  string
	passFirst int
	seen      *int
}

// RoundTrip fails the matching requests past passFirst and passes the rest on.
func (t methodFailingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == t.method && strings.Contains(r.URL.Path, t.pathPart) {
		*t.seen++
		if *t.seen > t.passFirst {
			return nil, errors.New("connection refused")
		}
	}
	return t.inner.RoundTrip(r)
}

// failingClient returns a Client at base that reaches the fake, except that every
// request method sends to a path containing pathPart fails in transport.
func (f *copyFake) failingClient(base, method, pathPart string) *Client {
	return f.failingClientAfter(base, method, pathPart, 0)
}

// failingClientAfter is failingClient for a client whose first passFirst matching
// requests are answered: the later ones fail in transport.
func (f *copyFake) failingClientAfter(base, method, pathPart string, passFirst int) *Client {
	seen := 0
	return &Client{Base: base, HTTP: &http.Client{Transport: methodFailingTransport{inner: roundTrip(f.serve), method: method, pathPart: pathPart, passFirst: passFirst, seen: &seen}}}
}

// bodyDropTransport answers blob reads with the first bytes of the blob and then
// cause, as a connection dropped mid-blob does.
type bodyDropTransport struct {
	inner http.RoundTripper
	cause error
}

func (t bodyDropTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil || req.Method != http.MethodGet || !strings.Contains(req.URL.Path, "/blobs/") {
		return resp, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(io.MultiReader(strings.NewReader("lay"), iotest.ErrReader(t.cause)))
	return resp, nil
}

// TestCopyBlobToReportsADroppedSource: a source that drops the connection while
// the blob streams is refused with the cause in the error chain, and the
// destination holds no blob.
func TestCopyBlobToReportsADroppedSource(t *testing.T) {
	t.Parallel()
	layer := []byte("layer bytes")
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(layer)] = layer
	cause := errors.New("connection reset")
	source := src.client("http://src.test")
	source.HTTP.Transport = bodyDropTransport{inner: source.HTTP.Transport, cause: cause}

	err := source.CopyBlobTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "gastown-polecat", hashOf(layer))
	if !errors.Is(err, cause) {
		t.Fatalf("CopyBlobTo error = %v, want the dropped connection in its chain", err)
	}
	if dst.has(hashOf(layer)) {
		t.Fatal("the destination holds a blob after a source dropped mid-blob")
	}
}

// copyManifest is a manifest the fake holds: its media type and its bytes.
type copyManifest struct {
	contentType string
	body        []byte
}

// newCopyFake returns an empty fake registry.
func newCopyFake() *copyFake {
	return &copyFake{
		blobs:     map[string][]byte{},
		manifests: map[string]copyManifest{},
		mountable: map[string][]byte{},
	}
}

// client returns a Client that reaches the fake in process, at base.
func (f *copyFake) client(base string) *Client {
	return &Client{Base: base, HTTP: &http.Client{Transport: roundTrip(f.serve)}}
}

// pathSuffix is the part of path after the last marker, which is how the routes
// find the digest or reference a request names.
func pathSuffix(path, marker string) string {
	return path[strings.LastIndex(path, marker)+len(marker):]
}

// serve answers the registry routes the copy uses.
func (f *copyFake) serve(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := req.URL.Path
	switch {
	case req.Method == http.MethodHead && strings.Contains(path, "/blobs/"):
		if _, ok := f.blobs[pathSuffix(path, "/blobs/")]; !ok || f.forgetful {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	case req.Method == http.MethodGet && strings.Contains(path, "/blobs/"):
		digest := pathSuffix(path, "/blobs/")
		f.blobReads = append(f.blobReads, digest)
		body, ok := f.blobs[digest]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/blobs/uploads/"):
		digest := req.URL.Query().Get("mount")
		if body, ok := f.mountable[digest]; ok {
			f.blobs[digest] = body
			f.mounts++
			w.WriteHeader(http.StatusCreated)
			return
		}
		if f.noLocation {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		location := f.location
		if location == "" {
			location = path + "session"
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPut && strings.Contains(path, "/blobs/uploads/"):
		body, _ := io.ReadAll(req.Body)
		digest := req.URL.Query().Get("digest")
		if hashOf(body) != digest && !f.trustUploads {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.blobs[digest] = body
		f.uploads++
		w.WriteHeader(http.StatusCreated)
	case req.Method == http.MethodGet && strings.Contains(path, "/manifests/"):
		m, ok := f.manifests[pathSuffix(path, "/manifests/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", m.contentType)
		w.Header().Set("Docker-Content-Digest", hashOf(m.body))
		_, _ = w.Write(m.body)
	case req.Method == http.MethodPut && strings.Contains(path, "/manifests/"):
		if f.manifestPutStatus != 0 {
			w.WriteHeader(f.manifestPutStatus)
			return
		}
		body, _ := io.ReadAll(req.Body)
		f.manifests[pathSuffix(path, "/manifests/")] = copyManifest{contentType: req.Header.Get("Content-Type"), body: body}
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

// put stores body as the manifest ref names, with its blobs already in place.
func (f *copyFake) put(ref string, contentType string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifests[ref] = copyManifest{contentType: contentType, body: body}
}

// has reports whether the fake holds the blob digest.
func (f *copyFake) has(digest string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.blobs[digest]
	return ok
}

// reads returns the blob digests the copy read from the fake.
func (f *copyFake) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.blobReads...)
}

// hasManifest reports whether the fake holds a manifest under ref.
func (f *copyFake) hasManifest(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.manifests[ref]
	return ok
}

// copyImageBytes is an image manifest over one config and one layer blob.
func copyImageBytes(t *testing.T, config, layer []byte) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaTypeOCIManifest,
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": hashOf(config), "size": len(config)},
		"layers":        []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": hashOf(layer), "size": len(layer)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// copyIndexBytes is an index naming each child manifest for linux/arm64.
func copyIndexBytes(t *testing.T, children ...[]byte) []byte {
	t.Helper()
	var entries []any
	for _, c := range children {
		entries = append(entries, map[string]any{
			"mediaType": mediaTypeOCIManifest,
			"digest":    hashOf(c),
			"size":      len(c),
			"platform":  map[string]any{"os": "linux", "architecture": "arm64"},
		})
	}
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaTypeOCIIndex,
		"manifests":     entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestCopyManifestToCopiesBlobsAndLeavesTheTopTagUnset: an image's config and
// layer reach the destination, verified, and the top manifest is not stored
// under any tag there: the caller tags it after its blobs are in place.
func TestCopyManifestToCopiesBlobsAndLeavesTheTopTagUnset(t *testing.T) {
	t.Parallel()
	config, layer := []byte(`{"config":{}}`), []byte("layer bytes")
	manifest := copyImageBytes(t, config, layer)
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(config)] = config
	src.blobs[hashOf(layer)] = layer
	src.put("abc123", mediaTypeOCIManifest, manifest)

	got, err := src.client("http://src.test").CopyManifestTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "abc123", "gastown-polecat")
	if err != nil {
		t.Fatalf("CopyManifestTo: %v", err)
	}
	if got.Digest != hashOf(manifest) || string(got.Body) != string(manifest) {
		t.Fatalf("returned manifest %s, want the source's bytes %s", got.Digest, hashOf(manifest))
	}
	if !dst.has(hashOf(config)) || !dst.has(hashOf(layer)) {
		t.Fatal("the destination does not hold the config and layer blobs")
	}
	if dst.hasManifest("abc123") {
		t.Fatal("the top manifest was stored under its tag on the destination")
	}
}

// TestCopyBlobToMountsWhenTheRegistryCan: a blob the registry can mount from the
// source repository is mounted, so the copy reads nothing from the source and
// opens no upload.
func TestCopyBlobToMountsWhenTheRegistryCan(t *testing.T) {
	t.Parallel()
	layer := []byte("layer bytes")
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(layer)] = layer
	dst.mountable[hashOf(layer)] = layer

	if err := src.client("http://src.test").CopyBlobTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "gastown-polecat", hashOf(layer)); err != nil {
		t.Fatalf("CopyBlobTo: %v", err)
	}
	if !dst.has(hashOf(layer)) {
		t.Fatal("the mounted blob is not on the destination")
	}
	if got := src.reads(); len(got) != 0 {
		t.Fatalf("source reads = %v, want none when the mount answered", got)
	}
	if dst.uploads != 0 || dst.mounts != 1 {
		t.Fatalf("uploads = %d, mounts = %d, want no upload and one mount", dst.uploads, dst.mounts)
	}
}

// TestCopyBlobToSkipsABlobTheDestinationHolds: a blob already on the destination
// is not read from the source or sent again.
func TestCopyBlobToSkipsABlobTheDestinationHolds(t *testing.T) {
	t.Parallel()
	layer := []byte("layer bytes")
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(layer)] = layer
	dst.blobs[hashOf(layer)] = layer

	if err := src.client("http://src.test").CopyBlobTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "gastown-polecat", hashOf(layer)); err != nil {
		t.Fatalf("CopyBlobTo: %v", err)
	}
	if got := src.reads(); len(got) != 0 || dst.uploads != 0 || dst.mounts != 0 {
		t.Fatalf("source reads = %v, uploads = %d, mounts = %d; want none", got, dst.uploads, dst.mounts)
	}
}

// TestCopyBlobToStreamsWhenMountIsRefused: a registry that cannot mount answers
// 202 with an upload session, and the bytes are streamed into it.
func TestCopyBlobToStreamsWhenMountIsRefused(t *testing.T) {
	t.Parallel()
	layer := []byte("layer bytes")
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(layer)] = layer

	if err := src.client("http://src.test").CopyBlobTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "gastown-polecat", hashOf(layer)); err != nil {
		t.Fatalf("CopyBlobTo: %v", err)
	}
	if !dst.has(hashOf(layer)) {
		t.Fatal("the streamed blob is not on the destination")
	}
	if got := src.reads(); len(got) != 1 || got[0] != hashOf(layer) {
		t.Fatalf("source reads = %v, want the one blob", got)
	}
}

// TestCopyBlobToRefusesACopyThatIsNotTheDigest: a source whose bytes do not hash
// to the digest asked for is refused, and the destination holds no blob under it.
func TestCopyBlobToRefusesACopyThatIsNotTheDigest(t *testing.T) {
	t.Parallel()
	layer := []byte("layer bytes")
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(layer)] = []byte("tampered bytes")

	err := src.client("http://src.test").CopyBlobTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "gastown-polecat", hashOf(layer))
	if err == nil {
		t.Fatal("CopyBlobTo copied bytes that do not hash to the digest")
	}
	if dst.has(hashOf(layer)) {
		t.Fatal("the destination holds a blob under a digest its bytes do not match")
	}
}

// TestCopyBlobToRefusesASpoolThatCannotBeMade: a source whose bytes cannot be
// spooled is refused before any byte is sent, and the destination holds no blob.
func TestCopyBlobToRefusesASpoolThatCannotBeMade(t *testing.T) {
	t.Parallel()
	layer := []byte("layer bytes")
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(layer)] = layer
	// A regular file cannot be the spool directory, so the spool file cannot be made.
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	source := src.client("http://src.test")
	source.spoolDir = notADir

	err := source.CopyBlobTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "gastown-polecat", hashOf(layer))
	if err == nil {
		t.Fatal("CopyBlobTo returned no error for a spool that cannot be made")
	}
	if dst.has(hashOf(layer)) {
		t.Fatal("the destination holds a blob after a copy that could not be spooled")
	}
}

// TestCopyBlobToRefusesUnusableUploadAnswers: an upload the destination answers
// without a session, or with a location on another origin, is an error, and no
// bytes are sent to that other origin.
func TestCopyBlobToRefusesUnusableUploadAnswers(t *testing.T) {
	t.Parallel()
	layer := []byte("layer bytes")
	for _, tc := range []struct {
		name       string
		noLocation bool
		location   string
	}{
		{name: "no upload session", noLocation: true},
		{name: "location on another origin", location: "http://elsewhere.test/v2/gastown-polecat/blobs/uploads/session"},
		{name: "location does not parse", location: "http://%zz/session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src, dst := newCopyFake(), newCopyFake()
			src.blobs[hashOf(layer)] = layer
			dst.noLocation = tc.noLocation
			dst.location = tc.location

			err := src.client("http://src.test").CopyBlobTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "gastown-polecat", hashOf(layer))
			if err == nil {
				t.Fatal("CopyBlobTo returned no error for an unusable upload answer")
			}
			if dst.has(hashOf(layer)) {
				t.Fatal("the destination holds the blob after a refused upload")
			}
		})
	}
}

// TestCopyBlobToRefusesFailedRequests: a request that fails in transport, a base
// that is not a URL, a registry that drops the blob after the upload, and an
// upload whose bytes do not hash to the digest are each an error, and the
// destination holds no blob under the digest.
func TestCopyBlobToRefusesFailedRequests(t *testing.T) {
	t.Parallel()
	layer := []byte("layer bytes")
	for _, tc := range []struct {
		name string
		// src and dst build the two registries the copy runs between.
		src func(f *copyFake) *Client
		dst func(f *copyFake) *Client
		// setup adjusts the fakes before the copy.
		setup func(src, dst *copyFake)
		// keeps is whether the destination holds the bytes anyway: a registry that
		// stored an upload the copy then refused. The copy still returns an error.
		keeps bool
	}{
		{name: "destination base is not a URL",
			src: func(f *copyFake) *Client { return f.client("http://src.test") },
			dst: func(f *copyFake) *Client { return f.client("http://dst.test\x7f") }},
		{name: "source base is not a URL",
			src: func(f *copyFake) *Client { return f.client("http://src.test\x7f") },
			dst: func(f *copyFake) *Client { return f.client("http://dst.test") }},
		{name: "destination HEAD fails",
			src: func(f *copyFake) *Client { return f.client("http://src.test") },
			dst: func(f *copyFake) *Client { return f.failingClient("http://dst.test", http.MethodHead, "/blobs/") }},
		{name: "mount attempt fails",
			src: func(f *copyFake) *Client { return f.client("http://src.test") },
			dst: func(f *copyFake) *Client {
				return f.failingClient("http://dst.test", http.MethodPost, "/blobs/uploads/")
			}},
		{name: "source read fails",
			src: func(f *copyFake) *Client { return f.failingClient("http://src.test", http.MethodGet, "/blobs/") },
			dst: func(f *copyFake) *Client { return f.client("http://dst.test") }},
		{name: "upload fails",
			src: func(f *copyFake) *Client { return f.client("http://src.test") },
			dst: func(f *copyFake) *Client {
				return f.failingClient("http://dst.test", http.MethodPut, "/blobs/uploads/")
			}},
		{name: "destination check after the upload fails",
			src: func(f *copyFake) *Client { return f.client("http://src.test") },
			dst: func(f *copyFake) *Client {
				return f.failingClientAfter("http://dst.test", http.MethodHead, "/blobs/", 1)
			}, keeps: true},
		{name: "registry drops the blob after the upload",
			src:   func(f *copyFake) *Client { return f.client("http://src.test") },
			dst:   func(f *copyFake) *Client { return f.client("http://dst.test") },
			setup: func(_, dst *copyFake) { dst.forgetful = true }, keeps: true},
		{name: "upload bytes do not hash to the digest",
			src: func(f *copyFake) *Client { return f.client("http://src.test") },
			dst: func(f *copyFake) *Client { return f.client("http://dst.test") },
			setup: func(src, dst *copyFake) {
				src.blobs[hashOf(layer)] = []byte("tampered bytes")
				dst.trustUploads = true
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src, dst := newCopyFake(), newCopyFake()
			if tc.setup != nil {
				tc.setup(src, dst)
			}
			// The source holds the layer unless the case says the copy must fail
			// there; the tampered case set its own bytes above.
			if _, ok := src.blobs[hashOf(layer)]; !ok {
				src.blobs[hashOf(layer)] = layer
			}

			err := tc.src(src).CopyBlobTo(context.Background(), tc.dst(dst), "bot-registry/gastown-polecat", "gastown-polecat", hashOf(layer))
			if err == nil {
				t.Fatal("CopyBlobTo returned no error")
			}
			if got := dst.has(hashOf(layer)); got != tc.keeps {
				t.Fatalf("the destination holds the blob = %v after a refused copy, want %v", got, tc.keeps)
			}
		})
	}
}

// TestCopyBlobToRefusesBadNames: a repository or digest that fails validation is
// refused before any request is sent.
func TestCopyBlobToRefusesBadNames(t *testing.T) {
	t.Parallel()
	layer := []byte("layer bytes")
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(layer)] = layer
	for _, tc := range []struct {
		name, repo, dstRepo, digest string
	}{
		{name: "source repo", repo: "Bot/Registry", dstRepo: "gastown-polecat", digest: hashOf(layer)},
		{name: "destination repo", repo: "bot-registry/gastown-polecat", dstRepo: "../gastown-polecat", digest: hashOf(layer)},
		{name: "digest", repo: "bot-registry/gastown-polecat", dstRepo: "gastown-polecat", digest: "layer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := src.client("http://src.test").CopyBlobTo(context.Background(), dst.client("http://dst.test"), tc.repo, tc.dstRepo, tc.digest)
			if !errors.Is(err, ErrBadName) {
				t.Fatalf("CopyBlobTo error = %v, want ErrBadName", err)
			}
		})
	}
	if got := src.reads(); len(got) != 0 {
		t.Fatalf("source reads = %v, want none for a refused name", got)
	}
}

// TestCopyManifestToCopiesIndexChildren: an index's children are stored on the
// destination by digest, with their blobs, and the index itself is not tagged.
func TestCopyManifestToCopiesIndexChildren(t *testing.T) {
	t.Parallel()
	config, layer := []byte(`{"config":{}}`), []byte("layer bytes")
	child := copyImageBytes(t, config, layer)
	index := copyIndexBytes(t, child)
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(config)] = config
	src.blobs[hashOf(layer)] = layer
	src.put(hashOf(child), mediaTypeOCIManifest, child)
	src.put("multi", mediaTypeOCIIndex, index)

	if _, err := src.client("http://src.test").CopyManifestTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "multi", "gastown-polecat"); err != nil {
		t.Fatalf("CopyManifestTo: %v", err)
	}
	if !dst.hasManifest(hashOf(child)) {
		t.Fatal("the index's child manifest is not stored by digest on the destination")
	}
	if dst.hasManifest("multi") {
		t.Fatal("the index was stored under its tag on the destination")
	}
	if !dst.has(hashOf(config)) || !dst.has(hashOf(layer)) {
		t.Fatal("the child's blobs are not on the destination")
	}
}

// TestCopyManifestToRefusesDeepIndexes: an index nested deeper than the walk
// allows is refused rather than followed.
func TestCopyManifestToRefusesDeepIndexes(t *testing.T) {
	t.Parallel()
	config, layer := []byte(`{"config":{}}`), []byte("layer bytes")
	src, dst := newCopyFake(), newCopyFake()
	src.blobs[hashOf(config)] = config
	src.blobs[hashOf(layer)] = layer
	// The innermost manifest is an image; each index above it nests one level more.
	inner := copyImageBytes(t, config, layer)
	src.put(hashOf(inner), mediaTypeOCIManifest, inner)
	ref := hashOf(inner)
	for range maxIndexDepth + 1 {
		index := copyIndexBytes(t, mustFindManifest(t, src, ref))
		src.put(hashOf(index), mediaTypeOCIIndex, index)
		ref = hashOf(index)
	}

	if _, err := src.client("http://src.test").CopyManifestTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", ref, "gastown-polecat"); err == nil {
		t.Fatal("CopyManifestTo followed an index nested past the walk's depth")
	}
}

// mustFindManifest returns the bytes of the manifest the fake holds under ref, as
// a child for the next index up.
func mustFindManifest(t *testing.T, f *copyFake, ref string) []byte {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.manifests[ref]
	if !ok {
		t.Fatalf("the fake holds no manifest %s", ref)
	}
	return m.body
}

// TestCopyManifestToRefusesBadDestination: a destination repository that fails
// validation is refused before the source is read.
func TestCopyManifestToRefusesBadDestination(t *testing.T) {
	t.Parallel()
	src, dst := newCopyFake(), newCopyFake()
	_, err := src.client("http://src.test").CopyManifestTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", "abc123", "Gastown/Polecat")
	if !errors.Is(err, ErrBadName) {
		t.Fatalf("CopyManifestTo error = %v, want ErrBadName", err)
	}
	if got := src.reads(); len(got) != 0 {
		t.Fatalf("source reads = %v, want none", got)
	}
}

// TestCopyManifestToRefusesUnreadableSource: a source manifest that does not
// decode, an index child that names no digest, and a blob the source does not
// hold are each refused, and nothing is tagged on the destination.
func TestCopyManifestToRefusesUnreadableSource(t *testing.T) {
	t.Parallel()
	config, layer := []byte(`{"config":{}}`), []byte("layer bytes")
	child := copyImageBytes(t, config, layer)
	// index names child, and holds the child manifest and its blobs on src unless
	// the case leaves one out.
	index := copyIndexBytes(t, child)
	for _, tc := range []struct {
		name  string
		ref   string
		setup func(src, dst *copyFake)
	}{
		{name: "manifest does not decode", ref: "abc123", setup: func(src, _ *copyFake) {
			src.put("abc123", mediaTypeOCIManifest, []byte("not json"))
		}},
		{name: "index child names no digest", ref: "multi", setup: func(src, _ *copyFake) {
			src.put("multi", mediaTypeOCIIndex, []byte(`{"mediaType":"`+mediaTypeOCIIndex+`","manifests":[{"digest":"layer"}]}`))
		}},
		{name: "source lacks a blob", ref: "abc123", setup: func(src, _ *copyFake) {
			src.put("abc123", mediaTypeOCIManifest, copyImageBytes(t, config, layer))
			src.blobs[hashOf(config)] = config
		}},
		{name: "top manifest is missing", ref: "nope", setup: func(_, _ *copyFake) {}},
		{name: "index child is missing", ref: "multi", setup: func(src, _ *copyFake) {
			src.put("multi", mediaTypeOCIIndex, index)
		}},
		{name: "child blob is missing", ref: "multi", setup: func(src, _ *copyFake) {
			src.put("multi", mediaTypeOCIIndex, index)
			src.put(hashOf(child), mediaTypeOCIManifest, child)
			src.blobs[hashOf(config)] = config
		}},
		{name: "destination refuses the child manifest", ref: "multi", setup: func(src, dst *copyFake) {
			src.put("multi", mediaTypeOCIIndex, index)
			src.put(hashOf(child), mediaTypeOCIManifest, child)
			src.blobs[hashOf(config)] = config
			src.blobs[hashOf(layer)] = layer
			dst.manifestPutStatus = http.StatusInternalServerError
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src, dst := newCopyFake(), newCopyFake()
			tc.setup(src, dst)

			_, err := src.client("http://src.test").CopyManifestTo(context.Background(), dst.client("http://dst.test"), "bot-registry/gastown-polecat", tc.ref, "gastown-polecat")
			if err == nil {
				t.Fatal("CopyManifestTo returned no error for an unreadable source")
			}
			if dst.hasManifest(tc.ref) {
				t.Fatal("the destination holds a manifest under the tag after a refused copy")
			}
		})
	}
}
