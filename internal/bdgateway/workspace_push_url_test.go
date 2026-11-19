package bdgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// pushRouteCall runs one of the three calls that take the push URL: the push
// itself, the probe, or the reported-hash read. path is the route the call
// sends, so a test can name the endpoint it expects.
type pushRouteCall struct {
	name string
	path string
	call func(t *testing.T, g GatewayPusher, root string) error
}

// pushRouteCalls is Push, Probe and ReportedHash, each driven through the
// production method, so a test that drives the three covers every request the
// pusher sends.
var pushRouteCalls = []pushRouteCall{
	{
		name: "push",
		path: WorkspacePath,
		call: func(t *testing.T, g GatewayPusher, root string) error {
			t.Helper()
			_, err := g.Push(context.Background(), root)
			return err
		},
	},
	{
		name: "probe",
		path: statsPath,
		call: func(t *testing.T, g GatewayPusher, _ string) error {
			t.Helper()
			return g.Probe(context.Background())
		},
	},
	{
		name: "reported hash",
		path: statsPath,
		call: func(t *testing.T, g GatewayPusher, _ string) error {
			t.Helper()
			_, err := g.ReportedHash(context.Background())
			return err
		},
	},
}

// gatewayDoer answers one call as the gateway does, records the request it saw,
// and echoes the manifest a push sent so the push's manifest check passes.
func gatewayDoer(t *testing.T, got **http.Request) HTTPDoer {
	t.Helper()
	return doerFunc(func(req *http.Request) (*http.Response, error) {
		*got = req
		if req.Method == http.MethodPut {
			var sent WorkspaceDocument
			if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
				t.Fatalf("the request body is not a workspace document: %v", err)
			}
			return jsonResponse(http.StatusOK, `{"manifest_sha":"`+sent.ManifestSHA+`"}`), nil
		}
		resp := jsonResponse(http.StatusOK, "")
		resp.Header.Set(WorkspaceHashHeader, "manifest-1")
		return resp, nil
	})
}

// TestGatewayPusherPushURLUsed: with PushURL set, Push sends to the push URL and
// Probe and ReportedHash send to URL: the push Service takes the write, and the
// gateway takes every read.
func TestGatewayPusherPushURLUsed(t *testing.T) {
	t.Parallel()
	const pushURL = "http://127.0.0.1:7441"
	const url = "http://127.0.0.1:7440"
	for _, route := range pushRouteCalls {
		route := route
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			root := clientTown(t)
			var got *http.Request
			pusher := GatewayPusher{
				URL:       url,
				PushURL:   pushURL,
				TokenFile: clientToken(t, "tok"),
				HTTP:      gatewayDoer(t, &got),
			}
			if err := route.call(t, pusher, root); err != nil {
				t.Fatalf("%s: %v", route.name, err)
			}
			if got == nil {
				t.Fatal("the pusher sent no request")
			}
			want := url + route.path
			if route.name == "push" {
				want = pushURL + route.path
			}
			if got.URL.String() != want {
				t.Errorf("the request went to %s, want %s", got.URL, want)
			}
		})
	}
}

// TestGatewayPusherPushURLFallback: with PushURL empty, Push, Probe and
// ReportedHash send to URL as they did before the push URL existed.
func TestGatewayPusherPushURLFallback(t *testing.T) {
	t.Parallel()
	const url = "http://127.0.0.1:7440"
	for _, route := range pushRouteCalls {
		route := route
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			root := clientTown(t)
			var got *http.Request
			pusher := GatewayPusher{
				URL:       url,
				TokenFile: clientToken(t, "tok"),
				HTTP:      gatewayDoer(t, &got),
			}
			if err := route.call(t, pusher, root); err != nil {
				t.Fatalf("%s: %v", route.name, err)
			}
			if got == nil {
				t.Fatal("the pusher sent no request")
			}
			if want := url + route.path; got.URL.String() != want {
				t.Errorf("the request went to %s, want the URL %s", got.URL, want)
			}
		})
	}
}

// TestGatewayPusherPushURLRefused: a PushURL that is plain http to a host that
// is not this machine and not the push Service is refused by Push, which names
// it and sends nothing, even when URL is a good loopback address; the reads use
// URL and reach it.
func TestGatewayPusherPushURLRefused(t *testing.T) {
	t.Parallel()
	const url = "http://127.0.0.1:7440"
	for _, tt := range []struct {
		name    string
		pushURL string
	}{
		{name: "a host outside the cluster", pushURL: "http://gateway.example.com:7441"},
		{name: "another in-cluster Service", pushURL: "http://dolt.gt.svc:7441"},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, route := range pushRouteCalls {
				route := route
				t.Run(route.name, func(t *testing.T) {
					t.Parallel()
					root := clientTown(t)
					var got *http.Request
					pusher := GatewayPusher{
						URL:       url,
						PushURL:   tt.pushURL,
						TokenFile: clientToken(t, "tok"),
						HTTP:      gatewayDoer(t, &got),
					}
					err := route.call(t, pusher, root)
					if route.name != "push" {
						if err != nil {
							t.Fatalf("%s: %v", route.name, err)
						}
						if got == nil {
							t.Fatalf("%s sent no request", route.name)
						}
						if want := url + route.path; got.URL.String() != want {
							t.Errorf("%s went to %s, want %s", route.name, got.URL, want)
						}
						return
					}
					if err == nil {
						t.Fatalf("Push accepted the push URL %s", tt.pushURL)
					}
					if got != nil {
						t.Fatal("Push sent a request with a refused push URL")
					}
					if !strings.Contains(err.Error(), "plain http") {
						t.Errorf("error = %q, want the plain-http refusal", err)
					}
					if !strings.Contains(err.Error(), tt.pushURL) {
						t.Errorf("error = %q, want it to name the push URL %s", err, tt.pushURL)
					}
				})
			}
		})
	}
}
