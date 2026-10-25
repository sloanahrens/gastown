package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// stepClock advances a fixed step on every reading. The instrumentation reads
// it twice per request, so a request's elapsed time is its step: elapsed
// times are exact and no test sleeps.
type stepClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.t
	c.t = c.t.Add(c.step)
	return now
}

func (c *stepClock) setStep(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step = step
}

// metricsServer is stubServer with the metrics clock the test injects and an
// optional log. The clock is set before the listener opens, so no request
// observes the write.
func metricsServer(t *testing.T, st store.Store, clock *stepClock, log Logger) *httptest.Server {
	t.Helper()
	cfg := Config{
		Auth:        fakeAuth{"good-token": "town/crew/dipper", "other-token": "gastown/polecats/furiosa"},
		Health:      fakeHealth{},
		SchemaLevel: 3,
		Store:       st,
		Log:         log,
	}
	s := New(cfg)
	if clock != nil {
		s.metrics.now = clock.Now
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// request sends one request with the headers the caller names; an empty
// token or schema omits that header.
func request(t *testing.T, method, url, body, token, schema string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if schema != "" {
		req.Header.Set(api.SchemaHeader, schema)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// scrape reads /metrics without a token and parses it into sample name ->
// value. A duplicate sample is a test failure: a scrape with two values for
// one time series is invalid to every reader.
func scrape(t *testing.T, ts *httptest.Server) map[string]float64 {
	t.Helper()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d %q, want 200", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	return parseSamples(t, string(body))
}

func parseSamples(t *testing.T, body string) map[string]float64 {
	t.Helper()
	samples := map[string]float64{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// The value follows the last space: a route label may hold spaces.
		cut := strings.LastIndexByte(line, ' ')
		if cut < 0 {
			t.Fatalf("malformed sample line %q", line)
		}
		name, value := line[:cut], line[cut+1:]
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("sample %q has a non-numeric value: %v", line, err)
		}
		if _, dup := samples[name]; dup {
			t.Errorf("duplicate sample %q", name)
		}
		samples[name] = v
	}
	return samples
}

// issueAndListStub answers the two routes the count test drives.
func issueAndListStub() issueStub {
	return issueStub{
		get: func(_ context.Context, ids []string) (map[string]api.Issue, error) {
			out := map[string]api.Issue{}
			for _, id := range ids {
				out[id] = api.Issue{ID: id}
			}
			return out, nil
		},
		list: func(context.Context, api.ListFilter) (api.IssuePage, error) {
			return api.IssuePage{}, nil
		},
	}
}

// The route label is the registered pattern, so two issues on one route are
// one sample and an unmatched path is bounded too. Unit tier: httptest, stub
// store, no Postgres.
func TestMetricsCountByRegisteredRouteAndStatus(t *testing.T) {
	t.Parallel()
	ts := metricsServer(t, issueAndListStub(), nil, nil)
	auth := map[string]string{}
	if code, body := request(t, http.MethodGet, ts.URL+"/v1/issues/gt-abc", "", "good-token", "3", auth); code != 200 {
		t.Fatalf("GET one issue = %d %q, want 200", code, body)
	}
	if code, body := request(t, http.MethodGet, ts.URL+"/v1/issues/gt-def", "", "good-token", "3", auth); code != 200 {
		t.Fatalf("GET another issue = %d %q, want 200", code, body)
	}
	if code, body := request(t, http.MethodPost, ts.URL+"/v1/issues:list", "{}", "good-token", "3", auth); code != 200 {
		t.Fatalf("POST list = %d %q, want 200", code, body)
	}
	if code, body := request(t, http.MethodGet, ts.URL+"/v1/nowhere", "", "good-token", "3", auth); code != 404 {
		t.Fatalf("GET an unmatched path = %d %q, want 404", code, body)
	}

	samples := scrape(t, ts)
	for name, want := range map[string]float64{
		`tracker_requests_total{route="GET /v1/issues/{id}",status="200"}`:  2,
		`tracker_requests_total{route="POST /v1/issues:list",status="200"}`: 1,
		`tracker_requests_total{route="unmatched",status="404"}`:            1,
	} {
		if got := samples[name]; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	// The ids must not be label values: every sample in the scrape is one of
	// the named series above, and no id appears in one.
	for name := range samples {
		if strings.Contains(name, "gt-abc") || strings.Contains(name, "gt-def") {
			t.Errorf("sample %q carries a path id as a label value", name)
		}
	}
}

// Buckets are cumulative in the exposition, and +Inf and the count are the
// number of observations. Unit tier: injected clock, no sleeps.
func TestMetricsHistogramBucketsAreCumulative(t *testing.T) {
	t.Parallel()
	clock := &stepClock{t: time.Unix(0, 0), step: 15 * time.Millisecond}
	ts := metricsServer(t, issueAndListStub(), clock, nil)
	for i := 0; i < 3; i++ {
		if code, body := request(t, http.MethodGet, ts.URL+"/v1/issues/gt-abc", "", "good-token", "3", nil); code != 200 {
			t.Fatalf("GET issue %d = %d %q, want 200", i, code, body)
		}
	}

	samples := scrape(t, ts)
	route := `tracker_request_seconds_bucket{route="GET /v1/issues/{id}",le=`
	const countKey = `tracker_request_seconds_count{route="GET /v1/issues/{id}"}`
	for name, want := range map[string]float64{
		route + `"0.005"}`: 0,
		route + `"0.01"}`:  0,
		route + `"0.025"}`: 3,
		route + `"0.05"}`:  3,
		route + `"5"}`:     3,
		route + `"+Inf"}`:  3,
		countKey:           3,
	} {
		if got := samples[name]; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	// Cumulative means the later buckets never read lower than the earlier.
	for _, bound := range []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "+Inf"} {
		if samples[route+`"`+bound+`"}`] < 0 {
			t.Errorf("bucket le=%s is missing", bound)
		}
	}
	if sum := samples[`tracker_request_seconds_sum{route="GET /v1/issues/{id}"}`]; sum < 0.044 || sum > 0.046 {
		t.Errorf("sum = %v, want 3 × 15ms", sum)
	}
}

// The actor is the token's, the tag is the header's, and a request with no
// tag counts under no tag value. Unit tier: httptest, stub store.
func TestMetricsCountActorsAndTags(t *testing.T) {
	t.Parallel()
	ts := metricsServer(t, issueAndListStub(), nil, nil)
	for i := 0; i < 3; i++ {
		if code, body := request(t, http.MethodGet, ts.URL+"/v1/issues/gt-abc", "", "good-token", "3",
			map[string]string{api.RequestTagHeader: "tick-1"}); code != 200 {
			t.Fatalf("GET with tick-1 = %d %q, want 200", code, body)
		}
	}
	if code, body := request(t, http.MethodGet, ts.URL+"/v1/issues/gt-abc", "", "other-token", "3",
		map[string]string{api.RequestTagHeader: "tick-2"}); code != 200 {
		t.Fatalf("GET with tick-2 = %d %q, want 200", code, body)
	}
	if code, body := request(t, http.MethodGet, ts.URL+"/v1/issues/gt-abc", "", "good-token", "3", nil); code != 200 {
		t.Fatalf("GET with no tag = %d %q, want 200", code, body)
	}

	samples := scrape(t, ts)
	for name, want := range map[string]float64{
		`tracker_requests_by_actor_total{actor="town/crew/dipper"}`:         4,
		`tracker_requests_by_actor_total{actor="gastown/polecats/furiosa"}`: 1,
		`tracker_requests_by_tag_total{tag="tick-1"}`:                       3,
		`tracker_requests_by_tag_total{tag="tick-2"}`:                       1,
	} {
		if got := samples[name]; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	for name := range samples {
		if strings.HasPrefix(name, `tracker_requests_by_tag_total{tag=""`) {
			t.Errorf("a request with no tag must not mint an empty tag value: %q", name)
		}
	}
}

// The 201st distinct tag counts under other, and a tag already known keeps
// its own value. A unit test of the recorder: 201 HTTP requests would test
// the same arithmetic more slowly.
func TestMetricsCapDistinctTagsAt200(t *testing.T) {
	t.Parallel()
	m := newMetrics(discardLog{})
	for i := 1; i <= maxTagValues; i++ {
		m.record(`GET /v1/issues/{id}`, 200, 0, "town/crew/dipper", fmt.Sprintf("t%d", i))
	}
	for _, tag := range []string{"t201", "t1"} {
		m.record(`GET /v1/issues/{id}`, 200, 0, "town/crew/dipper", tag)
	}

	samples := parseSamples(t, m.render())
	for name, want := range map[string]float64{
		`tracker_requests_by_tag_total{tag="t1"}`:    2,
		`tracker_requests_by_tag_total{tag="t200"}`:  1,
		`tracker_requests_by_tag_total{tag="other"}`: 1,
	} {
		if got := samples[name]; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if _, ok := samples[`tracker_requests_by_tag_total{tag="t201"}`]; ok {
		t.Errorf("the 201st distinct tag became its own label value")
	}
}

// A request past the threshold is logged once, after the response, with the
// route, status, duration and actor; never the body or the token. Unit tier:
// injected clock, no sleeps.
func TestSlowRequestIsLoggedOnceWithoutBodyOrToken(t *testing.T) {
	t.Parallel()
	log := &recLog{}
	clock := &stepClock{t: time.Unix(0, 0), step: slowRequestThreshold + 50*time.Millisecond}
	ts := metricsServer(t, issueAndListStub(), clock, log)

	// An invalid body is a 400, which the instrumentation still counts and
	// times: a refusal takes just as long as a success.
	code, body := request(t, http.MethodPost, ts.URL+"/v1/issues:list", `{"secret":"body-secret"}`, "good-token", "3",
		map[string]string{api.RequestTagHeader: "tick-9"})
	if code != 400 {
		t.Fatalf("POST an invalid body = %d %q, want 400", code, body)
	}

	log.mu.Lock()
	lines := append([]string(nil), log.logs...)
	log.mu.Unlock()
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want exactly 1: %q", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{"route=POST /v1/issues:list", "status=400", "duration=150ms", "actor=town/crew/dipper"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q lacks %q", line, want)
		}
	}
	for _, secret := range []string{"good-token", "body-secret"} {
		if strings.Contains(line, secret) {
			t.Errorf("log line %q carries the %q it must never carry", line, secret)
		}
	}

	if got := scrape(t, ts)[`tracker_requests_total{route="POST /v1/issues:list",status="400"}`]; got != 1 {
		t.Errorf("the slow refused request counts %v times, want 1", got)
	}
}

// The scrape is unauthenticated, and /v1 keeps its token check. Unit tier:
// httptest.
func TestMetricsNeedsNoTokenAndDoNotCountThemselves(t *testing.T) {
	t.Parallel()
	ts := metricsServer(t, issueAndListStub(), nil, nil)
	if code, body := request(t, http.MethodGet, ts.URL+"/metrics", "", "", "", nil); code != 200 {
		t.Fatalf("GET /metrics without a token = %d %q, want 200", code, body)
	}
	if code, body := request(t, http.MethodGet, ts.URL+"/v1/whoami", "", "", "3", nil); code != 401 {
		t.Fatalf("GET /v1/whoami without a token = %d %q, want 401", code, body)
	}
	if code, body := request(t, http.MethodGet, ts.URL+"/v1/whoami", "", "good-token", "3", nil); code != 200 {
		t.Fatalf("GET /v1/whoami with a token = %d %q, want 200", code, body)
	}

	samples := scrape(t, ts)
	for name := range samples {
		if strings.HasPrefix(name, "tracker_requests_total{") && strings.Contains(name, "/metrics") {
			t.Errorf("the scrape counted itself: %q", name)
		}
	}
}

// The schema gate runs after instrumentation, so a refused request is still
// counted under the route it asked for. Unit tier: httptest, stub store.
func TestSchemaRefusalIsCountedUnderItsRoute(t *testing.T) {
	t.Parallel()
	st := issueStub{get: func(context.Context, []string) (map[string]api.Issue, error) {
		t.Error("the store must not be reached through a schema skew")
		return nil, nil
	}}
	ts := metricsServer(t, st, nil, nil)
	if code, body := request(t, http.MethodGet, ts.URL+"/v1/issues/gt-abc", "", "good-token", "2", nil); code != 409 {
		t.Fatalf("GET with a skewed schema = %d %q, want 409", code, body)
	}
	if got := scrape(t, ts)[`tracker_requests_total{route="GET /v1/issues/{id}",status="409"}`]; got != 1 {
		t.Errorf("the refused request counts %v times, want 1", got)
	}
}

// Concurrent requests are all counted: a scrape never races the recording
// (`go test -race ./server/` covers the lock). Unit tier: httptest.
func TestMetricsCountConcurrentRequests(t *testing.T) {
	t.Parallel()
	ts := metricsServer(t, issueAndListStub(), nil, nil)
	const requests = 20
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := fmt.Sprintf("%s/v1/issues/gt-%d", ts.URL, i)
			resp, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Header.Set("Authorization", "Bearer good-token")
			resp.Header.Set(api.SchemaHeader, "3")
			// Each goroutine has its own tag, which is what the cap guards.
			resp.Header.Set(api.RequestTagHeader, fmt.Sprintf("tick-%d", i))
			r, err := http.DefaultClient.Do(resp)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}(i)
	}
	wg.Wait()

	samples := scrape(t, ts)
	if got := samples[`tracker_requests_total{route="GET /v1/issues/{id}",status="200"}`]; got != requests {
		t.Errorf("requests counted = %v, want %d", got, requests)
	}
	if got := samples[`tracker_request_seconds_count{route="GET /v1/issues/{id}"}`]; got != requests {
		t.Errorf("histogram count = %v, want %d", got, requests)
	}
}
