package registry

import (
	"context"
	"errors"
	"fmt"
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
