package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// forgejoStubTransport answers every health request with the status or error
// the test set, so no test opens a socket.
type forgejoStubTransport struct {
	req    *http.Request
	status int
	err    error
}

func (s *forgejoStubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.req = r
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Status:     fmt.Sprintf("%d %s", s.status, http.StatusText(s.status)),
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
	}, nil
}

// probesFor builds the production health probe over a fixed origin and the
// stub transport.
func probesFor(origin string, originErr error, tr *forgejoStubTransport) systemHostProbes {
	return systemHostProbes{
		forgejoBaseURL: func() (string, error) { return origin, originErr },
		forgejoHTTP:    &http.Client{Transport: tr},
	}
}

// One unanswered health read is an info line; attentionDockerProbeDownBeats in
// a row raises the High host.forgejo_down naming the origin and no credential,
// and a healthy read clears both.
func TestHostForgejoHealthRaisesAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	h.forgejoErr = errors.New("forgejo health https://forgejo.example/api/healthz: dial tcp: connection refused")
	f.src.host = h
	wakes := func() []attention.Event {
		var out []attention.Event
		for _, e := range f.events(t) {
			if e.Wake {
				out = append(out, e)
			}
		}
		return out
	}

	st := f.tick(t, now.Add(time.Minute))
	probe := itemByKey(t, st, hostProbeFailedPrefix+hostProbeForgejo)
	if probe.Severity != attention.SeverityLow || probe.Kind != attention.KindHostProbe {
		t.Errorf("one failed read = %+v, want a low host-probe item", probe)
	}
	if items := hostItems(st); len(items) != 1 {
		t.Errorf("host items after one failed read = %v, want only the probe line", hostKeys(st))
	}
	if _, ok := attention.Find(st, hostRuleForgejoDown); ok {
		t.Errorf("host items = %v, want no %s yet", hostKeys(st), hostRuleForgejoDown)
	}
	if got := wakes(); len(got) != 0 {
		t.Fatalf("one failed read wake events = %+v, want none", got)
	}

	for beat := 2; beat < attentionDockerProbeDownBeats; beat++ {
		st = f.tick(t, now.Add(time.Duration(beat)*time.Minute))
		if _, ok := attention.Find(st, hostRuleForgejoDown); ok {
			t.Fatalf("beat %d raised %s before the configured run", beat, hostRuleForgejoDown)
		}
	}

	st = f.tick(t, now.Add(time.Duration(attentionDockerProbeDownBeats)*time.Minute))
	down := itemByKey(t, st, hostRuleForgejoDown)
	if down.Severity != attention.SeverityHigh || down.Kind != attention.KindHostForgejo {
		t.Errorf("%s = %+v, want a high host-forgejo item", hostRuleForgejoDown, down)
	}
	if !strings.Contains(down.Summary, "https://forgejo.example/api/healthz") {
		t.Errorf("summary = %q, want it to name the origin", down.Summary)
	}
	if !strings.Contains(down.Summary, fmt.Sprintf("%d heartbeats", attentionDockerProbeDownBeats)) {
		t.Errorf("summary = %q, want it to name the run length", down.Summary)
	}
	if strings.Contains(down.Summary, "@") || strings.Contains(down.Summary, "\n") {
		t.Errorf("summary = %q, want one line and no credential", down.Summary)
	}
	got := wakes()
	if len(got) != 1 || got[0].Rule != attention.RuleHostForgejoDown || got[0].Handled != attention.HandledOperatorAction || got[0].State != attention.EventNew {
		t.Fatalf("wake events = %+v, want one raised host.forgejo_down", got)
	}

	f.tick(t, now.Add(time.Duration(attentionDockerProbeDownBeats+1)*time.Minute))
	if n := len(wakes()); n != 1 {
		t.Errorf("wake events after a further failed tick = %d, want the one", n)
	}

	h.forgejoErr = nil
	st = f.tick(t, now.Add(time.Duration(attentionDockerProbeDownBeats+2)*time.Minute))
	if items := hostItems(st); len(items) != 0 {
		t.Errorf("host items = %v, want both cleared once the instance answers", hostKeys(st))
	}
}

// A town where no rig the landing worker serves names a Forgejo has nothing to
// watch: the probe is not a failure, and an item a configured town raised is
// dropped rather than alarmed on for good.
func TestHostForgejoNotConfiguredRaisesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	h := healthyHost()
	f.src.host = h

	st := f.tick(t, now)
	if items := hostItems(st); len(items) != 0 {
		t.Fatalf("host items in a town with no Forgejo = %v, want none", hostKeys(st))
	}

	h.forgejoErr = errors.New("forgejo health https://forgejo.example/api/healthz: connection refused")
	for beat := 1; beat <= attentionDockerProbeDownBeats; beat++ {
		st = f.tick(t, now.Add(time.Duration(beat)*time.Minute))
	}
	itemByKey(t, st, hostRuleForgejoDown)

	h.forgejoErr = errForgejoNotConfigured
	st = f.tick(t, now.Add(time.Duration(attentionDockerProbeDownBeats+1)*time.Minute))
	if items := hostItems(st); len(items) != 0 {
		t.Errorf("host items = %v, want the raised item cleared when the town runs no Forgejo", hostKeys(st))
	}
}

// The production health read asks the origin's /api/healthz, bounds the request
// by attentionForgejoHealthTimeout, carries no credential, and refuses every
// answer but 200.
func TestHostForgejoHealthProbe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		origin       string
		originErr    error
		status       int
		transportErr error
		// wantErr is a substring of the error, "" for a healthy read.
		wantErr string
		// noRequest is set for a probe that must refuse before it can ask.
		noRequest bool
	}{
		{name: "healthy", origin: "https://forgejo.example", status: http.StatusOK},
		{name: "a trailing slash on the origin", origin: "https://forgejo.example/", status: http.StatusOK},
		{name: "a 503 is not healthy", origin: "https://forgejo.example", status: http.StatusServiceUnavailable, wantErr: "503 Service Unavailable"},
		{name: "an unreachable instance", origin: "https://forgejo.example", transportErr: errors.New("dial tcp: connection refused"), wantErr: "connection refused"},
		{name: "an unreadable setting", originErr: errors.New("reading the rig settings: permission denied"), wantErr: "permission denied", noRequest: true},
		{name: "a town with no Forgejo", origin: "", wantErr: errForgejoNotConfigured.Error(), noRequest: true},
		{name: "an origin no request can name", origin: "://forgejo.example", wantErr: "missing protocol scheme", noRequest: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := &forgejoStubTransport{status: tt.status, err: tt.transportErr}
			p := probesFor(tt.origin, tt.originErr, tr)
			before := time.Now()
			err := p.ForgejoHealthy(context.Background())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ForgejoHealthy = %v, want healthy", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ForgejoHealthy = %v, want it to name %q", err, tt.wantErr)
			}
			if tt.noRequest {
				if tr.req != nil {
					t.Errorf("a probe that must refuse made a request to %s", tr.req.URL)
				}
				return
			}
			if tr.req == nil {
				t.Fatal("no request was made")
			}
			if tr.req.Method != http.MethodGet || tr.req.URL.Path != forgejoHealthPath {
				t.Errorf("request = %s %s, want GET %s", tr.req.Method, tr.req.URL, forgejoHealthPath)
			}
			if tr.req.URL.User != nil || tr.req.Header.Get("Authorization") != "" {
				t.Errorf("request carries a credential: %s", tr.req.URL)
			}
			deadline, ok := tr.req.Context().Deadline()
			if !ok {
				t.Fatal("request has no deadline")
			}
			if d := deadline.Sub(before); d > attentionForgejoHealthTimeout+time.Second || d < attentionForgejoHealthTimeout-time.Second {
				t.Errorf("request deadline is %v away, want about %v", d, attentionForgejoHealthTimeout)
			}
		})
	}
}

// The probe built with no Forgejo URL source is a wiring error, never a
// healthy answer.
func TestHostForgejoHealthProbeNeedsAURLSource(t *testing.T) {
	t.Parallel()
	err := (systemHostProbes{}).ForgejoHealthy(context.Background())
	if err == nil {
		t.Fatal("ForgejoHealthy with no URL source = nil, want an error")
	}
}

// The origin a remote URL names keeps the scheme and host and drops the path,
// the query and any user info, so no credential can reach a summary.
func TestForgejoOriginFromRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, remote, want string
		wantErr            bool
	}{
		{name: "a repository URL", remote: "https://forgejo.example/gastown/gastown.git", want: "https://forgejo.example"},
		{name: "a port", remote: "http://127.0.0.1:3000/acme/rig", want: "http://127.0.0.1:3000"},
		{name: "user info is dropped", remote: "https://bot:s3cret@forgejo.example/acme/rig.git", want: "https://forgejo.example"},
		{name: "surrounding space", remote: "  https://forgejo.example/acme/rig.git  ", want: "https://forgejo.example"},
		{name: "no scheme", remote: "forgejo.example/acme/rig.git", wantErr: true},
		{name: "no host", remote: "https://", wantErr: true},
		{name: "empty", remote: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := forgejoOriginFromRemoteURL(tt.remote)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("forgejoOriginFromRemoteURL(%q) = %q, %v; want %q, err %v", tt.remote, got, err, tt.want, tt.wantErr)
			}
		})
	}
}
