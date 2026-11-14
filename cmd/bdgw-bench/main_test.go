package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// benchURL is the gateway address the runs name. The in-process transport answers
// it, so no socket is opened.
const benchURL = "http://gateway.test"

// gatewayTransport answers each request by running the fake gateway's handler in
// process, so the client's real request path runs without a network.
type gatewayTransport struct{ gw *fakeGateway }

func (tr gatewayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if tr.gw.plan.transportErr {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errors.New("connection refused")
	}
	rec := httptest.NewRecorder()
	tr.gw.ServeHTTP(rec, req)
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return rec.Result(), nil
}

// testToken is the read-class token the fake gateway accepts. The run must never
// print it, so every case checks the output for it.
const testToken = "read-token-0123456789abcdef0123456789"

// fakeClock is the clock the bench reads its latencies from. The fake gateway
// advances it by each call's latency, so a call measures exactly that latency.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// gatewayPlan says how the fake gateway answers the k-th call it receives, counting
// from 0 across every verb.
type gatewayPlan struct {
	// httpStatus, when non-zero, answers every call with that HTTP status.
	httpStatus int
	// latency is the time the k-th call takes on the fake clock.
	latency func(k int) time.Duration
	// exit is bd's exit status for the k-th call.
	exit func(k int) int
	// refusal answers with a gateway error frame instead of bd's output.
	refusal bool
	// truncate ends the answer after bd's output, before any exit frame.
	truncate bool
	// rawAnswer, when non-nil, is written in place of the frames: bytes a real
	// gateway would never send, for the frame decoder's refusals.
	rawAnswer []byte
	// transportErr fails the request before it reaches the gateway.
	transportErr bool
}

// frame returns the bytes of one response frame: its kind, a four-byte big-endian
// length and the payload.
func frame(kind byte, payload string) []byte {
	return append([]byte{kind, 0, 0, 0, byte(len(payload))}, payload...)
}

// fakeGateway is an in-process gateway for POST /v1/exec. It checks the bearer
// token, counts the calls by verb and answers them as the plan says.
type fakeGateway struct {
	token string
	clock *fakeClock
	plan  gatewayPlan

	mu    sync.Mutex
	shows int
	lists int
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/exec" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+g.token {
		http.Error(w, "not authorized", http.StatusUnauthorized)
		return
	}
	req, _, err := bdgateway.ReadRequest(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	k := g.shows + g.lists
	switch req.Argv[0] {
	case "show":
		g.shows++
	case "list":
		g.lists++
	}
	g.mu.Unlock()

	if g.plan.httpStatus != 0 {
		w.WriteHeader(g.plan.httpStatus)
		return
	}
	g.clock.Advance(g.plan.latency(k))
	if g.plan.rawAnswer != nil {
		_, _ = w.Write(g.plan.rawAnswer)
		return
	}
	if g.plan.refusal {
		payload, _ := json.Marshal(bdgateway.ErrorPayload{Code: "forbidden", Message: "the verb is not allowed"})
		_ = bdgateway.WriteFrame(w, bdgateway.Frame{Kind: bdgateway.FrameError, Payload: payload})
		_ = bdgateway.WriteFrame(w, bdgateway.ExitFrame(125))
		return
	}
	_ = bdgateway.WriteFrame(w, bdgateway.Frame{Kind: bdgateway.FrameStdout, Payload: []byte("{}\n")})
	if g.plan.truncate {
		return
	}
	_ = bdgateway.WriteFrame(w, bdgateway.ExitFrame(g.plan.exit(k)))
}

// steady is a plan where every call takes ms milliseconds and bd exits 0.
func steady(ms int) gatewayPlan {
	return gatewayPlan{
		latency: func(int) time.Duration { return time.Duration(ms) * time.Millisecond },
		exit:    func(int) int { return 0 },
	}
}

// writeToken writes content to a token file under the test's temporary directory.
func writeToken(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func TestBenchRun(t *testing.T) {
	t.Parallel()

	slowEvery10 := steady(40)
	slowEvery10.latency = func(k int) time.Duration {
		if k%10 == 0 {
			return 100 * time.Millisecond
		}
		return 40 * time.Millisecond
	}
	oneSlowCall := steady(40)
	oneSlowCall.latency = func(k int) time.Duration {
		if k == 0 {
			return 300 * time.Millisecond
		}
		return 40 * time.Millisecond
	}
	exitOneAtSix := steady(40)
	exitOneAtSix.exit = func(k int) int {
		if k == 5 {
			return 1
		}
		return 0
	}
	refused := steady(40)
	refused.refusal = true
	truncated := steady(40)
	truncated.truncate = true
	unknownKind := steady(40)
	unknownKind.rawAnswer = frame(9, "x")
	badExit := steady(40)
	badExit.rawAnswer = frame(byte(bdgateway.FrameExit), "x")
	unreachable := steady(40)
	unreachable.transportErr = true

	cases := []struct {
		name string
		// args builds the command line; url is the gateway address.
		args func(t *testing.T, url string) []string
		plan gatewayPlan
		// wantExit is the status run returns.
		wantExit int
		// wantShows and wantLists are the calls the gateway received.
		wantShows, wantLists int
		// wantErr, when set, is a substring stderr must hold; a failed run prints
		// no report on stdout.
		wantErr string
		// wantOut are substrings stdout must hold on a run that reached a verdict.
		wantOut []string
	}{
		{
			name: "refuses an unknown flag",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-shows", "5"}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "flag provided but not defined",
		},
		{
			name: "refuses a stray argument",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "extra"}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  `unexpected argument "extra"`,
		},
		{
			name: "refuses a run with no URL and sends nothing",
			args: func(t *testing.T, _ string) []string {
				return []string{"-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-url is required",
		},
		{
			name: "refuses a URL that is not http",
			args: func(t *testing.T, _ string) []string {
				return []string{"-url", "ftp://gateway.example", "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-url must be an http or https URL with a host",
		},
		{
			name: "refuses a URL with no host",
			args: func(t *testing.T, _ string) []string {
				return []string{"-url", "http://", "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-url must be an http or https URL with a host",
		},
		{
			name:     "refuses a run with no token file",
			args:     func(_ *testing.T, url string) []string { return []string{"-url", url, "-bead", "gt-abc"} },
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-token-file is required",
		},
		{
			name: "refuses a token file that is missing",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", filepath.Join(t.TempDir(), "missing"), "-bead", "gt-abc"}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "read the token file",
		},
		{
			name: "refuses a token file that holds only space",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, " \n\t"), "-bead", "gt-abc"}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "is empty",
		},
		{
			name: "refuses a run with no bead",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken)}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-bead is required",
		},
		{
			name: "passes at the in-pod baseline",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:      steady(40),
			wantExit:  0,
			wantShows: 200,
			wantLists: 50,
			wantOut:   []string{"p50: 40ms", "p95: 40ms", "p99: 40ms", "PASS"},
		},
		{
			name: "is over budget when the p95 overhead passes 40 ms",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:      slowEvery10,
			wantExit:  1,
			wantShows: 200,
			wantLists: 50,
			wantOut:   []string{"p50: 40ms", "p95: 100ms", "OVER BUDGET"},
		},
		{
			name: "is over budget when one call runs 200 ms past the median",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:      oneSlowCall,
			wantExit:  1,
			wantShows: 200,
			wantLists: 50,
			wantOut:   []string{"p95: 40ms", "max: 300ms", "OVER BUDGET"},
		},
		{
			name: "a refused HTTP status fails the run with no verdict",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:      gatewayPlan{httpStatus: http.StatusServiceUnavailable},
			wantExit:  1,
			wantShows: 1,
			wantErr:   "the gateway answered HTTP 503",
		},
		{
			name: "a gateway error frame fails the run and names its code",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:      refused,
			wantExit:  1,
			wantShows: 1,
			wantErr:   "the gateway refused the call (forbidden)",
		},
		{
			name: "an answer with no exit frame fails the run",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:      truncated,
			wantExit:  1,
			wantShows: 1,
			wantErr:   "ended before an exit status",
		},
		{
			name: "a gateway that does not answer fails the run before it is called",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:     unreachable,
			wantExit: 1,
			wantErr:  "the gateway did not answer",
		},
		{
			name: "an answer with an unknown frame kind fails the run",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:      unknownKind,
			wantExit:  1,
			wantShows: 1,
			wantErr:   "read the answer",
		},
		{
			name: "an exit frame that is not a status fails the run",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:      badExit,
			wantExit:  1,
			wantShows: 1,
			wantErr:   "read the answer",
		},
		{
			name: "a bd exit status other than 0 fails the run",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:      exitOneAtSix,
			wantExit:  1,
			wantShows: 6,
			wantErr:   "bd exited 1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeClock{t: time.Unix(0, 0)}
			gw := &fakeGateway{token: testToken, clock: clock, plan: tc.plan}
			client := &http.Client{Transport: gatewayTransport{gw: gw}}

			var stdout, stderr bytes.Buffer
			got := run(tc.args(t, benchURL), &stdout, &stderr, clock.Now, client)

			if got != tc.wantExit {
				t.Errorf("exit = %d, want %d; stderr: %q", got, tc.wantExit, stderr.String())
			}
			gw.mu.Lock()
			shows, lists := gw.shows, gw.lists
			gw.mu.Unlock()
			if shows != tc.wantShows || lists != tc.wantLists {
				t.Errorf("gateway got %d show and %d list calls, want %d and %d", shows, lists, tc.wantShows, tc.wantLists)
			}
			if tc.wantErr != "" {
				if !strings.Contains(stderr.String(), tc.wantErr) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantErr)
				}
				if stdout.Len() != 0 {
					t.Errorf("a failed run printed a report: %q", stdout.String())
				}
			} else if stderr.Len() != 0 {
				t.Errorf("a run that reached a verdict wrote to stderr: %q", stderr.String())
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
				}
			}
			if strings.Contains(stdout.String()+stderr.String(), testToken) {
				t.Errorf("the run printed the token")
			}
		})
	}
}
