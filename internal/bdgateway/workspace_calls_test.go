package bdgateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// callsGatewayAnswer is a healthy GET /v1/calls answer: two entries, oldest first.
const callsGatewayAnswer = `{"calls":[{"ts":"2026-11-19T03:00:01Z","class":"read","caller":"mayor","verb":"show","flags":[],"beads":["gt-1"],"exit_code":0,"duration_ms":4},{"ts":"2026-11-19T03:00:02Z","class":"write","caller":"furiosa","verb":"update","flags":["--status"],"beads":["gt-2"],"exit_code":1,"duration_ms":9,"error":"boom"}],"dropped":0}`

// inProcessGateway answers the pusher's requests with handler, run through
// httptest's recorder, so the test drives the gateway's answer without a socket
// (the no-network rule for unit tests). The URL the pusher names is never dialed.
func inProcessGateway(handler http.Handler) HTTPDoer {
	return doerFunc(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Result(), nil
	})
}

// TestGatewayCallsSendsTheQueryAndReturnsEntriesInOrder is acceptance box 1:
// against the gateway's answer, Calls sends GET /v1/calls with the bearer token,
// since and limit, and returns the answer's entries in order.
func TestGatewayCallsSendsTheQueryAndReturnsEntriesInOrder(t *testing.T) {
	t.Parallel()
	const token = "s3cret"
	since := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	reqs := make(chan *http.Request, 1)
	gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reqs <- r:
		default:
		}
		_, _ = io.WriteString(w, callsGatewayAnswer)
	})
	pusher := GatewayPusher{
		URL:       "http://127.0.0.1:7440",
		TokenFile: clientToken(t, token+"\n"),
		HTTP:      inProcessGateway(gateway),
		Observe: func(WorkspaceAnswer) {
			t.Error("a calls read observed the workspace; it must not")
		},
	}
	entries, err := pusher.Calls(context.Background(), since, 5)
	if err != nil {
		t.Fatalf("Calls: %v", err)
	}
	var got *http.Request
	select {
	case got = <-reqs:
	default:
		t.Fatal("no request reached the gateway")
	}
	if got.Method != http.MethodGet || got.URL.Path != callsPath {
		t.Errorf("request = %s %s, want GET %s", got.Method, got.URL.Path, callsPath)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer "+token {
		t.Errorf("Authorization = %q, want the bearer token from the token file", auth)
	}
	if s := got.URL.Query().Get("since"); s != "2026-11-19T03:00:00Z" {
		t.Errorf("since = %q, want the RFC3339 time", s)
	}
	if l := got.URL.Query().Get("limit"); l != "5" {
		t.Errorf("limit = %q, want 5", l)
	}
	if len(entries) != 2 || entries[0].Verb != "show" || entries[1].Verb != "update" {
		t.Fatalf("entries = %+v, want the two entries oldest first", entries)
	}
	if entries[0].DurationMS != 4 || entries[1].ExitCode != 1 || entries[1].Error != "boom" {
		t.Errorf("entries = %+v, want the fields the log recorded", entries)
	}
}

// TestGatewayCallsRefusals is acceptance box 2: a 401 or 403 answer, or any other
// non-200, returns the refusal error and no entries, and a 200 whose body is not
// the calls answer returns an error and no entries.
func TestGatewayCallsRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		status   int
		body     string
		wantErr  error
		wantText string
	}{
		{name: "a 401 is the refusal", status: http.StatusUnauthorized, body: `{"code":"unauthorized","message":"unknown token"}`, wantErr: ErrGatewayRefused},
		{name: "a 403 is the refusal", status: http.StatusForbidden, body: `{"code":"forbidden","message":"read class required"}`, wantErr: ErrGatewayRefused},
		{name: "a 400 names the gateway's message", status: http.StatusBadRequest, body: `{"code":"bad_request","message":"limit is not a whole number from 1 to 500"}`, wantErr: ErrGatewayRefused, wantText: "limit is not a whole number"},
		{name: "a 200 that is not JSON is an error", status: http.StatusOK, body: "<html>a proxy page</html>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			pusher := GatewayPusher{URL: "http://127.0.0.1:7440", TokenFile: clientToken(t, "s3cret"), HTTP: inProcessGateway(gateway)}
			entries, err := pusher.Calls(context.Background(), time.Time{}, 10)
			if err == nil {
				t.Fatalf("Calls = %+v, nil error; want an error", entries)
			}
			if entries != nil {
				t.Errorf("entries = %+v with an error, want none", entries)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && errors.Is(err, ErrGatewayRefused) {
				t.Errorf("error = %v, a 200 answer is not a refusal", err)
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantText)
			}
		})
	}
}

// TestGatewayCallsNoEndpointOrToken refuses a pusher that names no gateway or no
// token file before any request is sent.
func TestGatewayCallsNoEndpointOrToken(t *testing.T) {
	t.Parallel()
	t.Run("no gateway URL is refused", func(t *testing.T) {
		t.Parallel()
		pusher := GatewayPusher{TokenFile: clientToken(t, "s3cret")}
		entries, err := pusher.Calls(context.Background(), time.Time{}, 10)
		if !errors.Is(err, ErrNoGatewayURL) || entries != nil {
			t.Errorf("Calls = %+v, %v; want ErrNoGatewayURL and no entries", entries, err)
		}
	})
	t.Run("no token file is refused", func(t *testing.T) {
		t.Parallel()
		pusher := GatewayPusher{URL: "http://127.0.0.1:7440"}
		entries, err := pusher.Calls(context.Background(), time.Time{}, 10)
		if !errors.Is(err, ErrNoTokenFile) || entries != nil {
			t.Errorf("Calls = %+v, %v; want ErrNoTokenFile and no entries", entries, err)
		}
	})
}

// TestGatewayCallsTransportFailures covers an answer that never arrives and a
// body that breaks mid-read: each is an error that names the calls read and
// returns no entries.
func TestGatewayCallsTransportFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		doer     HTTPDoer
		wantText string
	}{
		{
			name: "an unreachable gateway is an error",
			doer: doerFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection refused")
			}),
			wantText: "did not answer the calls read",
		},
		{
			name: "a body that breaks mid-read is an error",
			doer: doerFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: failingBody{errors.New("reset")}, Header: make(http.Header)}, nil
			}),
			wantText: "reading the gateway's calls answer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pusher := GatewayPusher{URL: "http://127.0.0.1:7440", TokenFile: clientToken(t, "s3cret"), HTTP: tt.doer}
			entries, err := pusher.Calls(context.Background(), time.Time{}, 10)
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantText)
			}
			if entries != nil {
				t.Errorf("entries = %+v with an error, want none", entries)
			}
		})
	}
}

// TestGatewayCallsErrorsHoldNoSecret is acceptance box 3: neither the bearer
// token nor the URL's credentials appear in any error Calls returns.
func TestGatewayCallsErrorsHoldNoSecret(t *testing.T) {
	t.Parallel()
	const token = "t0ken-value"
	secrets := []string{token, "gw-user", "hunter2"}
	assertNoSecret := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("Calls returned nil error; want a refusal")
		}
		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error names a secret it must not: %q", err)
			}
		}
	}
	t.Run("credentials in the URL are refused without echoing them", func(t *testing.T) {
		t.Parallel()
		pusher := GatewayPusher{URL: "http://gw-user:hunter2@127.0.0.1:7440", TokenFile: clientToken(t, token)}
		_, err := pusher.Calls(context.Background(), time.Time{}, 10)
		assertNoSecret(t, err)
	})
	t.Run("an unreachable gateway holds no token", func(t *testing.T) {
		t.Parallel()
		pusher := GatewayPusher{
			URL:       "http://127.0.0.1:7440",
			TokenFile: clientToken(t, token),
			HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection refused")
			}),
		}
		_, err := pusher.Calls(context.Background(), time.Time{}, 10)
		assertNoSecret(t, err)
	})
	t.Run("a refusal holds no token", func(t *testing.T) {
		t.Parallel()
		pusher := GatewayPusher{
			URL:       "http://127.0.0.1:7440",
			TokenFile: clientToken(t, token),
			HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(http.StatusUnauthorized, `{"code":"unauthorized","message":"unknown token"}`), nil
			}),
		}
		_, err := pusher.Calls(context.Background(), time.Time{}, 10)
		assertNoSecret(t, err)
	})
}
