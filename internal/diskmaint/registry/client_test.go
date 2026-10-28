package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// roundTrip serves requests from handler in process: no listener, no network.
type roundTrip http.HandlerFunc

func (h roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	http.HandlerFunc(h).ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// newTestClient returns a Client whose requests handler answers in process.
func newTestClient(handler http.HandlerFunc) *Client {
	return &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: roundTrip(handler)}}
}

func testDigest() string { return "sha256:" + repeatByte('a', 64) }

// countingHandler answers with handler and counts every request that reaches it.
func countingHandler(handler http.HandlerFunc) (http.HandlerFunc, *atomic.Int32) {
	var requests atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler(w, r)
	}, &requests
}

func TestCatalogAndTags(t *testing.T) {
	t.Parallel()
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/_catalog":
			_, _ = w.Write([]byte(`{"repositories":["gastown-polecat","seedtest/polecat"]}`))
		case "/v2/gastown-polecat/tags/list":
			_, _ = w.Write([]byte(`{"name":"gastown-polecat","tags":["dev","2d5a48dcb"]}`))
		case "/v2/seedtest/polecat/tags/list":
			_, _ = w.Write([]byte(`{"name":"seedtest/polecat","tags":null}`))
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()

	repos, err := c.Catalog(ctx)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(repos) != 2 || repos[0] != "gastown-polecat" || repos[1] != "seedtest/polecat" {
		t.Errorf("Catalog = %v, want [gastown-polecat seedtest/polecat]", repos)
	}

	tags, err := c.Tags(ctx, "gastown-polecat")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 2 || tags[0] != "dev" || tags[1] != "2d5a48dcb" {
		t.Errorf("Tags = %v, want [dev 2d5a48dcb]", tags)
	}

	empty, err := c.Tags(ctx, "seedtest/polecat")
	if err != nil {
		t.Fatalf("Tags null: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("Tags with null = %v, want empty non-nil slice", empty)
	}
}

func TestTagsRejectsBadRepoBeforeRequest(t *testing.T) {
	t.Parallel()
	handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {})
	c := newTestClient(handler)

	_, err := c.Tags(context.Background(), "../x")
	if !errors.Is(err, ErrBadName) {
		t.Fatalf("Tags error = %v, want ErrBadName", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
}

func TestDeleteManifestAccepted(t *testing.T) {
	t.Parallel()
	var method, path string
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	})

	if err := c.DeleteManifest(context.Background(), "gastown-polecat", testDigest()); err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", method)
	}
	if path != "/v2/gastown-polecat/manifests/"+testDigest() {
		t.Errorf("path = %q, want the manifest path", path)
	}
}

func TestDeleteManifest405MeansDisabled(t *testing.T) {
	t.Parallel()
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	})

	err := c.DeleteManifest(context.Background(), "gastown-polecat", testDigest())
	if !errors.Is(err, ErrDeletesDisabled) {
		t.Fatalf("error = %v, want ErrDeletesDisabled", err)
	}
}

func TestDeleteManifest404IsErrNotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	err := c.DeleteManifest(context.Background(), "gastown-polecat", testDigest())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestDeleteManifestRejectsBadDigest(t *testing.T) {
	t.Parallel()
	handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {})
	c := newTestClient(handler)

	err := c.DeleteManifest(context.Background(), "gastown-polecat", "sha256:XYZ")
	if !errors.Is(err, ErrBadName) {
		t.Fatalf("error = %v, want ErrBadName", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
}

func TestDeleteManifestRejectsBadRepo(t *testing.T) {
	t.Parallel()
	handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {})
	c := newTestClient(handler)

	err := c.DeleteManifest(context.Background(), "../x", testDigest())
	if !errors.Is(err, ErrBadName) {
		t.Fatalf("error = %v, want ErrBadName", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
}

func TestClientErrorsOnOtherStatuses(t *testing.T) {
	t.Parallel()
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	ctx := context.Background()

	if _, err := c.Catalog(ctx); err == nil {
		t.Error("Catalog on 500 returned no error")
	}
	if _, err := c.Tags(ctx, "gastown-polecat"); err == nil {
		t.Error("Tags on 500 returned no error")
	}
	err := c.DeleteManifest(ctx, "gastown-polecat", testDigest())
	if err == nil {
		t.Error("DeleteManifest on 500 returned no error")
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrDeletesDisabled) || errors.Is(err, ErrBadName) {
		t.Errorf("500 mapped to a sentinel: %v", err)
	}
	if strings.Contains(err.Error(), "boom") {
		t.Errorf("error echoes the response body: %v", err)
	}
}
