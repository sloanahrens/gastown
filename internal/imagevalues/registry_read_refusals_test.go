package imagevalues_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// failingTransport is a registry that cannot be reached: every request fails.
type failingTransport struct{ err error }

func (t failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

// unreadableBody is an answer whose body fails partway: the connection drops.
type unreadableBody struct{}

func (unreadableBody) Read([]byte) (int, error) { return 0, errors.New("the connection dropped") }
func (unreadableBody) Close() error             { return nil }

// unreadableTransport answers 200 with a body that cannot be read.
type unreadableTransport struct{}

func (unreadableTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: unreadableBody{}}, nil
}

// TestTagDigestRefusesAnInvalidRegistry: a registry whose parts would not reach a
// URL as typed is refused before any request is built.
func TestTagDigestRefusesAnInvalidRegistry(t *testing.T) {
	t.Parallel()
	bad := imagevalues.Registry{Scheme: "ftp", Host: "127.0.0.1:5001", Owner: "bot-registry"}
	hc := &http.Client{Transport: failingTransport{err: errors.New("no request should be sent")}}

	_, _, err := imagevalues.TagDigest(context.Background(), hc, bad, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not https or http")
}

// TestTagDigestRefusesAnUnreachableRegistry: a registry that does not answer is
// an error, not a tag that is absent.
func TestTagDigestRefusesAnUnreachableRegistry(t *testing.T) {
	t.Parallel()
	hc := &http.Client{Transport: failingTransport{err: errors.New("connection refused")}}

	digest, found, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.False(t, found, "an unreachable registry read as an absent tag")
	assert.Empty(t, digest)
	assert.Contains(t, err.Error(), "connection refused")
}

// TestTagDigestRefusesAnAnswerThatDoesNotRead: a body that fails partway is an
// error, never a digest read from part of an answer.
func TestTagDigestRefusesAnAnswerThatDoesNotRead(t *testing.T) {
	t.Parallel()
	hc := &http.Client{Transport: unreadableTransport{}}

	_, found, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.False(t, found)
	assert.Contains(t, err.Error(), "reading the registry's answer")
}

// TestTagDigestRefusesAnAnswerLargerThanTheBound: a manifest past the reader's
// bound is refused, so a registry cannot make the check allocate without limit.
func TestTagDigestRefusesAnAnswerLargerThanTheBound(t *testing.T) {
	t.Parallel()
	const bound = 1 << 20
	body := bytes.Repeat([]byte("a"), bound+1)
	hc := &http.Client{Transport: handlerTransport{manifestServer(t, 0, digestOf(body), body, nil)}}

	_, found, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.False(t, found)
	assert.Contains(t, err.Error(), "larger than")
}

// TestTagDigestRefusesARedirectThatChangesTheHost: the credential follows only
// the first request's endpoint. A redirect to another host is refused before the
// credential is sent there.
func TestTagDigestRefusesARedirectThatChangesTheHost(t *testing.T) {
	t.Parallel()
	redirect := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://127.0.0.2:5001/v2/bot-registry/gastown-daemon/manifests/"+testTag)
		w.WriteHeader(http.StatusFound)
	})
	hc := &http.Client{Transport: handlerTransport{redirect}}

	_, _, err := imagevalues.TagDigest(context.Background(), hc, loopbackRegistry, "", testViewerUser, testViewerToken, imagevalues.DaemonImage, testTag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirected from")
	assert.NotContains(t, err.Error(), testViewerToken)
}
