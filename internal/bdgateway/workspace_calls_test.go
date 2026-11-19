package bdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	page, err := pusher.Calls(context.Background(), since, 5)
	if err != nil {
		t.Fatalf("Calls: %v", err)
	}
	entries := page.Entries
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
			page, err := pusher.Calls(context.Background(), time.Time{}, 10)
			if err == nil {
				t.Fatalf("Calls = %+v, nil error; want an error", page)
			}
			if page.Entries != nil {
				t.Errorf("entries = %+v with an error, want none", page.Entries)
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
		page, err := pusher.Calls(context.Background(), time.Time{}, 10)
		if !errors.Is(err, ErrNoGatewayURL) || page.Entries != nil {
			t.Errorf("Calls = %+v, %v; want ErrNoGatewayURL and no entries", page, err)
		}
	})
	t.Run("no token file is refused", func(t *testing.T) {
		t.Parallel()
		pusher := GatewayPusher{URL: "http://127.0.0.1:7440"}
		page, err := pusher.Calls(context.Background(), time.Time{}, 10)
		if !errors.Is(err, ErrNoTokenFile) || page.Entries != nil {
			t.Errorf("Calls = %+v, %v; want ErrNoTokenFile and no entries", page, err)
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
			page, err := pusher.Calls(context.Background(), time.Time{}, 10)
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantText)
			}
			if page.Entries != nil {
				t.Errorf("entries = %+v with an error, want none", page.Entries)
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

// maxSizeEntry is one call entry at the log writer's field bounds: class and
// caller maxFieldBytes each, verb 32 bytes, maxLoggedItems flags and bead ids,
// and an error of maxErrorBytes. index is carried in the first bead id, so a
// page's last entry is told from its first.
func maxSizeEntry(index int) CallEntry {
	entry := CallEntry{
		TS:         "2026-11-19T03:00:00Z",
		Class:      strings.Repeat("c", maxFieldBytes),
		Caller:     strings.Repeat("a", maxFieldBytes),
		Verb:       strings.Repeat("v", 32),
		ExitCode:   1,
		DurationMS: 1234567890,
		Error:      strings.Repeat("e", maxErrorBytes),
	}
	for i := 0; i < maxLoggedItems; i++ {
		entry.Flags = append(entry.Flags, fmt.Sprintf("-f%02d-%s", i, strings.Repeat("f", 58)))
		entry.Beads = append(entry.Beads, fmt.Sprintf("gt-%04d-%s", index, strings.Repeat("b", 50)))
	}
	return entry
}

// callsPusher answers every request with body. The gateway's URL is never
// dialed: inProcessGateway runs the handler through a recorder.
func callsPusher(t *testing.T, body []byte) GatewayPusher {
	t.Helper()
	gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	return GatewayPusher{URL: "http://127.0.0.1:7440", TokenFile: clientToken(t, "s3cret"), HTTP: inProcessGateway(gateway)}
}

// TestGatewayCallsReturnsAFullPage is rework finding 1: a full page of entries
// at the log writer's field bounds is returned whole, not truncated at a
// smaller read bound.
func TestGatewayCallsReturnsAFullPage(t *testing.T) {
	t.Parallel()
	entries := make([]CallEntry, maxCallsLimit)
	for i := range entries {
		entries[i] = maxSizeEntry(i)
	}
	answer, err := json.Marshal(callsBody{Calls: entries})
	if err != nil {
		t.Fatalf("marshal the answer: %v", err)
	}
	pusher := callsPusher(t, answer)
	page, err := pusher.Calls(context.Background(), time.Time{}, maxCallsLimit)
	if err != nil {
		t.Fatalf("Calls: %v", err)
	}
	if len(page.Entries) != maxCallsLimit {
		t.Fatalf("entries = %d, want the whole %d-entry page", len(page.Entries), maxCallsLimit)
	}
	want := fmt.Sprintf("gt-%04d-%s", maxCallsLimit-1, strings.Repeat("b", 50))
	if got := page.Entries[maxCallsLimit-1].Beads[0]; got != want {
		t.Errorf("last entry's first bead = %q, want %q", got, want)
	}
}

// TestGatewayCallsRefusesAnAnswerOverTheReadLimit is the other half of rework
// finding 1: an answer past the read bound is an error, never a truncated page
// handed to the decoder.
func TestGatewayCallsRefusesAnAnswerOverTheReadLimit(t *testing.T) {
	t.Parallel()
	over := make([]CallEntry, maxCallsLimit)
	for i := range over {
		over[i] = CallEntry{TS: "2026-11-19T03:00:00Z", Class: "read", Caller: "mayor", Verb: "show", Error: strings.Repeat("e", 10<<10)}
	}
	answer, err := json.Marshal(callsBody{Calls: over})
	if err != nil {
		t.Fatalf("marshal the answer: %v", err)
	}
	if len(answer) <= callsResponseLimit {
		t.Fatalf("answer = %d bytes, the fixture must cross the %d-byte bound", len(answer), callsResponseLimit)
	}
	page, err := callsPusher(t, answer).Calls(context.Background(), time.Time{}, maxCallsLimit)
	if err == nil {
		t.Fatalf("Calls = %d entries, nil error; want the over-limit error", len(page.Entries))
	}
	if !strings.Contains(err.Error(), "over 4 MiB") {
		t.Errorf("error = %v, want it to name the 4 MiB bound", err)
	}
	if page.Entries != nil {
		t.Errorf("entries = %+v with an error, want none", page.Entries)
	}
}

// TestGatewayCallsRefusalNamesTheRead is rework finding 2: a refusal with no
// payload names the calls read, not the pushed workspace, and still matches
// ErrGatewayRefused.
func TestGatewayCallsRefusalNamesTheRead(t *testing.T) {
	t.Parallel()
	gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<html>forbidden</html>")
	})
	pusher := GatewayPusher{URL: "http://127.0.0.1:7440", TokenFile: clientToken(t, "s3cret"), HTTP: inProcessGateway(gateway)}
	page, err := pusher.Calls(context.Background(), time.Time{}, 10)
	if err == nil {
		t.Fatal("Calls returned nil error; want a refusal")
	}
	if !errors.Is(err, ErrGatewayRefused) {
		t.Errorf("error = %v, want it to match ErrGatewayRefused", err)
	}
	if !strings.Contains(err.Error(), "calls read") {
		t.Errorf("error = %q, want it to name the calls read", err)
	}
	if strings.Contains(err.Error(), "workspace") {
		t.Errorf("error = %q, it names a workspace the read did not store", err)
	}
	if page.Entries != nil {
		t.Errorf("entries = %+v with an error, want none", page.Entries)
	}
}

// TestGatewayCallsAnswerShape is rework finding 3: a 200 answer with no calls
// field is an error, while a present empty array is a real empty log.
func TestGatewayCallsAnswerShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "no calls field is an error", body: `{"dropped":0}`, wantErr: true},
		{name: "a null calls field is an error", body: `{"calls":null,"dropped":0}`, wantErr: true},
		{name: "an empty array is an empty log", body: `{"calls":[],"dropped":0}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			page, err := callsPusher(t, []byte(tt.body)).Calls(context.Background(), time.Time{}, 10)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "no calls field") {
					t.Fatalf("error = %v, want the missing-calls-field error", err)
				}
				if page.Entries != nil {
					t.Errorf("entries = %+v with an error, want none", page.Entries)
				}
				return
			}
			if err != nil {
				t.Fatalf("Calls: %v", err)
			}
			if page.Entries == nil || len(page.Entries) != 0 {
				t.Errorf("entries = %#v, want a present empty list", page.Entries)
			}
		})
	}
}

// TestGatewayCallsCarriesDropped is rework finding 4: the answer's dropped
// counter comes back with the page, so a reader can tell a quiet window from a
// log that lost lines.
func TestGatewayCallsCarriesDropped(t *testing.T) {
	t.Parallel()
	page, err := callsPusher(t, []byte(`{"calls":[],"dropped":7}`)).Calls(context.Background(), time.Time{}, 10)
	if err != nil {
		t.Fatalf("Calls: %v", err)
	}
	if page.Dropped != 7 {
		t.Errorf("dropped = %d, want 7", page.Dropped)
	}
}
