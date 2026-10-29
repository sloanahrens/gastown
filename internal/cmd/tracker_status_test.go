package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

const trackerStatusGolden = "testdata/tracker_status_healthy.golden"

// compareGolden reports whether the file at path holds got. The golden is the
// expected screen, so the check only ever reads it: a test run never rewrites
// a file in the repo, and the run's behaviour does not depend on a switch.
func compareGolden(path, got string) error {
	want, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if got != string(want) {
		return fmt.Errorf("output differs from %s:\n--- got\n%s--- want\n%s", path, got, want)
	}
	return nil
}

// The golden comparison fails on a difference, passes on a match, and fails on
// a golden it cannot read.
func TestCompareGolden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		before   string // the golden's content; empty means no file
		got      string
		wantErr  string
		wantFile string // the golden's content after the run; empty means no file
	}{
		{name: "a match passes", before: "screen\n", got: "screen\n", wantFile: "screen\n"},
		{name: "a difference fails", before: "screen\n", got: "other\n", wantErr: "differs", wantFile: "screen\n"},
		{name: "a missing golden fails", got: "screen\n", wantErr: "reading"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "golden")
			if tt.before != "" {
				if err := os.WriteFile(path, []byte(tt.before), 0o644); err != nil {
					t.Fatalf("staging the golden: %v", err)
				}
			}
			err := compareGolden(path, tt.got)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("compareGolden = %v, want an error naming %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("compareGolden = %v, want a match", err)
			}
			after, err := os.ReadFile(path)
			if tt.wantFile == "" {
				if err == nil {
					t.Errorf("golden holds %q after the run, want no file", after)
				}
				return
			}
			if err != nil {
				t.Fatalf("reading the golden after the run: %v", err)
			}
			if string(after) != tt.wantFile {
				t.Errorf("golden holds %q after the run, want %q", after, tt.wantFile)
			}
		})
	}
}

// trackerStatusURL is the endpoint the tests name. Nothing listens on it: the
// tests answer through an in-process transport, so no socket is opened.
const trackerStatusURL = "https://tracker.test:7433"

// trackerStatusMetrics is a /metrics page in the shape the tracker server
// renders: the request family, the latency histograms, and the actor and tag
// families the screen does not read.
const trackerStatusMetrics = `# HELP tracker_requests_total Authenticated requests by registered route and status.
# TYPE tracker_requests_total counter
tracker_requests_total{route="GET /readyz",status="200"} 12
tracker_requests_total{route="GET /v1/stats",status="200"} 4
tracker_requests_total{route="POST /v1/issues:list",status="200"} 30
# HELP tracker_request_seconds Request latency in seconds by registered route.
# TYPE tracker_request_seconds histogram
tracker_request_seconds_bucket{route="GET /readyz",le="0.005"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="0.01"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="0.025"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="0.05"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="0.1"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="0.25"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="0.5"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="1"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="2.5"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="5"} 12
tracker_request_seconds_bucket{route="GET /readyz",le="+Inf"} 12
tracker_request_seconds_sum{route="GET /readyz"} 0.03
tracker_request_seconds_count{route="GET /readyz"} 12
tracker_request_seconds_bucket{route="GET /v1/stats",le="0.005"} 1
tracker_request_seconds_bucket{route="GET /v1/stats",le="0.01"} 2
tracker_request_seconds_bucket{route="GET /v1/stats",le="0.025"} 2
tracker_request_seconds_bucket{route="GET /v1/stats",le="0.05"} 2
tracker_request_seconds_bucket{route="GET /v1/stats",le="0.1"} 2
tracker_request_seconds_bucket{route="GET /v1/stats",le="0.25"} 4
tracker_request_seconds_bucket{route="GET /v1/stats",le="0.5"} 4
tracker_request_seconds_bucket{route="GET /v1/stats",le="1"} 4
tracker_request_seconds_bucket{route="GET /v1/stats",le="2.5"} 4
tracker_request_seconds_bucket{route="GET /v1/stats",le="5"} 4
tracker_request_seconds_bucket{route="GET /v1/stats",le="+Inf"} 4
tracker_request_seconds_sum{route="GET /v1/stats"} 0.4
tracker_request_seconds_count{route="GET /v1/stats"} 4
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="0.005"} 10
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="0.01"} 20
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="0.025"} 28
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="0.05"} 30
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="0.1"} 30
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="0.25"} 30
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="0.5"} 30
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="1"} 30
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="2.5"} 30
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="5"} 30
tracker_request_seconds_bucket{route="POST /v1/issues:list",le="+Inf"} 30
tracker_request_seconds_sum{route="POST /v1/issues:list"} 0.6
tracker_request_seconds_count{route="POST /v1/issues:list"} 30
# HELP tracker_requests_by_actor_total Requests by the actor the bearer token names.
# TYPE tracker_requests_by_actor_total counter
tracker_requests_by_actor_total{actor="angharad"} 46
# HELP tracker_requests_by_tag_total Requests by the X-Tracker-Tag header value; tags past 200 are counted under "other".
# TYPE tracker_requests_by_tag_total counter
`

// trackerFake is how the fake trackerd answers. The zero value is a healthy
// server with no /metrics route.
type trackerFake struct {
	// metrics gives the server a /metrics route; without it the mux answers
	// 404 as a server without the route does.
	metrics bool
	// metricsPage replaces the default exposition.
	metricsPage string
	// ready is the /readyz status; empty means "ready".
	ready string
	// notReady makes /readyz answer 503 with this reason, as the tracker
	// answers when its database does not answer or does not take writes.
	notReady string
	// noStatus makes /readyz answer 200 with no status at all, as a proxy in
	// front of the tracker might.
	noStatus bool
	// level is the /readyz schema level; 0 means 4.
	level int
	// failStats names a rig whose stats call answers 500, as a broken store
	// would.
	failStats string
}

// fakeTrackerd is the set of routes the screen reads, answered as trackerd
// answers them. Two tracker rigs, keyed by beads prefix: gt holds 120 issues
// and 45 wisps, ag holds 8 and 3. With withMetrics false the server has no
// /metrics route, so it answers 404 as the mux does.
func fakeTrackerd(t *testing.T, withMetrics bool) http.Handler {
	t.Helper()
	return fakeTrackerdWith(t, trackerFake{metrics: withMetrics})
}

func fakeTrackerdWith(t *testing.T, f trackerFake) http.Handler {
	t.Helper()
	stats := map[string]api.Stats{
		"gt": {Total: 120, Wisps: 45, ByStatus: map[string]int{"open": 18, "in_progress": 2, "closed": 100}},
		"ag": {Total: 8, Wisps: 3, ByStatus: map[string]int{"open": 8}},
	}
	ready, level := f.ready, f.level
	if ready == "" {
		ready = "ready"
	}
	if level == 0 {
		level = 4
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		// The tracker's own not-ready answer: 503 with the reason in the API's
		// error body.
		if f.notReady != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			writeTestJSON(w, api.Unavailable("%s", f.notReady))
			return
		}
		if f.noStatus {
			writeTestJSON(w, map[string]any{"schema_level": level})
			return
		}
		writeTestJSON(w, map[string]any{"status": ready, "schema_level": level})
	})
	mux.HandleFunc("GET /v1/stats", func(w http.ResponseWriter, r *http.Request) {
		rig := r.URL.Query().Get("rig")
		if rig == f.failStats {
			http.Error(w, "the store is unavailable", http.StatusInternalServerError)
			return
		}
		writeTestJSON(w, stats[rig])
	})
	if f.metrics {
		page := f.metricsPage
		if page == "" {
			page = trackerStatusMetrics
		}
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = w.Write([]byte(page))
		})
	}
	return mux
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// statusRoundTrip adapts a function to the transport interface.
type statusRoundTrip func(*http.Request) (*http.Response, error)

func (f statusRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// inProcess is a transport that answers each request from h through a
// recorder, so the production client runs against the fake in this process.
func inProcess(h http.Handler) http.RoundTripper {
	return statusRoundTrip(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
}

// refusing is a transport on which the tracker cannot be reached.
func refusing() http.RoundTripper {
	return statusRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
}

// statusTestEnv is a town with two tracker rigs and one Dolt rig, reading the
// tracker at trackerStatusURL through rt, with a test token. The environment
// is the map alone.
func statusTestEnv(rt http.RoundTripper) statusEnv {
	routes := []beads.Route{
		{Prefix: "gt-", Path: "gastown", Backend: beads.BackendTracker},
		{Prefix: "ag-", Path: "agentlab", Backend: beads.BackendTracker},
		{Prefix: "hq-", Path: "."},
	}
	env := map[string]string{"TRACKER_URL": trackerStatusURL, "TRACKER_TOKEN": "trk_test-token"}
	return statusEnv{
		findTown:   func() (string, error) { return "/town", nil },
		loadRoutes: func(string) ([]beads.Route, error) { return routes, nil },
		getenv:     func(key string) string { return env[key] },
		newSource: func(cfg client.Config) (trackerStatusSource, error) {
			cfg.HTTP = &http.Client{Transport: rt}
			return newTrackerStatusSource(cfg)
		},
	}
}

// The healthy golden shows the text a reader sees for a server with two rigs.
func TestTrackerStatusTextGoldenHealthyServer(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runTrackerStatus(t.Context(), statusTestEnv(inProcess(fakeTrackerd(t, true))), &out, false); err != nil {
		t.Fatalf("runTrackerStatus = %v, want the screen", err)
	}
	if err := compareGolden(trackerStatusGolden, out.String()); err != nil {
		t.Error(err)
	}
}

// --json carries the same fields as the text, and the same numbers.
func TestTrackerStatusJSONCarriesTheSameFields(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runTrackerStatus(t.Context(), statusTestEnv(inProcess(fakeTrackerd(t, true))), &out, true); err != nil {
		t.Fatalf("runTrackerStatus --json = %v", err)
	}
	var got trackerStatus
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("--json output is not the status: %v\n%s", err, out.String())
	}
	if got.URL != trackerStatusURL || !got.Ready || got.SchemaLevel == nil || *got.SchemaLevel != 4 {
		t.Errorf("readiness = %q ready %t level %v, want the server's ready at level 4", got.URL, got.Ready, got.SchemaLevel)
	}
	if len(got.Rigs) != 2 || got.Rigs[0].Rig != "gt" || got.Rigs[1].Rig != "ag" {
		t.Fatalf("rigs = %+v, want gt then ag", got.Rigs)
	}
	if got.Rigs[0].Issues == nil || *got.Rigs[0].Issues != 120 || got.Rigs[0].Wisps == nil || *got.Rigs[0].Wisps != 45 {
		t.Errorf("gt counts = %+v, want 120 issues and 45 wisps", got.Rigs[0])
	}
	if got.Requests == nil || got.Requests.Total != 46 {
		t.Fatalf("requests = %+v, want 46 served", got.Requests)
	}
	wantSlowest := &trackerSlowest{Route: "GET /v1/stats", Seconds: seconds(0.25)}
	if !sameSlowest(got.Requests.Slowest, wantSlowest) {
		t.Errorf("slowest = %s, want %s", slowestText(got.Requests.Slowest), slowestText(wantSlowest))
	}

	// "The same fields" holds when the text rendered from the decoded JSON is
	// the text the same server prints: a field the JSON dropped would be
	// missing from that screen.
	var text bytes.Buffer
	writeTrackerStatusText(&text, got)
	want, err := os.ReadFile(trackerStatusGolden)
	if err != nil {
		t.Fatalf("reading %s: %v", trackerStatusGolden, err)
	}
	if text.String() != string(want) {
		t.Errorf("text rendered from --json differs from the text the screen prints:\n%s\nwant:\n%s", text.String(), want)
	}
}

// seconds is a duration bound as the screen carries it.
func seconds(f float64) *float64 { return &f }

// sameSlowest compares two slowest-route answers by value.
func sameSlowest(a, b *trackerSlowest) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Route != b.Route || a.Over != b.Over || (a.Seconds == nil) != (b.Seconds == nil) {
		return false
	}
	return a.Seconds == nil || *a.Seconds == *b.Seconds
}

// slowestText renders an answer for a failure message.
func slowestText(s *trackerSlowest) string {
	if s == nil {
		return "none"
	}
	if s.Seconds == nil {
		return fmt.Sprintf("%s over=%t seconds=<unbounded>", s.Route, s.Over)
	}
	return fmt.Sprintf("%s over=%t seconds=%v", s.Route, s.Over, *s.Seconds)
}

// A tracker that cannot be reached is a failed run with one line of reason and
// nothing on the screen, in text and in JSON.
func TestTrackerStatusUnreachableExitsWithOneLine(t *testing.T) {
	t.Parallel()
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		err := runTrackerStatus(t.Context(), statusTestEnv(refusing()), &out, asJSON)
		if err == nil {
			t.Fatalf("runTrackerStatus(json=%t) to an unreachable tracker = nil, want a failed run", asJSON)
		}
		if !strings.Contains(err.Error(), "does not answer") {
			t.Errorf("error = %q, want the reason that the tracker does not answer", err)
		}
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("error spans lines: %q", err)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q for an unreachable tracker, want nothing", out.String())
		}
	}
}

// statusCtxSource is a tracker source that records the context each read was
// handed and fails with that context's error, so a test can show which context
// the reads run under.
type statusCtxSource struct {
	seen []context.Context
}

func (s *statusCtxSource) Readyz(ctx context.Context) (client.ReadyzStatus, error) {
	s.seen = append(s.seen, ctx)
	return client.ReadyzStatus{}, ctx.Err()
}

func (s *statusCtxSource) Stats(ctx context.Context, _ string) (api.Stats, error) {
	s.seen = append(s.seen, ctx)
	return api.Stats{}, ctx.Err()
}

func (s *statusCtxSource) Metrics(ctx context.Context) (string, error) {
	s.seen = append(s.seen, ctx)
	return "", ctx.Err()
}

// The tracker reads run under the caller's context, so an interrupted command
// stops waiting on the tracker instead of running on under a context of its
// own.
func TestTrackerStatusReadsRunUnderTheCallersContext(t *testing.T) {
	t.Parallel()
	src := &statusCtxSource{}
	env := statusTestEnv(refusing())
	env.newSource = func(client.Config) (trackerStatusSource, error) { return src, nil }

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	err := runTrackerStatus(ctx, env, &out, false)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("runTrackerStatus under a cancelled context = %v, want the cancellation", err)
	}
	if len(src.seen) == 0 {
		t.Fatal("the screen read nothing, want a readiness read")
	}
	if err := src.seen[0].Err(); err != context.Canceled {
		t.Errorf("the readiness read ran under a context with error %v, want the caller's cancellation", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q under a cancelled context, want nothing", out.String())
	}
}

// A tracker that answers not ready has answered enough to print its screen, and
// the run still fails: the exit code follows readiness. Every answer that is
// not ready is driven: the server's own 503 with the reason (the shape /readyz
// really sends when the database is down), a 200 whose status is not ready, and
// a 200 that carries no status at all.
func TestTrackerStatusNotReadyPrintsTheScreenAndExitsOne(t *testing.T) {
	t.Parallel()
	shapes := []struct {
		name string
		fake trackerFake
		// reason is the tracker's own text, as the JSON carries it.
		reason string
		// failure is the reason the failed run names; readyLine is the Ready
		// line of the screen.
		failure   string
		readyLine string
	}{
		{
			name:      "a 503 with the server's reason",
			fake:      trackerFake{metrics: true, notReady: "database does not answer"},
			reason:    "database does not answer",
			failure:   "database does not answer",
			readyLine: "Ready      no (database does not answer)\n",
		},
		{
			name:      "a 200 whose status is not ready",
			fake:      trackerFake{metrics: true, ready: "degraded"},
			reason:    "degraded",
			failure:   "degraded",
			readyLine: "Ready      no (degraded)\n",
		},
		{
			name:      "a 200 with no status at all",
			fake:      trackerFake{metrics: true, noStatus: true},
			failure:   "it did not say why",
			readyLine: "Ready      no\n",
		},
	}
	for _, shape := range shapes {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", shape.name, asJSON), func(t *testing.T) {
				t.Parallel()
				var out bytes.Buffer
				err := runTrackerStatus(t.Context(), statusTestEnv(inProcess(fakeTrackerdWith(t, shape.fake))), &out, asJSON)
				if err == nil {
					t.Fatal("runTrackerStatus for a not-ready tracker = nil, want a failed run")
				}
				if code := exitCodeForError(err); code != 1 {
					t.Errorf("exit code = %d, want 1 for a tracker that does not answer ready", code)
				}
				if !strings.Contains(err.Error(), "is not ready: "+shape.failure) {
					t.Errorf("error = %q, want the tracker's reason for not being ready", err)
				}
				if strings.Contains(err.Error(), "\n") {
					t.Errorf("error spans lines: %q", err)
				}
				if out.Len() == 0 {
					t.Fatal("printed nothing, want the screen the tracker could still answer")
				}
				var screen strings.Builder
				if asJSON {
					// The JSON carries the same fact; render it as the text does.
					var got trackerStatus
					if err := json.Unmarshal(out.Bytes(), &got); err != nil {
						t.Fatalf("--json output is not the status: %v\n%s", err, out.String())
					}
					if got.Ready || got.ReadyReason != shape.reason {
						t.Errorf("--json readiness = ready %t reason %q, want not ready with the tracker's reason", got.Ready, got.ReadyReason)
					}
					if got.SchemaLevel != nil {
						t.Errorf("--json carries schema level %d for a tracker that never said ready, want none", *got.SchemaLevel)
					}
					writeTrackerStatusText(&screen, got)
				} else {
					screen.Write(out.Bytes())
				}
				if !strings.Contains(screen.String(), shape.readyLine) {
					t.Errorf("screen lacks %q:\n%s", shape.readyLine, screen.String())
				}
				// The counts and the metrics are still there: a tracker whose
				// database is down can still say what its other routes give.
				if !strings.Contains(screen.String(), "Requests   46 since start\n") {
					t.Errorf("screen lost the request lines:\n%s", screen.String())
				}
			})
		}
	}
}

// A server whose not-ready reason is long or hostile still fails in one short
// line, and the screen's Ready line is one line too: the server chose that
// text.
func TestTrackerStatusNotReadyKeepsTheReasonToOneLine(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200)
	hostile := "not\nready\x1b[31m"
	for _, shape := range []trackerFake{
		{notReady: long},
		{notReady: hostile},
		{ready: long},
		{ready: hostile},
	} {
		var out bytes.Buffer
		err := runTrackerStatus(t.Context(), statusTestEnv(inProcess(fakeTrackerdWith(t, shape))), &out, false)
		if err == nil {
			t.Fatalf("runTrackerStatus with reason %q = nil, want a failed run", shape.notReady+shape.ready)
		}
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("error spans lines: %q", err)
		}
		if len(err.Error()) > 200 {
			t.Errorf("error is %d bytes for a %q reason, want it cut to a line", len(err.Error()), shape.notReady+shape.ready)
		}
		if strings.Contains(out.String(), "\x1b") {
			t.Errorf("screen carries a raw escape from the reason:\n%q", out.String())
		}
		// Tracker, Ready, Rigs and its two rig lines, Requests, Slowest.
		if got, want := strings.Count(out.String(), "\n"), 7; got != want {
			t.Errorf("screen has %d lines, want %d (one fact each):\n%q", got, want, out.String())
		}
	}
}

// One rig's stats failing marks both its counts unavailable and still prints
// the other rig's counts.
func TestTrackerStatusRigCountsDegradeOneRigAtATime(t *testing.T) {
	t.Parallel()
	env := statusTestEnv(inProcess(fakeTrackerdWith(t, trackerFake{metrics: true, failStats: "gt"})))
	var out bytes.Buffer
	if err := runTrackerStatus(t.Context(), env, &out, false); err != nil {
		t.Fatalf("runTrackerStatus with one failed stats call = %v, want the screen", err)
	}
	text := out.String()
	for _, want := range []string{
		"issues unavailable: tracker answered 500", // gt's stats call failed, with its reason
		"wisps unavailable: tracker answered 500",  // and its wisp count came from the same call
		"issues 8 (open 8)",                        // ag's stats call still answered
		"wisps 3",                                  // so ag's wisp count did too
	} {
		if !strings.Contains(text, want) {
			t.Errorf("screen lacks %q:\n%s", want, text)
		}
	}
}

// Every string the tracker chose is escaped on the way to the terminal, so a
// hostile route, rig or status name cannot recolour the screen or forge a line.
func TestTrackerStatusEscapesTrackerChosenStrings(t *testing.T) {
	t.Parallel()
	n := 1
	// Ready without a schema level: a status value the screen can be handed
	// without one, which it prints as ready rather than reading the level of a
	// nil pointer.
	rep := trackerStatus{
		URL:   "https://tracker.test:7433/\x1b[2J",
		Ready: true,
		Rigs: []trackerRigStatus{{
			Rig:      "gt\x1b[31m",
			Issues:   &n,
			ByStatus: map[string]int{"op\nen": 1},
			Wisps:    &n,
		}},
		Requests: &trackerRequests{
			Total:   3,
			Slowest: &trackerSlowest{Route: "GET /x\nSlowest    forged", Seconds: seconds(0.5)},
		},
	}
	var out bytes.Buffer
	writeTrackerStatusText(&out, rep)
	text := out.String()

	if strings.Contains(text, "\x1b") {
		t.Errorf("screen carries a raw escape:\n%q", text)
	}
	if got, want := strings.Count(text, "\n"), 6; got != want {
		t.Errorf("screen has %d lines, want %d (one fact each):\n%q", got, want, text)
	}
	for _, want := range []string{
		`\x1b[2J`,
		`gt\x1b[31m`,
		`op\nen`,
		`GET /x\nSlowest    forged`,
		"Ready      yes\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("screen lacks the escaped form %q:\n%s", want, text)
		}
	}

	// The same holds on the run path, where the rig name comes from the route
	// table rather than from a value the test built.
	env := statusTestEnv(inProcess(fakeTrackerdWith(t, trackerFake{metrics: true})))
	env.loadRoutes = func(string) ([]beads.Route, error) {
		return []beads.Route{{Prefix: "gt-\x1b[31m", Path: "gastown", Backend: beads.BackendTracker}}, nil
	}
	var screen bytes.Buffer
	if err := runTrackerStatus(t.Context(), env, &screen, false); err != nil {
		t.Fatalf("runTrackerStatus with a hostile rig name = %v, want the screen", err)
	}
	if strings.Contains(screen.String(), "\x1b") {
		t.Errorf("screen carries a raw escape from the route table:\n%q", screen.String())
	}
	if !strings.Contains(screen.String(), `gt-\x1b[31m`) {
		t.Errorf("screen lacks the escaped rig name:\n%s", screen.String())
	}
}

// A rig whose name holds multi-byte runes keeps the columns where they belong:
// the width is counted in runes, as fmt pads by, not in bytes.
func TestTrackerStatusRigColumnsCountRunesNotBytes(t *testing.T) {
	t.Parallel()
	n := 1
	rep := trackerStatus{
		Rigs: []trackerRigStatus{
			{Rig: "éé", Issues: &n, Wisps: &n},  // 2 runes, 4 bytes
			{Rig: "abc", Issues: &n, Wisps: &n}, // 3 runes, 3 bytes
		},
	}
	var out bytes.Buffer
	writeTrackerStatusText(&out, rep)
	for _, want := range []string{
		"  éé   issues 1  wisps 1\n", // padded to the 3-rune column, plus the 2-space gap
		"  abc  issues 1  wisps 1\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("screen lacks %q:\n%q", want, out.String())
		}
	}
}

// A server whose latency histogram has only an +Inf bucket has no bound to
// report, so the line says unknown rather than a time of zero, and the run
// still succeeds.
func TestTrackerStatusWithoutFiniteBucketsShowsUnknown(t *testing.T) {
	t.Parallel()
	page := "# TYPE tracker_requests_total counter\n" +
		"tracker_requests_total{route=\"GET /x\",status=\"200\"} 2\n" +
		"tracker_request_seconds_bucket{route=\"GET /x\",le=\"+Inf\"} 2\n"
	env := statusTestEnv(inProcess(fakeTrackerdWith(t, trackerFake{metrics: true, metricsPage: page})))
	var out bytes.Buffer
	if err := runTrackerStatus(t.Context(), env, &out, false); err != nil {
		t.Fatalf("runTrackerStatus with an unbounded histogram = %v, want the screen", err)
	}
	for _, want := range []string{
		"Slowest    unknown on GET /x (no finite bucket)\n",
		"Requests   2 since start\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("screen lacks %q:\n%s", want, out.String())
		}
	}
}

// A server without /metrics is still a screen: the request lines say they are
// unavailable, the rest prints, and the run succeeds.
func TestTrackerStatusWithoutMetricsRouteShowsUnavailable(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runTrackerStatus(t.Context(), statusTestEnv(inProcess(fakeTrackerd(t, false))), &out, false); err != nil {
		t.Fatalf("runTrackerStatus without /metrics = %v, want the screen", err)
	}
	text := out.String()
	for _, want := range []string{
		"Requests   unavailable: tracker answered 404\n",
		"Slowest    unavailable\n",
		"  gt  ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("screen lacks %q:\n%s", want, text)
		}
	}
}

// Without TRACKER_URL there is no endpoint to read, so the run refuses before
// it sends anything.
func TestTrackerStatusWithoutURLRefuses(t *testing.T) {
	t.Parallel()
	env := statusTestEnv(refusing())
	env.getenv = func(string) string { return "" }
	var out bytes.Buffer
	err := runTrackerStatus(t.Context(), env, &out, false)
	if err == nil || !strings.Contains(err.Error(), "TRACKER_URL is not set") {
		t.Fatalf("runTrackerStatus without TRACKER_URL = %v, want the unset-URL refusal", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q, want nothing", out.String())
	}
}

// A town the screen cannot find still shows the tracker, and says the rigs are
// unavailable rather than failing.
func TestTrackerStatusWithoutTownKeepsTheTrackerLines(t *testing.T) {
	t.Parallel()
	env := statusTestEnv(inProcess(fakeTrackerd(t, true)))
	env.findTown = func() (string, error) { return "", errors.New("no town here") }
	var out bytes.Buffer
	if err := runTrackerStatus(t.Context(), env, &out, false); err != nil {
		t.Fatalf("runTrackerStatus outside a town = %v, want the screen", err)
	}
	if !strings.Contains(out.String(), "Rigs       unavailable: not in a Gas Town workspace: no town here\n") {
		t.Errorf("screen = %q, want the rigs unavailable with the town's reason", out.String())
	}
	if !strings.Contains(out.String(), "Requests   46 since start\n") {
		t.Errorf("screen lost the request lines: %q", out.String())
	}
}

// The production client refuses a configuration it cannot send with, and
// builds one it can, so a missing token surfaces before any request.
func TestNewTrackerStatusSourceNeedsAToken(t *testing.T) {
	t.Parallel()
	if _, err := newTrackerStatusSource(client.Config{BaseURL: trackerStatusURL}); err == nil {
		t.Error("newTrackerStatusSource without a token = nil, want the client's refusal")
	}
	if _, err := newTrackerStatusSource(client.Config{BaseURL: trackerStatusURL, Token: "trk_test-token"}); err != nil {
		t.Errorf("newTrackerStatusSource with a token = %v, want a source", err)
	}
}

// A client that cannot be built is a failed run that names the endpoint, with
// nothing on the screen.
func TestTrackerStatusRefusesAClientThatCannotBeBuilt(t *testing.T) {
	t.Parallel()
	env := statusTestEnv(inProcess(fakeTrackerd(t, true)))
	env.newSource = func(client.Config) (trackerStatusSource, error) {
		return nil, errors.New("client: no token: set Token or TokenFile")
	}
	var out bytes.Buffer
	err := runTrackerStatus(t.Context(), env, &out, false)
	if err == nil || !strings.Contains(err.Error(), "cannot be reached") || !strings.Contains(err.Error(), trackerStatusURL) {
		t.Fatalf("runTrackerStatus with no client = %v, want a refusal naming the endpoint", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q, want nothing", out.String())
	}
}

// A route table that cannot be read leaves the rig lines unavailable with the
// reason, and the tracker lines still print.
func TestTrackerStatusKeepsTheTrackerLinesWhenTheRouteTableIsUnreadable(t *testing.T) {
	t.Parallel()
	env := statusTestEnv(inProcess(fakeTrackerd(t, true)))
	env.loadRoutes = func(string) ([]beads.Route, error) { return nil, errors.New("routes.jsonl: permission denied") }
	var out bytes.Buffer
	if err := runTrackerStatus(t.Context(), env, &out, false); err != nil {
		t.Fatalf("runTrackerStatus with an unreadable route table = %v, want the screen", err)
	}
	if !strings.Contains(out.String(), "Rigs       unavailable: routes.jsonl: permission denied\n") {
		t.Errorf("screen = %q, want the rigs unavailable with the read error", out.String())
	}
	if !strings.Contains(out.String(), "Ready      yes (schema level 4)\n") {
		t.Errorf("screen lost the readiness line: %q", out.String())
	}
}

// trackerBrokenPipe is a screen that cannot be written to, as a closed pipe is.
type trackerBrokenPipe struct{}

func (trackerBrokenPipe) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A JSON screen that cannot be written is a failed run, not a silent one.
func TestTrackerStatusJSONRefusesAWriterThatFails(t *testing.T) {
	t.Parallel()
	err := runTrackerStatus(t.Context(), statusTestEnv(inProcess(fakeTrackerd(t, true))), trackerBrokenPipe{}, true)
	if err == nil || !strings.Contains(err.Error(), "write: broken pipe") {
		t.Fatalf("runTrackerStatus --json to a failing writer = %v, want the write error", err)
	}
}

// The parser reads the totals and the slowest route, and refuses a page that
// is not the tracker's exposition rather than reading it as zero requests.
func TestParseTrackerMetrics(t *testing.T) {
	t.Parallel()
	const head = "# TYPE tracker_requests_total counter\n"
	tests := []struct {
		name    string
		page    string
		want    trackerRequests
		wantErr string
	}{
		{
			name: "the sample page",
			page: trackerStatusMetrics,
			want: trackerRequests{Total: 46, Slowest: &trackerSlowest{Route: "GET /v1/stats", Seconds: seconds(0.25)}},
		},
		{
			name: "no requests yet",
			page: head,
			want: trackerRequests{Total: 0, Slowest: nil},
		},
		{
			name: "a route past the largest bucket reads as over it",
			page: head + `tracker_requests_total{route="GET /slow",status="200"} 1
tracker_request_seconds_bucket{route="GET /slow",le="0.5"} 0
tracker_request_seconds_bucket{route="GET /slow",le="5"} 0
tracker_request_seconds_bucket{route="GET /slow",le="+Inf"} 1
`,
			want: trackerRequests{Total: 1, Slowest: &trackerSlowest{Route: "GET /slow", Seconds: seconds(5), Over: true}},
		},
		{
			name: "a histogram with only an +Inf bucket has no bound",
			page: head + `tracker_requests_total{route="GET /x",status="200"} 1
tracker_request_seconds_bucket{route="GET /x",le="+Inf"} 1
`,
			want: trackerRequests{Total: 1, Slowest: &trackerSlowest{Route: "GET /x"}},
		},
		{
			name: "an unbounded route outranks a bounded one",
			page: head + `tracker_requests_total{route="GET /bounded",status="200"} 1
tracker_requests_total{route="GET /unbounded",status="200"} 1
tracker_request_seconds_bucket{route="GET /bounded",le="0.5"} 1
tracker_request_seconds_bucket{route="GET /bounded",le="+Inf"} 1
tracker_request_seconds_bucket{route="GET /unbounded",le="+Inf"} 1
`,
			want: trackerRequests{Total: 2, Slowest: &trackerSlowest{Route: "GET /unbounded"}},
		},
		{
			name: "a tie in the bound goes to the route name that sorts first",
			page: head + `tracker_requests_total{route="GET /b",status="200"} 1
tracker_requests_total{route="GET /a",status="200"} 1
tracker_request_seconds_bucket{route="GET /b",le="0.1"} 1
tracker_request_seconds_bucket{route="GET /b",le="+Inf"} 1
tracker_request_seconds_bucket{route="GET /a",le="0.1"} 1
tracker_request_seconds_bucket{route="GET /a",le="+Inf"} 1
`,
			want: trackerRequests{Total: 2, Slowest: &trackerSlowest{Route: "GET /a", Seconds: seconds(0.1)}},
		},
		{
			name:    "a page without the request family is refused",
			page:    "<html>404 page not found</html>",
			wantErr: "no tracker_requests_total family",
		},
		{
			name:    "an unreadable request line is refused",
			page:    head + "tracker_requests_total{route=\"GET /x\"} many\n",
			wantErr: "unreadable tracker_requests_total line",
		},
		{
			name: "a histogram without +Inf is refused",
			page: head + `tracker_requests_total{route="GET /x",status="200"} 1
tracker_request_seconds_bucket{route="GET /x",le="0.1"} 1
`,
			wantErr: "no +Inf bucket",
		},
		{
			name:    "a request count past uint64 is refused",
			page:    head + "tracker_requests_total{route=\"GET /x\",status=\"200\"} 99999999999999999999999\n",
			wantErr: "unreadable request count",
		},
		{
			name:    "a bucket line that is not a bucket count is refused",
			page:    head + "tracker_request_seconds_bucket{route=\"GET /x\",le=\"0.1\"} many\n",
			wantErr: "unreadable tracker_request_seconds_bucket line",
		},
		{
			name: "a route label that does not unquote is refused",
			page: head + `tracker_request_seconds_bucket{route="GET \q",le="+Inf"} 1
`,
			wantErr: "unreadable route label",
		},
		{
			name: "a bucket count past uint64 is refused",
			page: head + `tracker_request_seconds_bucket{route="GET /x",le="+Inf"} 99999999999999999999999
`,
			wantErr: "unreadable bucket count",
		},
		{
			name: "a bucket bound that is not a number is refused",
			page: head + `tracker_request_seconds_bucket{route="GET /x",le="soon"} 1
tracker_request_seconds_bucket{route="GET /x",le="+Inf"} 1
`,
			wantErr: "unreadable bucket bound",
		},
		{
			name: "a bucket bound that is NaN is refused",
			page: head + `tracker_request_seconds_bucket{route="GET /x",le="NaN"} 1
tracker_request_seconds_bucket{route="GET /x",le="+Inf"} 1
`,
			wantErr: "non-finite bucket bound",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTrackerMetrics(tt.page)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseTrackerMetrics error = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTrackerMetrics = %v", err)
			}
			if got.Total != tt.want.Total {
				t.Errorf("total = %d, want %d", got.Total, tt.want.Total)
			}
			if !sameSlowest(got.Slowest, tt.want.Slowest) {
				t.Errorf("slowest = %s, want %s", slowestText(got.Slowest), slowestText(tt.want.Slowest))
			}
		})
	}
}
