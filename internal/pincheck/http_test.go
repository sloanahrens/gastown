package pincheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	// httpRepo is the repository every HTTP test reads: a host, an owner and a
	// name, none of them a real registry.
	httpRepo = "example.test/oci/docker"
	// tokenRealm is the realm the fake challenge names.
	tokenRealm = "https://auth.example.test/token"
	// httpService is the service the fake challenge names.
	httpService = "example.test"
)

// hAnswer is one answer a fakeHttp gives: a status, headers and a body, or an
// error instead of a response, or a body that fails to read.
type hAnswer struct {
	status  int
	header  http.Header
	body    string
	bodyErr error
	err     error
}

// errReader is a response body whose every read fails.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
func (e errReader) Close() error             { return nil }

// hSeen is one request a fakeHttp answered, copied, so a test reads the URL and
// the headers the code sent.
type hSeen struct {
	method string
	url    string
	header http.Header
}

// fakeHttp answers each route ("METHOD /path") from its own queue, in order,
// and records every request it answered.
type fakeHttp struct {
	answers map[string][]hAnswer
	seen    []hSeen
}

func newFakeHttp() *fakeHttp { return &fakeHttp{answers: map[string][]hAnswer{}} }

// answer queues answers for one route.
func (f *fakeHttp) answer(method, path string, answers ...hAnswer) {
	key := method + " " + path
	f.answers[key] = append(f.answers[key], answers...)
}

func (f *fakeHttp) RoundTrip(req *http.Request) (*http.Response, error) {
	f.seen = append(f.seen, hSeen{method: req.Method, url: req.URL.String(), header: req.Header.Clone()})
	key := req.Method + " " + req.URL.Path
	queue, ok := f.answers[key]
	if !ok || len(queue) == 0 {
		return nil, fmt.Errorf("the fake has no answer for %s", key)
	}
	a := queue[0]
	f.answers[key] = queue[1:]
	if a.err != nil {
		return nil, a.err
	}
	header := a.header
	if header == nil {
		header = http.Header{}
	}
	var body io.ReadCloser = io.NopCloser(strings.NewReader(a.body))
	if a.bodyErr != nil {
		body = errReader{a.bodyErr}
	}
	return &http.Response{
		StatusCode:    a.status,
		Status:        fmt.Sprintf("%d %s", a.status, http.StatusText(a.status)),
		Header:        header,
		Body:          body,
		ContentLength: int64(len(a.body)),
		Request:       req,
	}, nil
}

// hseenFor is the requests the fake answered for one route, in order.
func (f *fakeHttp) seenFor(method, path string) []hSeen {
	var out []hSeen
	for _, seen := range f.seen {
		u, err := url.Parse(seen.url)
		if err != nil {
			continue
		}
		if seen.method == method && u.Path == path {
			out = append(out, seen)
		}
	}
	return out
}

// httpChallenge is the 401 a registry answers for a public namespace: a Bearer
// challenge naming the token realm, the service and the scope.
func httpChallenge() http.Header {
	h := http.Header{}
	h.Set("WWW-Authenticate", `Bearer realm="`+tokenRealm+`",service="`+httpService+`",scope="repository:oci/docker:pull"`)
	return h
}

// challengeAnswer is a 401 carrying that challenge.
func challengeAnswer() hAnswer {
	return hAnswer{status: http.StatusUnauthorized, header: httpChallenge()}
}

// locationHeader is a redirect to target.
func locationHeader(target string) http.Header {
	return http.Header{"Location": []string{target}}
}

// httpRegistry is a Registry over a fake transport.
func httpRegistry(f *fakeHttp) HTTP {
	return HTTP{Client: &http.Client{Transport: f}}
}

// TestImagePinDurabilityHTTPBearerHandshake drives the acceptance case: a 401
// with a Bearer challenge costs exactly one anonymous token request, and the
// retry carries the token, for both a manifest read and a blob read.
func TestImagePinDurabilityHTTPBearerHandshake(t *testing.T) {
	t.Parallel()
	digest := digestFor(0x51)
	tests := []struct {
		name     string
		method   string
		path     string
		answered hAnswer
		call     func(context.Context, HTTP) (string, error)
	}{
		{
			name:     "manifest",
			method:   http.MethodGet,
			path:     "/v2/oci/docker/manifests/" + digest,
			answered: hAnswer{status: http.StatusOK, body: `{"mediaType":"` + mediaTypeOCIManifest + `"}`},
			call: func(ctx context.Context, reg HTTP) (string, error) {
				body, _, err := reg.Manifest(ctx, httpRepo, digest)
				return string(body), err
			},
		},
		{
			name:     "blob",
			method:   http.MethodHead,
			path:     "/v2/oci/docker/blobs/" + digest,
			answered: hAnswer{status: http.StatusOK},
			call: func(ctx context.Context, reg HTTP) (string, error) {
				served, err := reg.BlobExists(ctx, httpRepo, digest)
				return fmt.Sprint(served), err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeHttp()
			fake.answer(tc.method, tc.path, challengeAnswer(), tc.answered)
			fake.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, body: `{"token":"tok-abc123"}`})

			got, err := tc.call(context.Background(), httpRegistry(fake))
			if err != nil {
				t.Fatalf("call = error %v, want the challenge answered and the retry to succeed", err)
			}
			want := "true"
			if tc.name == "manifest" {
				want = tc.answered.body
			}
			if got != want {
				t.Errorf("call = %q, want %q", got, want)
			}

			registryCalls := fake.seenFor(tc.method, tc.path)
			if len(registryCalls) != 2 {
				t.Fatalf("the registry was asked %d times for %s %s, want the first request and one retry", len(registryCalls), tc.method, tc.path)
			}
			if auth := registryCalls[0].header.Get("Authorization"); auth != "" {
				t.Errorf("the first request carried Authorization %q, want none: the challenge is what asks for a token", auth)
			}
			if auth := registryCalls[1].header.Get("Authorization"); auth != "Bearer tok-abc123" {
				t.Errorf("the retry carried Authorization %q, want the token the realm issued", auth)
			}

			tokenCalls := fake.seenFor(http.MethodGet, "/token")
			if len(tokenCalls) != 1 {
				t.Fatalf("the token realm was asked %d times, want exactly one anonymous request", len(tokenCalls))
			}
			if auth := tokenCalls[0].header.Get("Authorization"); auth != "" {
				t.Errorf("the token request carried Authorization %q, want none", auth)
			}
			u, err := url.Parse(tokenCalls[0].url)
			if err != nil {
				t.Fatalf("parse the token request URL %q: %v", tokenCalls[0].url, err)
			}
			if got := u.Query().Get("service"); got != httpService {
				t.Errorf("the token request's service = %q, want the challenge's %q", got, httpService)
			}
			if got := u.Query().Get("scope"); got != "repository:oci/docker:pull" {
				t.Errorf("the token request's scope = %q, want %q", got, "repository:oci/docker:pull")
			}
			for _, seen := range fake.seen {
				if !strings.HasPrefix(seen.url, httpScheme+"://") {
					t.Errorf("a request went to %q, want every request over %s", seen.url, httpScheme)
				}
			}
		})
	}
}

// TestImagePinDurabilityHTTPChallengeScope: the token is asked for the scope of
// the repository being read, so a challenge naming another repository cannot
// make the read ask for a token to that one.
func TestImagePinDurabilityHTTPChallengeScope(t *testing.T) {
	t.Parallel()
	digest := digestFor(0x52)
	path := "/v2/oci/docker/manifests/" + digest
	fake := newFakeHttp()
	challenge := http.Header{}
	challenge.Set("WWW-Authenticate", `Bearer realm="`+tokenRealm+`",service="`+httpService+`",scope="repository:someone/else:pull"`)
	fake.answer(http.MethodGet, path, hAnswer{status: http.StatusUnauthorized, header: challenge},
		hAnswer{status: http.StatusOK, body: `{}`})
	fake.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, body: `{"token":"tok-abc123"}`})

	if _, _, err := httpRegistry(fake).Manifest(context.Background(), httpRepo, digest); err != nil {
		t.Fatalf("Manifest = error %v, want the challenge answered", err)
	}
	tokenCalls := fake.seenFor(http.MethodGet, "/token")
	if len(tokenCalls) != 1 {
		t.Fatalf("the token realm was asked %d times, want once", len(tokenCalls))
	}
	u, err := url.Parse(tokenCalls[0].url)
	if err != nil {
		t.Fatalf("parse the token request URL %q: %v", tokenCalls[0].url, err)
	}
	if got := u.Query().Get("scope"); got != "repository:oci/docker:pull" {
		t.Errorf("the token request's scope = %q, want the read repository's %q", got, "repository:oci/docker:pull")
	}
}

// TestImagePinDurabilityHTTPAcceptHeader drives the acceptance case: the
// manifest request asks for exactly the four media types readManifest
// classifies.
func TestImagePinDurabilityHTTPAcceptHeader(t *testing.T) {
	t.Parallel()
	digest := digestFor(0x53)
	path := "/v2/oci/docker/manifests/" + digest
	fake := newFakeHttp()
	fake.answer(http.MethodGet, path, hAnswer{
		status: http.StatusOK,
		header: http.Header{"Content-Type": []string{mediaTypeOCIManifest}},
		body:   `{}`,
	})

	if _, _, err := httpRegistry(fake).Manifest(context.Background(), httpRepo, digest); err != nil {
		t.Fatalf("Manifest = error %v, want the manifest read", err)
	}
	calls := fake.seenFor(http.MethodGet, path)
	if len(calls) != 1 {
		t.Fatalf("the registry was asked %d times, want one GET", len(calls))
	}
	var named []string
	for _, mediaType := range strings.Split(calls[0].header.Get("Accept"), ",") {
		named = append(named, strings.TrimSpace(mediaType))
	}
	want := []string{mediaTypeOCIIndex, mediaTypeDockerList, mediaTypeOCIManifest, mediaTypeDockerManifest}
	if len(named) != len(want) {
		t.Errorf("the Accept header names %d media types (%v), want the four manifest types %v", len(named), named, want)
	}
	for _, mediaType := range want {
		if !slices.Contains(named, mediaType) {
			t.Errorf("the Accept header %q does not name %q", strings.Join(named, ", "), mediaType)
		}
	}
}

// TestImagePinDurabilityHTTPBlobStatus drives the acceptance case: BlobExists
// answers true on 200 and false on 404, and every other status is an error.
func TestImagePinDurabilityHTTPBlobStatus(t *testing.T) {
	t.Parallel()
	digest := digestFor(0x54)
	path := "/v2/oci/docker/blobs/" + digest
	tests := []struct {
		name    string
		status  int
		want    bool
		wantErr string
	}{
		{name: "served", status: http.StatusOK, want: true},
		{name: "gone upstream", status: http.StatusNotFound, want: false},
		{name: "registry error", status: http.StatusInternalServerError, wantErr: "answered HTTP 500 for the blob"},
		{name: "no content is neither served nor gone", status: http.StatusNoContent, wantErr: "answered HTTP 204 for the blob"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeHttp()
			fake.answer(http.MethodHead, path, hAnswer{status: tc.status})

			got, err := httpRegistry(fake).BlobExists(context.Background(), httpRepo, digest)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("BlobExists on HTTP %d = %v, nil, want an error", tc.status, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("BlobExists error = %q, want it to carry %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BlobExists on HTTP %d = error %v, want no error", tc.status, err)
			}
			if got != tc.want {
				t.Errorf("BlobExists on HTTP %d = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestImagePinDurabilityHTTPRefusals drives every refusal: an input that
// cannot reach a request path, a challenge that cannot yield a token, and a
// registry answer that is neither the body nor a status the read understands
// are errors, never a served blob or a readable manifest.
func TestImagePinDurabilityHTTPRefusals(t *testing.T) {
	t.Parallel()
	digest := digestFor(0x55)
	manifestPath := "/v2/oci/docker/manifests/" + digest
	blobPath := "/v2/oci/docker/blobs/" + digest
	manifestCall := func(repo, ref string) func(context.Context, HTTP) error {
		return func(ctx context.Context, reg HTTP) error {
			_, _, err := reg.Manifest(ctx, repo, ref)
			return err
		}
	}
	blobCall := func(repo, digest string) func(context.Context, HTTP) error {
		return func(ctx context.Context, reg HTTP) error {
			_, err := reg.BlobExists(ctx, repo, digest)
			return err
		}
	}
	challengeWith := func(value string) hAnswer {
		header := http.Header{}
		header.Set("WWW-Authenticate", value)
		return hAnswer{status: http.StatusUnauthorized, header: header}
	}
	tests := []struct {
		name    string
		call    func(context.Context, HTTP) error
		fake    func(*fakeHttp)
		want    string
		notWant string
	}{
		{
			name: "repo with no host",
			call: manifestCall("oci/docker", digest),
			want: "does not begin with a registry host",
		},
		{
			name: "repo with a traversal component",
			call: manifestCall("example.test/../docker", digest),
			want: "not lowercase path components",
		},
		{
			name: "repo with an uppercase component",
			call: manifestCall("example.test/oci/Docker", digest),
			want: "not lowercase path components",
		},
		{
			name: "repo with an empty name",
			call: manifestCall("example.test/", digest),
			want: "is not host/owner/name",
		},
		{
			name: "empty repo",
			call: manifestCall("", digest),
			want: "is not host/owner/name",
		},
		{
			name: "manifest ref that is neither a digest nor a tag",
			call: manifestCall(httpRepo, "sha256:abc"),
			want: "is neither a digest nor a tag",
		},
		{
			name: "empty manifest ref",
			call: manifestCall(httpRepo, ""),
			want: "is neither a digest nor a tag",
		},
		{
			name: "blob digest that is not a digest",
			call: blobCall(httpRepo, "sha256:abc"),
			want: "not sha256:<64 lowercase hex>",
		},
		{
			name: "blob repo with no host",
			call: blobCall("oci/docker", digest),
			want: "does not begin with a registry host",
		},
		{
			name: "manifest the registry does not serve",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, hAnswer{status: http.StatusNotFound})
			},
			want: "answered HTTP 404 for the manifest",
		},
		{
			name: "manifest registry error",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, hAnswer{status: http.StatusInternalServerError})
			},
			want: "answered HTTP 500 for the manifest",
		},
		{
			name: "manifest body over the limit",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, hAnswer{status: http.StatusOK, body: strings.Repeat("x", httpMaxManifestBytes+1)})
			},
			want: "larger than the",
		},
		{
			name: "manifest body that cannot be read",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, hAnswer{status: http.StatusOK, bodyErr: errors.New("the connection was cut")})
			},
			want: "reading the manifest at",
		},
		{
			name: "blob registry error",
			call: blobCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodHead, blobPath, hAnswer{status: http.StatusInternalServerError})
			},
			want: "answered HTTP 500 for the blob",
		},
		{
			name: "blob transport error",
			call: blobCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodHead, blobPath, hAnswer{err: errors.New("the network is down")})
			},
			want: "the HEAD request to example.test",
		},
		{
			name: "401 without a challenge",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, hAnswer{status: http.StatusUnauthorized})
			},
			want: "without a Bearer challenge",
		},
		{
			name: "401 with a Basic challenge",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeWith(`Basic realm="example.test"`))
			},
			want: "without a Bearer challenge",
		},
		{
			name: "Bearer challenge with no realm",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeWith(`Bearer service="`+httpService+`"`))
			},
			want: "names no realm",
		},
		{
			name: "Bearer challenge with no service",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeWith(`Bearer realm="`+tokenRealm+`"`))
			},
			want: "names no service",
		},
		{
			name: "realm that is not a URL",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeWith(`Bearer realm="::not a url",service="`+httpService+`"`))
			},
			want: "not a URL",
		},
		{
			name: "realm over http",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeWith(`Bearer realm="http://auth.example.test/token",service="`+httpService+`"`))
			},
			want: "https only",
		},
		{
			name: "token realm error",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusInternalServerError})
			},
			want: "the token realm answered HTTP 500",
		},
		{
			name: "token body that does not parse",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, body: "<"})
			},
			want: "does not parse",
		},
		{
			name: "token body with no token",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, body: `{}`})
			},
			want: "carries no token",
		},
		{
			name: "token that is not a bearer token",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, body: `{"token":"not a token"}`})
			},
			want: "is not a bearer token",
		},
		{
			name: "token body over the limit",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, body: strings.Repeat("x", httpMaxTokenBytes+1)})
			},
			want: "larger than the",
		},
		{
			name: "token body that cannot be read",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, bodyErr: errors.New("the connection was cut")})
			},
			want: "reading the token for the service",
		},
		{
			name: "a retry that is still 401",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer(), challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, body: `{"token":"tok-refused-9876"}`})
			},
			want:    "refused GET /v2/oci/docker",
			notWant: "tok-refused-9876",
		},
		{
			name: "transport error",
			call: manifestCall(httpRepo, digest),
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, hAnswer{err: errors.New("the network is down")})
			},
			want: "the GET request to example.test",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeHttp()
			if tc.fake != nil {
				tc.fake(fake)
			}
			err := tc.call(context.Background(), httpRegistry(fake))
			if err == nil {
				t.Fatalf("call = nil, want an error carrying %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("call error = %q, want it to carry %q", err, tc.want)
			}
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("the error %q carries the issued token %q, want the token kept out of every error", err, tc.notWant)
			}
			if tc.fake == nil && len(fake.seen) != 0 {
				t.Errorf("%d requests were sent before the input was refused, want none", len(fake.seen))
			}
		})
	}
}

// TestImagePinDurabilityHTTPRedirect: a redirect that changes the scheme or the
// host is refused on the retry that carries the token and on the token request
// itself, so the token never follows a Location to another endpoint.
func TestImagePinDurabilityHTTPRedirect(t *testing.T) {
	t.Parallel()
	digest := digestFor(0x56)
	manifestPath := "/v2/oci/docker/manifests/" + digest
	tests := []struct {
		name string
		fake func(*fakeHttp)
	}{
		{
			name: "the token-bearing retry redirected to http",
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, body: `{"token":"tok-abc123"}`})
				f.answer(http.MethodGet, manifestPath, hAnswer{status: http.StatusFound, header: locationHeader("http://example.test" + manifestPath)})
			},
		},
		{
			name: "the token-bearing retry redirected to another host",
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusOK, body: `{"token":"tok-abc123"}`})
				f.answer(http.MethodGet, manifestPath, hAnswer{status: http.StatusFound, header: locationHeader("https://elsewhere.example.test" + manifestPath)})
			},
		},
		{
			name: "the token request redirected to http",
			fake: func(f *fakeHttp) {
				f.answer(http.MethodGet, manifestPath, challengeAnswer())
				f.answer(http.MethodGet, "/token", hAnswer{status: http.StatusFound, header: locationHeader("http://auth.example.test/token")})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeHttp()
			tc.fake(fake)

			_, _, err := httpRegistry(fake).Manifest(context.Background(), httpRepo, digest)
			if err == nil {
				t.Fatal("Manifest = nil error, want the redirect refused")
			}
			if !strings.Contains(err.Error(), "is refused") {
				t.Errorf("Manifest error = %q, want the redirect refused", err)
			}
			for _, seen := range fake.seen {
				if !strings.HasPrefix(seen.url, httpScheme+"://") {
					t.Errorf("a request went to %q, want every request over %s", seen.url, httpScheme)
				}
			}
		})
	}
}

// boundTransport records the deadline the client puts on each request and
// answers 200, so a test reads the bound without waiting for it.
type boundTransport struct {
	deadlines []time.Time
	has       []bool
}

func (b *boundTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	b.deadlines = append(b.deadlines, deadline)
	b.has = append(b.has, ok)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{mediaTypeOCIManifest}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    req,
	}, nil
}

// TestImagePinDurabilityHTTPBound drives the bound: a request always carries a
// deadline, the injected client's own timeout when it names one and httpTimeout
// when it does not, so a registry that never answers cannot hold the read open.
func TestImagePinDurabilityHTTPBound(t *testing.T) {
	t.Parallel()
	digest := digestFor(0x57)
	tests := []struct {
		name   string
		client *http.Client
		floor  time.Duration
		ceil   time.Duration
	}{
		{
			name:   "a client that names no timeout is bounded by httpTimeout",
			client: &http.Client{},
			ceil:   httpTimeout,
		},
		{
			name:   "a client's own timeout is kept",
			client: &http.Client{Timeout: 90 * time.Second},
			floor:  httpTimeout,
			ceil:   90 * time.Second,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			transport := &boundTransport{}
			tc.client.Transport = transport
			if _, _, err := (HTTP{Client: tc.client}).Manifest(context.Background(), httpRepo, digest); err != nil {
				t.Fatalf("Manifest = error %v, want the manifest read", err)
			}
			if len(transport.has) != 1 {
				t.Fatalf("the transport answered %d requests, want one", len(transport.has))
			}
			if !transport.has[0] {
				t.Fatal("the request carried no deadline, want it bounded")
			}
			remaining := time.Until(transport.deadlines[0])
			if remaining <= tc.floor {
				t.Errorf("the request's deadline is %v away, want it later than %v: the client's own bound was replaced", remaining, tc.floor)
			}
			if remaining > tc.ceil {
				t.Errorf("the request's deadline is %v away, want it within %v", remaining, tc.ceil)
			}
		})
	}
}
