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
// it, so no socket is opened. It is https, so the runs need no -allow-http.
const benchURL = "https://gateway.test"

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

// inPodBDPath is the in-pod bd binary the runs name. The fake pod refuses any
// other path, so a run that execs the wrong binary fails.
const inPodBDPath = "/fake/bd"

// fakePod is the in-pod bd for a run's baseline. It advances the fake clock by
// each call's latency, so the baseline is measured on the same clock as the
// gateway, and it can fail its failAt-th call (0 for none).
type fakePod struct {
	clock   *fakeClock
	latency time.Duration
	failAt  int

	mu    sync.Mutex
	calls int
}

func (p *fakePod) run(bdPath string, _ []string) error {
	p.mu.Lock()
	p.calls++
	k := p.calls
	p.mu.Unlock()
	if bdPath != inPodBDPath {
		return errors.New("the run exec'd the wrong bd binary")
	}
	if k == p.failAt {
		return errors.New("exit status 1")
	}
	p.clock.Advance(p.latency)
	return nil
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
		// podLatency is the in-pod bd's latency per call; zero is the design's warm
		// 40 ms. podFailAt, when non-zero, fails the in-pod bd's call with that number.
		podLatency time.Duration
		podFailAt  int
		// wantPodCalls is the calls the in-pod bd ran for the baseline.
		wantPodCalls int
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
				return []string{"-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-url is required",
		},
		{
			name: "refuses a URL that is not http",
			args: func(t *testing.T, _ string) []string {
				return []string{"-url", "ftp://gateway.example", "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-url must be an http or https URL with a host",
		},
		{
			name: "refuses a URL with no host",
			args: func(t *testing.T, _ string) []string {
				return []string{"-url", "http://", "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-url must be an http or https URL with a host",
		},
		{
			name: "refuses a run with no token file",
			args: func(_ *testing.T, url string) []string {
				return []string{"-url", url, "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-token-file is required",
		},
		{
			name: "refuses a token file that is missing",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", filepath.Join(t.TempDir(), "missing"), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "read the token file",
		},
		{
			name: "refuses a token file that holds only space",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, " \n\t"), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
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
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:         steady(40),
			wantExit:     0,
			wantShows:    200,
			wantLists:    50,
			wantPodCalls: 250,
			wantOut:      []string{"p50: 40ms", "p95: 40ms", "p99: 40ms", "PASS"},
		},
		{
			name: "is over budget when the p95 overhead passes 40 ms",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:         slowEvery10,
			wantExit:     1,
			wantShows:    200,
			wantLists:    50,
			wantPodCalls: 250,
			wantOut:      []string{"p50: 40ms", "p95: 100ms", "OVER BUDGET"},
		},
		{
			name: "is over budget when one call runs 200 ms past the median",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:         oneSlowCall,
			wantExit:     1,
			wantShows:    200,
			wantLists:    50,
			wantPodCalls: 250,
			wantOut:      []string{"p95: 40ms", "max: 300ms", "OVER BUDGET"},
		},
		{
			name: "a refused HTTP status fails the run with no verdict",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:      gatewayPlan{httpStatus: http.StatusServiceUnavailable},
			wantExit:  1,
			wantShows: 1,
			wantErr:   "the gateway answered HTTP 503",
		},
		{
			name: "a gateway error frame fails the run and names its code",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:      refused,
			wantExit:  1,
			wantShows: 1,
			wantErr:   "the gateway refused the call (forbidden)",
		},
		{
			name: "an answer with no exit frame fails the run",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:      truncated,
			wantExit:  1,
			wantShows: 1,
			wantErr:   "ended before an exit status",
		},
		{
			name: "a gateway that does not answer fails the run before it is called",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:     unreachable,
			wantExit: 1,
			wantErr:  "the gateway did not answer",
		},
		{
			name: "an answer with an unknown frame kind fails the run",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:      unknownKind,
			wantExit:  1,
			wantShows: 1,
			wantErr:   "read the answer",
		},
		{
			name: "an exit frame that is not a status fails the run",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:      badExit,
			wantExit:  1,
			wantShows: 1,
			wantErr:   "read the answer",
		},
		{
			name: "a bd exit status other than 0 fails the run",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:      exitOneAtSix,
			wantExit:  1,
			wantShows: 6,
			wantErr:   "bd exited 1",
		},
		{
			name: "refuses a run with no in-pod bd and sends nothing",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc"}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-in-pod-bd is required",
		},
		{
			name: "refuses plain http to a host that is not loopback",
			args: func(t *testing.T, _ string) []string {
				return []string{"-url", "http://gateway.example", "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:     steady(40),
			wantExit: 2,
			wantErr:  "-url must be https for a host that is not loopback",
		},
		{
			name: "passes plain http to a loopback host",
			args: func(t *testing.T, _ string) []string {
				return []string{"-url", "http://127.0.0.1:8080", "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:         steady(40),
			wantExit:     0,
			wantShows:    200,
			wantLists:    50,
			wantPodCalls: 250,
			wantOut:      []string{"p50: 40ms", "PASS"},
		},
		{
			name: "passes plain http to a named host when -allow-http is set",
			args: func(t *testing.T, _ string) []string {
				return []string{"-url", "http://gateway.example", "-allow-http", "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:         steady(40),
			wantExit:     0,
			wantShows:    200,
			wantLists:    50,
			wantPodCalls: 250,
			wantOut:      []string{"PASS"},
		},
		{
			name: "measures the baseline on the in-pod bd, so a slower in-pod bd passes",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:         steady(100),
			podLatency:   100 * time.Millisecond,
			wantExit:     0,
			wantShows:    200,
			wantLists:    50,
			wantPodCalls: 250,
			wantOut:      []string{"p50: 100ms", "in-pod baseline: p50 100ms, p95 100ms", "PASS"},
		},
		{
			name: "a failed in-pod call fails the run with no verdict",
			args: func(t *testing.T, url string) []string {
				return []string{"-url", url, "-token-file", writeToken(t, testToken), "-bead", "gt-abc", "-in-pod-bd", inPodBDPath}
			},
			plan:         steady(40),
			podFailAt:    3,
			wantExit:     1,
			wantShows:    200,
			wantLists:    50,
			wantPodCalls: 3,
			wantErr:      "the in-pod baseline: call 3 (show)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeClock{t: time.Unix(0, 0)}
			gw := &fakeGateway{token: testToken, clock: clock, plan: tc.plan}
			client := &http.Client{Transport: gatewayTransport{gw: gw}}
			podLatency := tc.podLatency
			if podLatency == 0 {
				// The design's warm in-pod bd call, 35 to 40 ms.
				podLatency = 40 * time.Millisecond
			}
			pod := &fakePod{clock: clock, latency: podLatency, failAt: tc.podFailAt}

			var stdout, stderr bytes.Buffer
			got := run(tc.args(t, benchURL), &stdout, &stderr, clock.Now, client, pod.run)

			if got != tc.wantExit {
				t.Errorf("exit = %d, want %d; stderr: %q", got, tc.wantExit, stderr.String())
			}
			gw.mu.Lock()
			shows, lists := gw.shows, gw.lists
			gw.mu.Unlock()
			if shows != tc.wantShows || lists != tc.wantLists {
				t.Errorf("gateway got %d show and %d list calls, want %d and %d", shows, lists, tc.wantShows, tc.wantLists)
			}
			pod.mu.Lock()
			podCalls := pod.calls
			pod.mu.Unlock()
			if podCalls != tc.wantPodCalls {
				t.Errorf("in-pod bd ran %d calls, want %d", podCalls, tc.wantPodCalls)
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
