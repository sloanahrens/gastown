package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// failingTransport fails every request with err, the way an unreachable
// registry does.
type failingTransport struct{ err error }

func (t failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

// stalledTransport answers nothing: it waits for the request to be cancelled
// and reports the cancellation, the way a registry that accepts a connection
// and never answers does. limit bounds the wait, so a request that is never
// cancelled fails instead of hanging the test.
type stalledTransport struct{ limit time.Duration }

func (t stalledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	limit, cancel := context.WithTimeout(r.Context(), t.limit)
	defer cancel()
	select {
	case <-r.Context().Done():
	case <-limit.Done():
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("the request was never cancelled")
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

func TestCatalogRejectsUnbuildableRequest(t *testing.T) {
	t.Parallel()
	handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {})
	c := &Client{Base: "http://registry.test/\nbad", HTTP: &http.Client{Transport: roundTrip(handler)}}

	if _, err := c.Catalog(context.Background()); err == nil {
		t.Fatal("Catalog with a base that cannot form a URL returned no error")
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("transport saw %d requests, want 0", n)
	}
}

func TestCatalogTransportErrorIsReturned(t *testing.T) {
	t.Parallel()
	refused := errors.New("connection refused")
	c := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: failingTransport{err: refused}}}

	_, err := c.Catalog(context.Background())
	if !errors.Is(err, refused) {
		t.Fatalf("Catalog error = %v, want the transport error", err)
	}
}

func TestCatalogMalformedBodyIsError(t *testing.T) {
	t.Parallel()
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})

	if _, err := c.Catalog(context.Background()); err == nil {
		t.Fatal("Catalog with a malformed body returned no error")
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

func TestCatalogFollowsPagination(t *testing.T) {
	t.Parallel()
	var uris []string
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.URL.RequestURI())
		switch r.URL.Query().Get("last") {
		case "":
			w.Header().Set("Link", `</v2/_catalog?n=100&last=gastown-polecat>; rel="next"`)
			_, _ = w.Write([]byte(`{"repositories":["gastown-polecat"]}`))
		case "gastown-polecat":
			w.Header().Set("Link", `</v2/_catalog?n=100&last=seedtest/polecat>; rel="next"`)
			_, _ = w.Write([]byte(`{"repositories":["seedtest/polecat"]}`))
		default:
			_, _ = w.Write([]byte(`{"repositories":["zoo"]}`))
		}
	})

	repos, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if want := []string{"gastown-polecat", "seedtest/polecat", "zoo"}; !slices.Equal(repos, want) {
		t.Errorf("Catalog = %v, want %v", repos, want)
	}
	want := []string{
		"/v2/_catalog?n=100",
		"/v2/_catalog?n=100&last=gastown-polecat",
		"/v2/_catalog?n=100&last=seedtest/polecat",
	}
	if !slices.Equal(uris, want) {
		t.Errorf("registry read %v, want %v", uris, want)
	}
}

func TestTagsFollowPagination(t *testing.T) {
	t.Parallel()
	var uris []string
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.URL.RequestURI())
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/gastown-polecat/tags/list?n=100&last=dev>; rel="next"`)
			_, _ = w.Write([]byte(`{"name":"gastown-polecat","tags":["dev"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"gastown-polecat","tags":["2d5a48dcb"]}`))
	})

	tags, err := c.Tags(context.Background(), "gastown-polecat")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if want := []string{"dev", "2d5a48dcb"}; !slices.Equal(tags, want) {
		t.Errorf("Tags = %v, want %v", tags, want)
	}
	if len(uris) != 2 || !strings.HasPrefix(uris[0], "/v2/gastown-polecat/tags/list?n=") {
		t.Errorf("registry read %v, want two tag pages", uris)
	}
}

func TestCatalogFollowsAbsoluteLinkOnOwnOrigin(t *testing.T) {
	t.Parallel()
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `<http://registry.test/v2/_catalog?n=100&last=a>; rel="next"`)
			_, _ = w.Write([]byte(`{"repositories":["a"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"repositories":["b"]}`))
	})

	repos, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if want := []string{"a", "b"}; !slices.Equal(repos, want) {
		t.Errorf("Catalog = %v, want %v", repos, want)
	}
}

func TestCatalogRefusesUnusableNextLink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, link string
	}{
		{"another origin", `<http://elsewhere.test/v2/_catalog?n=100&last=evil>; rel="next"`},
		{"not a path", `<v2/_catalog?n=100>; rel="next"`},
		{"unparseable", `<http:// registry.test/v2/_catalog>; rel="next"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The page the bad link points at ends the listing cleanly, so a
			// client that follows it returns no error instead of being caught
			// by the loop guards.
			handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("last") == "evil" {
					_, _ = w.Write([]byte(`{"repositories":["evil"]}`))
					return
				}
				w.Header().Set("Link", tc.link)
				_, _ = w.Write([]byte(`{"repositories":["a"]}`))
			})

			if _, err := newTestClient(handler).Catalog(context.Background()); err == nil {
				t.Fatalf("Catalog with a %s link returned no error", tc.name)
			}
			if n := requests.Load(); n != 1 {
				t.Errorf("registry saw %d requests, want 1", n)
			}
		})
	}
}

func TestCatalogRefusesRepeatedNextLink(t *testing.T) {
	t.Parallel()
	handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `</v2/_catalog?n=100>; rel="next"`)
		_, _ = w.Write([]byte(`{"repositories":["a"]}`))
	})

	if _, err := newTestClient(handler).Catalog(context.Background()); err == nil {
		t.Fatal("Catalog whose Link header names its own page returned no error")
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("registry read %d pages, want 1", n)
	}
}

func TestCatalogStopsAtThePageBound(t *testing.T) {
	t.Parallel()
	const bound = 3
	var pages atomic.Int32
	c := &Client{
		Base:      "http://registry.test",
		pageLimit: bound,
		HTTP: &http.Client{Transport: roundTrip(func(w http.ResponseWriter, r *http.Request) {
			n := pages.Add(1)
			w.Header().Set("Link", fmt.Sprintf(`</v2/_catalog?n=100&last=repo-%d>; rel="next"`, n))
			_, _ = w.Write([]byte(`{"repositories":["repo"]}`))
		})},
	}

	if _, err := c.Catalog(context.Background()); err == nil {
		t.Fatal("Catalog read past the page bound without an error")
	}
	if got := pages.Load(); got != bound {
		t.Errorf("registry saw %d requests, want %d", got, bound)
	}
}

func TestCatalogBoundsTheWholeListing(t *testing.T) {
	t.Parallel()
	page := func(entries int) http.HandlerFunc {
		names := make([]string, entries)
		for i := range names {
			names[i] = `"r"`
		}
		body := `{"repositories":[` + strings.Join(names, ",") + `]}`
		return func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }
	}
	tests := []struct {
		name    string
		entries int
		wantErr bool
	}{
		{"one page at the bound", pageSize, false},
		{"one page past the bound", pageSize + 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// One page is the whole listing here, so the listing may hold one
			// page's worth of entries and not one more.
			c := &Client{
				Base:      "http://registry.test",
				pageLimit: 1,
				HTTP:      &http.Client{Transport: roundTrip(page(tc.entries))},
			}

			repos, err := c.Catalog(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Catalog read a listing of %d entries, past the page bound", tc.entries)
				}
				return
			}
			if err != nil {
				t.Fatalf("Catalog: %v", err)
			}
			if len(repos) != tc.entries {
				t.Errorf("Catalog = %d entries, want %d", len(repos), tc.entries)
			}
		})
	}
}

func TestCatalogKeepsTheBasePathPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, link, wantSecond string
	}{
		{
			"link without the prefix",
			`</v2/_catalog?n=100&last=a>; rel="next"`,
			"/prefix/v2/_catalog?n=100&last=a",
		},
		{
			"link with the prefix",
			`</prefix/v2/_catalog?n=100&last=a>; rel="next"`,
			"/prefix/v2/_catalog?n=100&last=a",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var uris []string
			c := &Client{
				Base: "http://registry.test/prefix",
				HTTP: &http.Client{Transport: roundTrip(func(w http.ResponseWriter, r *http.Request) {
					uris = append(uris, r.URL.RequestURI())
					if r.URL.Query().Get("last") == "" {
						w.Header().Set("Link", tc.link)
						_, _ = w.Write([]byte(`{"repositories":["a"]}`))
						return
					}
					_, _ = w.Write([]byte(`{"repositories":["b"]}`))
				})},
			}

			repos, err := c.Catalog(context.Background())
			if err != nil {
				t.Fatalf("Catalog: %v", err)
			}
			if want := []string{"a", "b"}; !slices.Equal(repos, want) {
				t.Errorf("Catalog = %v, want %v", repos, want)
			}
			want := []string{"/prefix/v2/_catalog?n=100", tc.wantSecond}
			if !slices.Equal(uris, want) {
				t.Errorf("registry read %v, want %v", uris, want)
			}
		})
	}
}

func TestCatalogFollowsLinkHoldingAComma(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, link string }{
		{"comma in the target", `</v2/_catalog?n=100&last=a,b>; rel="next"`},
		{"comma in a quoted parameter", `</v2/_catalog?n=100&last=a>; title="x,y"; rel="next"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("last") == "" {
					w.Header().Set("Link", tc.link)
					_, _ = w.Write([]byte(`{"repositories":["a"]}`))
					return
				}
				_, _ = w.Write([]byte(`{"repositories":["b"]}`))
			})

			repos, err := c.Catalog(context.Background())
			if err != nil {
				t.Fatalf("Catalog: %v", err)
			}
			if want := []string{"a", "b"}; !slices.Equal(repos, want) {
				t.Errorf("Catalog = %v, want %v", repos, want)
			}
		})
	}
}

func TestCatalogIgnoresNonNextLinks(t *testing.T) {
	t.Parallel()
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `</v2/_catalog?n=100>; rel="prev"`)
		_, _ = w.Write([]byte(`{"repositories":["a"]}`))
	})

	repos, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if want := []string{"a"}; !slices.Equal(repos, want) {
		t.Errorf("Catalog = %v, want %v", repos, want)
	}
}

func TestListingBodyOverLimitIsError(t *testing.T) {
	t.Parallel()
	oversized := func(field string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"`+field+`":["`)
			_, _ = io.WriteString(w, strings.Repeat("a", maxBodyBytes))
			_, _ = io.WriteString(w, `"]}`)
		}
	}
	ctx := context.Background()

	if _, err := newTestClient(oversized("repositories")).Catalog(ctx); err == nil {
		t.Error("Catalog decoded a body past the size bound")
	}
	if _, err := newTestClient(oversized("tags")).Tags(ctx, "gastown-polecat"); err == nil {
		t.Error("Tags decoded a body past the size bound")
	}
}

func TestListingBodyAtTheLimitIsRead(t *testing.T) {
	t.Parallel()
	// The body is the bound itself, which a bound that refused it would turn
	// into an error: only a body past it is over.
	const size = maxBodyBytes - len(`{"repositories":[""]}`)
	body := `{"repositories":["` + strings.Repeat("a", size) + `"]}`
	if len(body) != maxBodyBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(body), maxBodyBytes)
	}
	c := newTestClient(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })

	repos, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog with a body at the size bound: %v", err)
	}
	if len(repos) != 1 || len(repos[0]) != size {
		t.Errorf("Catalog read %d names, want one of %d bytes", len(repos), size)
	}
}

func TestCatalogReturnsABodyThatFailsMidRead(t *testing.T) {
	t.Parallel()
	c := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: bodyTransport{body: brokenBody{}}}}

	_, err := c.Catalog(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("Catalog error = %v, want the read failure", err)
	}
}

func TestDeleteManifestRejectsUnbuildableRequest(t *testing.T) {
	t.Parallel()
	handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {})
	c := &Client{Base: "http://registry.test/\nbad", HTTP: &http.Client{Transport: roundTrip(handler)}}

	err := c.DeleteManifest(context.Background(), "gastown-polecat", testDigest())
	if err == nil {
		t.Fatal("DeleteManifest with a base that cannot form a URL returned no error")
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("transport saw %d requests, want 0", n)
	}
}

func TestDoReturnsTheRequestItCannotBuild(t *testing.T) {
	t.Parallel()
	u, err := url.Parse("http://registry.test/v2/_catalog")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	handler, requests := countingHandler(func(w http.ResponseWriter, r *http.Request) {})
	c := &Client{Base: "http://registry.test", HTTP: &http.Client{Transport: roundTrip(handler)}}

	// A caller's method is the one thing this client cannot validate, so the
	// request builder's refusal is passed back as it comes.
	if _, err := c.do(context.Background(), "GET WITH A SPACE", u, nil); err == nil {
		t.Fatal("do with a method that cannot form a request returned no error")
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("transport saw %d requests, want 0", n)
	}
}

func TestCatalogEndsAStalledRequest(t *testing.T) {
	t.Parallel()
	c := &Client{
		Base:      "http://registry.test",
		waitLimit: 10 * time.Millisecond,
		HTTP:      &http.Client{Transport: stalledTransport{limit: 2 * time.Second}},
	}

	_, err := c.Catalog(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Catalog error = %v, want the request's deadline", err)
	}
}
