package bdgateway

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestWorkspaceDriftProbeObservesAnswer: a probe observes what the gateway's answer
// to GET /v1/stats says about the workspace, as a push's answer does. A transport
// error observes nothing, and a refusal is an error the probe returns.
func TestWorkspaceDriftProbeObservesAnswer(t *testing.T) {
	t.Parallel()
	transport := errors.New("connection refused")
	tests := []struct {
		name    string
		status  int
		header  string
		body    string
		err     error
		want    WorkspaceAnswer
		wantErr bool
		seen    bool
	}{
		{
			name:   "a 200 reports the live hash",
			status: http.StatusOK,
			header: "live",
			body:   `{}`,
			want:   WorkspaceAnswer{Hash: "live"},
			seen:   true,
		},
		{
			name:   "a 200 with no header reports no hash",
			status: http.StatusOK,
			body:   `{}`,
			want:   WorkspaceAnswer{},
			seen:   true,
		},
		{
			name:    "a refusal that the workspace is not present is observed absent and returned",
			status:  http.StatusBadRequest,
			header:  "live",
			body:    `{"code":"bad_request","message":"bdgateway: the workspace is not present: no tree"}`,
			want:    WorkspaceAnswer{Hash: "live", Absent: true},
			wantErr: true,
			seen:    true,
		},
		{
			name:    "a refusal for another reason is observed not absent and returned",
			status:  http.StatusUnauthorized,
			header:  "live",
			body:    `{"code":"unauthorized","message":"bdgateway: the token is not known"}`,
			want:    WorkspaceAnswer{Hash: "live"},
			wantErr: true,
			seen:    true,
		},
		{
			name:    "a transport error observes nothing",
			err:     transport,
			wantErr: true,
			seen:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var seen []WorkspaceAnswer
			probe := GatewayPusher{
				URL:       "http://127.0.0.1:7440",
				TokenFile: clientToken(t, "s3cret"),
				HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
					if tt.err != nil {
						return nil, tt.err
					}
					resp := jsonResponse(tt.status, tt.body)
					if tt.header != "" {
						resp.Header.Set(WorkspaceHashHeader, tt.header)
					}
					return resp, nil
				}),
				Observe: func(a WorkspaceAnswer) { seen = append(seen, a) },
			}
			err := probe.Probe(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Probe() error = %v, want error %v", err, tt.wantErr)
			}
			if !tt.seen {
				if len(seen) != 0 {
					t.Fatalf("observed %+v for a probe that got no answer, want none", seen)
				}
				return
			}
			if len(seen) != 1 {
				t.Fatalf("observed %d answers, want 1", len(seen))
			}
			if seen[0] != tt.want {
				t.Errorf("observed %+v, want %+v", seen[0], tt.want)
			}
		})
	}
}

// TestWorkspaceDriftProbeSendsStatsGET: the probe reads GET /v1/stats of the gateway
// with the push's bearer token, and sends no document.
func TestWorkspaceDriftProbeSendsStatsGET(t *testing.T) {
	t.Parallel()
	var method, path, auth string
	var body int64
	probe := GatewayPusher{
		URL:       "http://127.0.0.1:7440/",
		TokenFile: clientToken(t, "s3cret"),
		HTTP: doerFunc(func(req *http.Request) (*http.Response, error) {
			method, path, auth = req.Method, req.URL.Path, req.Header.Get("Authorization")
			body = req.ContentLength
			return jsonResponse(http.StatusOK, `{}`), nil
		}),
	}
	if err := probe.Probe(context.Background()); err != nil {
		t.Fatalf("Probe() error = %v, want nil", err)
	}
	if method != http.MethodGet {
		t.Errorf("probe method = %s, want GET", method)
	}
	if path != statsPath {
		t.Errorf("probe path = %s, want %s", path, statsPath)
	}
	if auth != "Bearer s3cret" {
		t.Errorf("probe Authorization = %q, want the push's bearer token", auth)
	}
	if body > 0 {
		t.Errorf("probe sent a %d-byte body, want none", body)
	}
}

// TestWorkspaceDriftProbeRefusesBeforeSending: a probe that cannot name a gateway it
// may use, or has no token, returns the refusal and sends nothing and observes
// nothing.
func TestWorkspaceDriftProbeRefusesBeforeSending(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		url     string
		token   string
		wantErr error
	}{
		{name: "no gateway URL", url: "", token: clientToken(t, "s3cret"), wantErr: ErrNoGatewayURL},
		{name: "plain http to another host", url: "http://gateway.example.com:7440", token: clientToken(t, "s3cret"), wantErr: nil},
		{name: "no token file", url: "http://127.0.0.1:7440", token: "", wantErr: ErrNoTokenFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sent, observed := 0, 0
			probe := GatewayPusher{
				URL:       tt.url,
				TokenFile: tt.token,
				HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
					sent++
					return jsonResponse(http.StatusOK, `{}`), nil
				}),
				Observe: func(WorkspaceAnswer) { observed++ },
			}
			err := probe.Probe(context.Background())
			if err == nil {
				t.Fatal("Probe() error = nil, want a refusal")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Probe() error = %v, want %v", err, tt.wantErr)
			}
			if sent != 0 || observed != 0 {
				t.Errorf("sent %d requests and observed %d answers for a refused probe, want none", sent, observed)
			}
		})
	}
}

// unreadableProbeBody is an answer whose body cannot be read.
type unreadableProbeBody struct{}

func (unreadableProbeBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (unreadableProbeBody) Close() error             { return nil }

// TestWorkspaceDriftProbeUnreadableAnswer: a probe whose answer body cannot be read
// returns the read error and observes nothing, because the answer was not read.
func TestWorkspaceDriftProbeUnreadableAnswer(t *testing.T) {
	t.Parallel()
	observed := 0
	probe := GatewayPusher{
		URL:       "http://127.0.0.1:7440",
		TokenFile: clientToken(t, "s3cret"),
		HTTP: doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: unreadableProbeBody{}}, nil
		}),
		Observe: func(WorkspaceAnswer) { observed++ },
	}
	if err := probe.Probe(context.Background()); err == nil {
		t.Fatal("Probe() error = nil for an unreadable answer, want the read error")
	}
	if observed != 0 {
		t.Errorf("observed %d answers for an unreadable answer, want none", observed)
	}
}
