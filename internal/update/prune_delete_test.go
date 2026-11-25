package update

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// pruneDeleteBody stands in for an answer's body: no error may carry it.
const pruneDeleteBody = "SECRET-BODY-MARKER"

// pruneDeleteRegistry is the town's registry at srv, built as the read path builds it.
func pruneDeleteRegistry(t *testing.T, srv *httptest.Server) imagevalues.Registry {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing the test server's URL: %v", err)
	}
	return imagevalues.Registry{Scheme: u.Scheme, Host: u.Host, Owner: "bot-registry"}
}

// pruneDeleteServer answers every request with status and pruneDeleteBody, and records the requests it received.
func pruneDeleteServer(t *testing.T, status int, seen *[]*http.Request) *httptest.Server {
	t.Helper()
	//testpolicy:allow no-network — the test's subject is the request the packages API receives, which a scripted Doer cannot show; the server is in-process on loopback
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Clone(context.Background()))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, pruneDeleteBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDeletePruneTag holds that one tag is deleted by one DELETE to its
// package version's path with the registry bot's basic credential, that a 2xx
// answer is success, and that any other status is an error naming the image,
// the tag and the status but never the answer's body.
func TestDeletePruneTag(t *testing.T) {
	t.Parallel()
	const image, tag, user, token = "gastown-daemon", "gdeadbeef123", "bot-registry", "registry-token"
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"204 is success", http.StatusNoContent},
		{"200 is success", http.StatusOK},
		{"403 is an error", http.StatusForbidden},
		{"500 is an error", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen []*http.Request
			srv := pruneDeleteServer(t, tc.status, &seen)
			err := DeletePruneTag(context.Background(), srv.Client(), pruneDeleteRegistry(t, srv), "", user, token, image, tag)
			if len(seen) != 1 {
				t.Fatalf("the packages API received %d requests, want one", len(seen))
			}
			req := seen[0]
			if req.Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", req.Method)
			}
			if want := "/api/v1/packages/bot-registry/container/" + image + "/" + tag; req.URL.Path != want {
				t.Errorf("path = %q, want %q", req.URL.Path, want)
			}
			gotUser, gotToken, ok := req.BasicAuth()
			if !ok || gotUser != user || gotToken != token {
				t.Errorf("the delete carried no basic auth for the registry bot")
			}
			if tc.status >= 200 && tc.status <= 299 {
				if err != nil {
					t.Fatalf("DeletePruneTag() error = %v, want success on HTTP %d", err, tc.status)
				}
				return
			}
			if err == nil {
				t.Fatalf("DeletePruneTag() error = nil, want an error on HTTP %d", tc.status)
			}
			for _, part := range []string{image, tag, strconv.Itoa(tc.status)} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not name %q", err, part)
				}
			}
			if strings.Contains(err.Error(), pruneDeleteBody) {
				t.Errorf("error %q carries the answer's body", err)
			}
		})
	}
}

// pruneDeleteProbe records the requests it is asked to send and answers 204, or
// err, without dialing, so a refusal is shown to send nothing.
type pruneDeleteProbe struct {
	sent []*http.Request
	err  error
}

func (p *pruneDeleteProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	p.sent = append(p.sent, req)
	if p.err != nil {
		return nil, p.err
	}
	return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
}

// TestDeletePruneTagRefuses holds that a delete its arguments cannot make, or
// one the registry never answers, is an error, and that the arguments are
// refused before any request is sent.
func TestDeletePruneTagRefuses(t *testing.T) {
	t.Parallel()
	const image, tag, user, token = "gastown-daemon", "gdeadbeef123", "bot-registry", "registry-token"
	loopback := imagevalues.Registry{Scheme: "http", Host: "127.0.0.1:5000", Owner: "bot-registry"}
	for _, tc := range []struct {
		name     string
		client   bool
		probeErr error
		reg      imagevalues.Registry
		image    string
		tag      string
		wantSent int
		wantErr  string
	}{
		{"no HTTP client", false, nil, loopback, image, tag, 0, "no HTTP client"},
		{"unusable registry", true, nil, imagevalues.Registry{Scheme: "ftp", Host: "127.0.0.1:5000", Owner: "bot-registry"}, image, tag, 0, "not https or http"},
		{"image name", true, nil, loopback, "../daemon", tag, 0, "not one repository name"},
		{"tag", true, nil, loopback, image, "tag/../other", 0, "not a tag"},
		{"http to another host", true, nil, imagevalues.Registry{Scheme: "http", Host: "forgejo.example:3000", Owner: "bot-registry"}, image, tag, 0, "refusing to send"},
		{"no answer", true, errors.New("connection reset"), loopback, image, tag, 1, "deleting " + image + ":" + tag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := &pruneDeleteProbe{err: tc.probeErr}
			var client *http.Client
			if tc.client {
				client = &http.Client{Transport: probe}
			}
			err := DeletePruneTag(context.Background(), client, tc.reg, "", user, token, tc.image, tc.tag)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("DeletePruneTag() error = %v, want one naming %q", err, tc.wantErr)
			}
			if len(probe.sent) != tc.wantSent {
				t.Errorf("the delete sent %d requests, want %d", len(probe.sent), tc.wantSent)
			}
		})
	}
}
