package imagevalues_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	testViewerUser  = "viewer-bot"
	testViewerToken = "viewer-token-do-not-log"
	testTag         = "0123456789ab"
)

// handlerTransport answers each request in process, so the registry fixture
// opens no socket: the unit tier stays in-process.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// loopbackRegistry is a registry on a loopback address: the credential may be
// sent to it over http, so a fixture needs no https endpoint.
var loopbackRegistry = imagevalues.Registry{Scheme: "http", Host: "127.0.0.1:5001", Owner: "bot-registry"}

// digestOf is the digest a registry reports for body.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// manifestServer answers the manifest of loopbackRegistry's daemon image at
// testTag, with the digest header the test sets and the body the test serves.
func manifestServer(t *testing.T, status int, header string, body []byte, seen *http.Request) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Docker-Content-Digest", header)
		_, _ = w.Write(body)
	})
}

// TestTagDigestReadsTheDigestTheManifestHashesTo: the read sends the viewer's
// basic credential to the manifest path of the tag, and returns the digest only
// when the manifest bytes hash to the digest the registry reported.
func TestTagDigestReadsTheDigestTheManifestHashesTo(t *testing.T) {
	t.Parallel()
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	want := digestOf(body)
	var seen http.Request
	hc := &http.Client{Transport: handlerTransport{manifestServer(t, 0, want, body, &seen)}}

	digest, found, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, want, digest)
	assert.Equal(t, "/v2/bot-registry/"+imagevalues.DaemonImage+"/manifests/"+testTag, seen.URL.Path)
	user, token, ok := seen.BasicAuth()
	require.True(t, ok, "the request carries no basic credential")
	assert.Equal(t, testViewerUser, user)
	assert.Equal(t, testViewerToken, token)
}

// TestTagDigestAbsentTagIsNotFound: a tag the registry does not hold is an
// answer, found=false, so one image not pushed yet reads as absent, not as a
// failed read.
func TestTagDigestAbsentTagIsNotFound(t *testing.T) {
	t.Parallel()
	hc := &http.Client{Transport: handlerTransport{manifestServer(t, http.StatusNotFound, "", nil, nil)}}

	digest, found, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, imagevalues.PolecatImage, testTag)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Empty(t, digest)
}

// TestTagDigestRefusesAnAnswerWhoseBytesDoNotHashToItsDigest: a registry that
// reports a digest its manifest bytes do not hash to is an error, never a pair.
func TestTagDigestRefusesAnAnswerWhoseBytesDoNotHashToItsDigest(t *testing.T) {
	t.Parallel()
	body := []byte(`{"schemaVersion":2}`)
	other := digestOf([]byte("some other manifest"))
	hc := &http.Client{Transport: handlerTransport{manifestServer(t, 0, other, body, nil)}}

	_, found, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.False(t, found)
	assert.Contains(t, err.Error(), "hash to")
}

// TestTagDigestRefusesAMalformedDigestHeader: a digest that is not
// sha256:<64 lowercase hex> is refused before any bytes are hashed.
func TestTagDigestRefusesAMalformedDigestHeader(t *testing.T) {
	t.Parallel()
	hc := &http.Client{Transport: handlerTransport{manifestServer(t, 0, "sha256:short", []byte("{}"), nil)}}

	_, _, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not sha256:<64 lowercase hex>")
}

// TestTagDigestRefusesPlaintextToAPublicHost: the viewer credential goes over
// http only to loopback or the town's Forgejo at forgejo:3000. Any other host is
// refused before a request is made, so no transport is consulted.
func TestTagDigestRefusesPlaintextToAPublicHost(t *testing.T) {
	t.Parallel()
	public := imagevalues.Registry{Scheme: "http", Host: "registry.example.com", Owner: "bot-registry"}
	hc := &http.Client{Transport: handlerTransport{manifestServer(t, http.StatusInternalServerError, "", nil, nil)}}

	_, _, err := imagevalues.TagDigest(context.Background(), hc, public, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to send the registry credential")
}

// TestTagDigestSendsPlaintextOnlyToTheHostTheCallerNames: http reaches a host
// only when the caller names it as the town's plaintext host. The same registry
// with no name is refused, so the host is the caller's, not this package's.
func TestTagDigestSendsPlaintextOnlyToTheHostTheCallerNames(t *testing.T) {
	t.Parallel()
	body := []byte(`{"schemaVersion":2}`)
	want := digestOf(body)
	town := imagevalues.Registry{Scheme: "http", Host: "forgejo:3000", Owner: "bot-registry"}
	hc := &http.Client{Transport: handlerTransport{manifestServer(t, 0, want, body, nil)}}

	digest, found, err := imagevalues.TagDigest(context.Background(), hc, town, "forgejo:3000", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, want, digest)

	_, _, err = imagevalues.TagDigest(context.Background(), hc, town, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to send the registry credential")
}

// TestTagDigestRefusesABadTagOrImage: a tag or image name that is not one the
// registry accepts never reaches a URL path.
func TestTagDigestRefusesABadTagOrImage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, image, tag string
	}{
		{name: "tag with a path segment", image: imagevalues.DaemonImage, tag: "../manifests/x"},
		{name: "empty tag", image: imagevalues.DaemonImage, tag: ""},
		{name: "image with a slash", image: "bot/gastown-daemon", tag: testTag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hc := &http.Client{Transport: handlerTransport{manifestServer(t, http.StatusInternalServerError, "", nil, nil)}}
			_, _, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, tc.image, tc.tag)
			require.Error(t, err)
		})
	}
}

// TestTagDigestErrorNeverCarriesTheToken: a registry that fails names its
// status, and the viewer's token reaches no error text.
func TestTagDigestErrorNeverCarriesTheToken(t *testing.T) {
	t.Parallel()
	hc := &http.Client{Transport: handlerTransport{manifestServer(t, http.StatusInternalServerError, "", nil, nil)}}

	_, _, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 500")
	assert.False(t, strings.Contains(err.Error(), testViewerToken), "the registry error carries the viewer token")
}
