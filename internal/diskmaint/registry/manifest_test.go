package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func digestOf(b byte) string { return "sha256:" + repeatByte(b, 64) }

// imageBody renders an image manifest that names configDigest and layers of
// the given sizes.
func imageBody(configDigest string, configSize int64, layerSizes ...int64) string {
	layers := make([]string, len(layerSizes))
	for i, n := range layerSizes {
		layers[i] = fmt.Sprintf(`{"size":%d}`, n)
	}
	return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{"digest":%q,"size":%d},"layers":[%s]}`, configDigest, configSize, strings.Join(layers, ","))
}

// layer is one layer an image body names: its digest and size.
type layer struct {
	Digest string
	Size   int64
}

// imageBodyWithLayers renders an image manifest whose layers carry digests, so
// a test can name one layer from two children.
func imageBodyWithLayers(configDigest string, configSize int64, layers ...layer) string {
	named := make([]string, len(layers))
	for i, l := range layers {
		named[i] = fmt.Sprintf(`{"digest":%q,"size":%d}`, l.Digest, l.Size)
	}
	return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{"digest":%q,"size":%d},"layers":[%s]}`, configDigest, configSize, strings.Join(named, ","))
}

// indexBody renders an index that names each child digest.
func indexBody(childDigests ...string) string {
	children := make([]string, len(childDigests))
	for i, d := range childDigests {
		children[i] = fmt.Sprintf(`{"digest":%q,"platform":{"architecture":"amd64","os":"linux"}}`, d)
	}
	return fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json",
		"manifests":[%s]}`, strings.Join(children, ","))
}

// blobBody renders an image config blob whose created field is set when
// created is non-empty.
func blobBody(created string) string {
	if created == "" {
		return `{"architecture":"amd64","os":"linux"}`
	}
	return fmt.Sprintf(`{"created":%q,"architecture":"amd64","os":"linux"}`, created)
}

func served(w http.ResponseWriter, digest, body string) {
	w.Header().Set("Docker-Content-Digest", digest)
	_, _ = w.Write([]byte(body))
}

func TestGetManifestImage(t *testing.T) {
	t.Parallel()
	manifestDigest := digestOf('a')
	configDigest := digestOf('c')
	var blobPath string
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/gastown-polecat/manifests/dev":
			served(w, manifestDigest, imageBody(configDigest, 1000, 2048, 512))
		case "/v2/gastown-polecat/blobs/" + configDigest:
			blobPath = r.URL.Path
			_, _ = w.Write([]byte(blobBody("2026-10-27T10:00:00Z")))
		default:
			http.NotFound(w, r)
		}
	})

	m, err := c.GetManifest(context.Background(), "gastown-polecat", "dev")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if m.Digest != manifestDigest {
		t.Errorf("Digest = %q, want %q from Docker-Content-Digest", m.Digest, manifestDigest)
	}
	if want := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC); !m.Created.Equal(want) {
		t.Errorf("Created = %v, want %v from the config blob", m.Created, want)
	}
	if want := int64(1000 + 2048 + 512); m.Bytes != want {
		t.Errorf("Bytes = %d, want %d (config + layers)", m.Bytes, want)
	}
	if blobPath != "/v2/gastown-polecat/blobs/"+configDigest {
		t.Errorf("config blob path = %q, want the config digest path", blobPath)
	}
}

func TestGetManifestIndexTakesNewestChild(t *testing.T) {
	t.Parallel()
	indexDigest := digestOf('d')
	oldChild, newChild := digestOf('1'), digestOf('2')
	oldConfig, newConfig := digestOf('3'), digestOf('4')
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/gastown-polecat/manifests/" + indexDigest:
			served(w, indexDigest, indexBody(oldChild, newChild))
		case "/v2/gastown-polecat/manifests/" + oldChild:
			served(w, oldChild, imageBody(oldConfig, 100, 200))
		case "/v2/gastown-polecat/manifests/" + newChild:
			served(w, newChild, imageBody(newConfig, 300, 400))
		case "/v2/gastown-polecat/blobs/" + oldConfig:
			_, _ = w.Write([]byte(blobBody("2026-10-20T09:00:00Z")))
		case "/v2/gastown-polecat/blobs/" + newConfig:
			_, _ = w.Write([]byte(blobBody("2026-10-27T10:00:00Z")))
		default:
			http.NotFound(w, r)
		}
	})

	m, err := c.GetManifest(context.Background(), "gastown-polecat", indexDigest)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if m.Digest != indexDigest {
		t.Errorf("Digest = %q, want the index digest %q", m.Digest, indexDigest)
	}
	if want := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC); !m.Created.Equal(want) {
		t.Errorf("Created = %v, want the newest child's %v", m.Created, want)
	}
	if want := int64(100 + 200 + 300 + 400); m.Bytes != want {
		t.Errorf("Bytes = %d, want %d (every child's config and layers)", m.Bytes, want)
	}
}

func TestGetManifestIndexCountsASharedBlobOnce(t *testing.T) {
	t.Parallel()
	indexDigest := digestOf('d')
	firstChild, secondChild := digestOf('1'), digestOf('2')
	firstConfig, secondConfig := digestOf('3'), digestOf('4')
	sharedLayer, ownLayer := digestOf('5'), digestOf('6')
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/gastown-polecat/manifests/" + indexDigest:
			served(w, indexDigest, indexBody(firstChild, secondChild))
		case "/v2/gastown-polecat/manifests/" + firstChild:
			served(w, firstChild, imageBodyWithLayers(firstConfig, 100, layer{sharedLayer, 500}))
		case "/v2/gastown-polecat/manifests/" + secondChild:
			served(w, secondChild, imageBodyWithLayers(secondConfig, 200,
				layer{sharedLayer, 500}, layer{ownLayer, 40}))
		case "/v2/gastown-polecat/blobs/" + firstConfig:
			_, _ = w.Write([]byte(blobBody("2026-10-20T09:00:00Z")))
		case "/v2/gastown-polecat/blobs/" + secondConfig:
			_, _ = w.Write([]byte(blobBody("2026-10-27T10:00:00Z")))
		default:
			http.NotFound(w, r)
		}
	})

	m, err := c.GetManifest(context.Background(), "gastown-polecat", indexDigest)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if want := int64(100 + 200 + 500 + 40); m.Bytes != want {
		t.Errorf("Bytes = %d, want %d: the layer both children name counts once", m.Bytes, want)
	}
	if want := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC); !m.Created.Equal(want) {
		t.Errorf("Created = %v, want the newest child's %v", m.Created, want)
	}
}

// paddedBody renders body with a padding member so the whole document is
// exactly size bytes, letting a test sit a document on a byte limit.
func paddedBody(t *testing.T, body string, size int) string {
	t.Helper()
	if !strings.HasSuffix(body, "}") {
		t.Fatalf("body does not end with }: %q", body)
	}
	prefix := strings.TrimSuffix(body, "}")
	const member = `,"padding":""}`
	// The padding string takes whatever the fixed part leaves, and member already
	// holds the closing quote and brace.
	fixed := len(prefix) + len(member)
	if size < fixed {
		t.Fatalf("size %d is shorter than the document's fixed length %d", size, fixed)
	}
	return prefix + `,"padding":"` + strings.Repeat("a", size-fixed) + `"}`
}

func TestGetManifestBoundsTheResponseBody(t *testing.T) {
	t.Parallel()
	manifestDigest, configDigest := digestOf('a'), digestOf('c')
	cases := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"a body at the limit is read", maxDocumentBytes, false},
		{"a body one byte over the limit is refused", maxDocumentBytes + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/gastown-polecat/manifests/dev":
					served(w, manifestDigest, paddedBody(t, imageBody(configDigest, 1), tc.size))
				case "/v2/gastown-polecat/blobs/" + configDigest:
					_, _ = w.Write([]byte(blobBody("2026-10-27T10:00:00Z")))
				default:
					http.NotFound(w, r)
				}
			})

			m, err := c.GetManifest(context.Background(), "gastown-polecat", "dev")
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("GetManifest with a %d-byte body: %v", tc.size, err)
				}
				if m.Bytes != 1 {
					t.Errorf("Bytes = %d, want 1 from the config", m.Bytes)
				}
				return
			}
			if err == nil {
				t.Fatalf("GetManifest read a %d-byte body: %+v, want a refusal", tc.size, m)
			}
			if want := fmt.Sprint(maxDocumentBytes); !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to name the %s-byte limit", err, want)
			}
		})
	}
}

func TestGetManifestIndexWalkHasARequestBudget(t *testing.T) {
	t.Parallel()
	// Each child costs a manifest and a config blob request, so this index wants
	// far more requests than the budget allows.
	indexDigest := fmt.Sprintf("sha256:%064x", 0)
	configDigest := digestOf('f')
	children := make([]string, 2*maxIndexRequests)
	for i := range children {
		children[i] = fmt.Sprintf("sha256:%064x", i+1)
	}
	handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/gastown-polecat/manifests/"):
			ref := strings.TrimPrefix(r.URL.Path, "/v2/gastown-polecat/manifests/")
			if ref == indexDigest {
				served(w, indexDigest, indexBody(children...))
				return
			}
			served(w, ref, imageBody(configDigest, 10))
		case strings.HasPrefix(r.URL.Path, "/v2/gastown-polecat/blobs/"):
			_, _ = w.Write([]byte(blobBody("2026-10-27T10:00:00Z")))
		default:
			http.NotFound(w, r)
		}
	})
	c := newTestClient(handler)

	m, err := c.GetManifest(context.Background(), "gastown-polecat", indexDigest)
	if err == nil {
		t.Fatalf("GetManifest returned %+v for an index of %d children, want a refusal", m, len(children))
	}
	if want := fmt.Sprint(maxIndexRequests); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to name the %s-request budget", err, want)
	}
	if n := requests.Load(); n > maxIndexRequests {
		t.Errorf("server saw %d requests, want at most the budget of %d", n, maxIndexRequests)
	}
}

func TestGetManifestSendsAcceptHeaders(t *testing.T) {
	t.Parallel()
	manifestDigest, configDigest := digestOf('a'), digestOf('c')
	var accept string
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/gastown-polecat/manifests/dev":
			accept = r.Header.Get("Accept")
			served(w, manifestDigest, imageBody(configDigest, 1))
		case "/v2/gastown-polecat/blobs/" + configDigest:
			_, _ = w.Write([]byte(blobBody("2026-10-27T10:00:00Z")))
		default:
			http.NotFound(w, r)
		}
	})

	if _, err := c.GetManifest(context.Background(), "gastown-polecat", "dev"); err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	for _, mediaType := range []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	} {
		if !strings.Contains(accept, mediaType) {
			t.Errorf("Accept = %q, missing %q", accept, mediaType)
		}
	}
}

func TestGetManifestRejectsBadRefBeforeRequest(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, repo, ref string }{
		{"bad repo", "../x", "dev"},
		{"traversal ref", "gastown-polecat", "../x"},
		{"space in ref", "gastown-polecat", "a b"},
		{"empty ref", "gastown-polecat", ""},
		{"digest with a bad hex length", "gastown-polecat", "sha256:" + repeatByte('a', 63)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {})
			c := newTestClient(handler)

			_, err := c.GetManifest(context.Background(), tc.repo, tc.ref)
			if !errors.Is(err, ErrBadName) {
				t.Fatalf("GetManifest(%q, %q) error = %v, want ErrBadName", tc.repo, tc.ref, err)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("server saw %d requests, want 0", n)
			}
		})
	}
}

func TestGetManifestMissingCreatedIsError(t *testing.T) {
	t.Parallel()
	manifestDigest, configDigest := digestOf('a'), digestOf('c')
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/gastown-polecat/manifests/dev":
			served(w, manifestDigest, imageBody(configDigest, 1))
		case "/v2/gastown-polecat/blobs/" + configDigest:
			_, _ = w.Write([]byte(blobBody("")))
		default:
			http.NotFound(w, r)
		}
	})

	m, err := c.GetManifest(context.Background(), "gastown-polecat", "dev")
	if err == nil {
		t.Fatalf("GetManifest with no created returned %+v, want an error", m)
	}
	if !m.Created.IsZero() {
		t.Errorf("Created = %v on error, want the zero time", m.Created)
	}
}

// brokenBody is a response body whose Read fails, the way a connection that
// dies mid-body does.
type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (brokenBody) Close() error             { return nil }

// bodyTransport answers every request with body, so a test can fail a read that
// no handler can.
type bodyTransport struct{ body io.ReadCloser }

func (t bodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: t.body}, nil
}

func TestGetManifestBodyReadFailureIsReturned(t *testing.T) {
	t.Parallel()
	c := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: bodyTransport{body: brokenBody{}}}}

	_, err := c.GetManifest(context.Background(), "gastown-polecat", "dev")
	if err == nil {
		t.Fatal("GetManifest on a failing body returned no error, want the read error")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error = %v, want it to carry the read failure", err)
	}
}

func TestGetManifest404IsErrNotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	_, err := c.GetManifest(context.Background(), "gastown-polecat", "dev")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetManifest error = %v, want ErrNotFound", err)
	}
}

// TestGetManifestErrorBranches drives every refusal path GetManifest can take,
// so a failure is never read as a manifest with unknown values.
func TestGetManifestErrorBranches(t *testing.T) {
	t.Parallel()
	imageDigest, configDigest := digestOf('a'), digestOf('c')
	childDigest := digestOf('b')

	serve := func(routes map[string]func(http.ResponseWriter, *http.Request)) *Client {
		return newTestClient(func(w http.ResponseWriter, r *http.Request) {
			if route, ok := routes[r.URL.Path]; ok {
				route(w, r)
				return
			}
			http.NotFound(w, r)
		})
	}
	body := func(digest, text string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) { served(w, digest, text) }
	}

	cases := []struct {
		name     string
		client   *Client
		ref      string
		want     error  // sentinel the error must wrap
		wantText string // text the error must carry
	}{
		{
			name: "index nests past the depth bound",
			client: serve(map[string]func(http.ResponseWriter, *http.Request){
				// An index that names itself is the shortest loop a registry can make.
				"/v2/gastown-polecat/manifests/" + childDigest: body(childDigest, indexBody(childDigest)),
			}),
			ref:      childDigest,
			wantText: "nests deeper",
		},
		{
			name: "response carries no digest header",
			client: serve(map[string]func(http.ResponseWriter, *http.Request){
				"/v2/gastown-polecat/manifests/dev": func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(imageBody(configDigest, 1)))
				},
			}),
			ref:      "dev",
			wantText: "Docker-Content-Digest",
		},
		{
			name: "config digest fails validation",
			client: serve(map[string]func(http.ResponseWriter, *http.Request){
				"/v2/gastown-polecat/manifests/dev": body(imageDigest, imageBody("sha256:not-a-digest", 1)),
			}),
			ref:      "dev",
			wantText: "config digest fails validation",
		},
		{
			name: "index has no children",
			client: serve(map[string]func(http.ResponseWriter, *http.Request){
				"/v2/gastown-polecat/manifests/dev": body(imageDigest, indexBody()),
			}),
			ref:      "dev",
			wantText: "no child manifests",
		},
		{
			name: "index child digest fails validation",
			client: serve(map[string]func(http.ResponseWriter, *http.Request){
				"/v2/gastown-polecat/manifests/dev": body(imageDigest, indexBody("sha256:not-a-digest")),
			}),
			ref:      "dev",
			wantText: "child digest fails validation",
		},
		{
			name: "index child is missing",
			client: serve(map[string]func(http.ResponseWriter, *http.Request){
				"/v2/gastown-polecat/manifests/dev": body(imageDigest, indexBody(childDigest)),
			}),
			ref:  "dev",
			want: ErrNotFound,
		},
		{
			name: "config blob is missing",
			client: serve(map[string]func(http.ResponseWriter, *http.Request){
				"/v2/gastown-polecat/manifests/dev": body(imageDigest, imageBody(configDigest, 1)),
			}),
			ref:  "dev",
			want: ErrNotFound,
		},
		{
			name: "created is not an RFC3339 time",
			client: serve(map[string]func(http.ResponseWriter, *http.Request){
				"/v2/gastown-polecat/manifests/dev": body(imageDigest, imageBody(configDigest, 1)),
				"/v2/gastown-polecat/blobs/" + configDigest: func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(blobBody("yesterday")))
				},
			}),
			ref:      "dev",
			wantText: "RFC3339",
		},
		{
			name: "manifest body is not JSON",
			client: serve(map[string]func(http.ResponseWriter, *http.Request){
				"/v2/gastown-polecat/manifests/dev": body(imageDigest, "not json"),
			}),
			ref:      "dev",
			wantText: "decode",
		},
		{
			name:   "base cannot form a URL",
			client: &Client{Base: "http://registry.test/\nbad", HTTP: &http.Client{Transport: roundTrip(func(http.ResponseWriter, *http.Request) {})}},
			ref:    "dev",
			// No handler runs: NewRequestWithContext refuses the URL.
		},
		{
			name:     "transport error is returned",
			client:   &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: failingTransport{err: errors.New("connection refused")}}},
			ref:      "dev",
			wantText: "connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := tc.client.GetManifest(context.Background(), "gastown-polecat", tc.ref)
			if err == nil {
				t.Fatalf("GetManifest returned %+v, want an error", m)
			}
			if !m.Created.IsZero() || m.Digest != "" {
				t.Errorf("manifest = %+v on error, want the zero value", m)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.want)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantText)
			}
		})
	}
}
