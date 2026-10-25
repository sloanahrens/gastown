package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

// The bounds the metrics keep: one log line per slow request, and one label
// value per 200 request tags plus the other bucket.
const (
	slowRequestThreshold = 100 * time.Millisecond
	maxTagValues         = 200
	otherTagValue        = "other"
	// unmatchedRoute labels a request no registered pattern matched (a 404 or
	// a 405); a label needs a bounded value, so the raw path never becomes one.
	unmatchedRoute = "unmatched"
)

// requestSecondsBuckets are the latency histogram's upper bounds in seconds.
// The +Inf bucket is implicit: it is the observation count.
var requestSecondsBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// metrics is the server's request instrumentation: counts by registered route
// and status, a latency histogram per route, per-actor and per-tag counters,
// and the slow-request log. Every label value is bounded: routes are the mux's
// registered patterns (an id in a path would mint a label per issue), actors
// come from tokens and tags stop at maxTagValues.
type metrics struct {
	log Logger
	// now is the clock the latency and the slow-request threshold read; the
	// constructor sets it and tests replace it to make elapsed time exact.
	now func() time.Time

	mu       sync.Mutex
	requests map[routeStatus]uint64
	seconds  map[string]*requestHistogram
	actors   map[string]uint64
	tags     map[string]uint64
}

// routeStatus is one tracker_requests_total sample.
type routeStatus struct{ route, status string }

// requestHistogram is one route's latency histogram: non-cumulative counts per
// requestSecondsBuckets entry, the sum of the observations and their count.
type requestHistogram struct {
	buckets []uint64
	sum     float64
	count   uint64
}

func newMetrics(log Logger) *metrics {
	return &metrics{
		log:      log,
		now:      time.Now,
		requests: map[routeStatus]uint64{},
		seconds:  map[string]*requestHistogram{},
		actors:   map[string]uint64{},
		tags:     map[string]uint64{},
	}
}

// instrument records every request it serves: its count by registered route
// and status, its latency for the route's histogram, its actor and its request
// tag. routes is the /v1 mux, matched without serving the request, so the
// route is known even when the schema gate refuses it. A request slower than
// slowRequestThreshold is logged once, after the response, with the route,
// status, duration and actor -- never the raw path, the body or the token.
func (m *metrics) instrument(routes *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := m.now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		elapsed := m.now().Sub(start)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		m.record(routeOf(routes, r), status, elapsed, api.ActorFrom(r.Context()), r.Header.Get(api.RequestTagHeader))
	})
}

// routeOf is the registered pattern a request matches, method included (for
// example "GET /v1/issues/{id}"), or unmatchedRoute.
func routeOf(mux *http.ServeMux, r *http.Request) string {
	if _, pattern := mux.Handler(r); pattern != "" {
		return pattern
	}
	return unmatchedRoute
}

// record adds one served request to every counter it belongs to and logs it if
// it was slow. Logging happens outside the lock: it writes somewhere the
// server does not control, and a blocked writer must not stall a scrape.
func (m *metrics) record(route string, status int, elapsed time.Duration, actor, tag string) {
	slow := elapsed > slowRequestThreshold
	m.mu.Lock()
	m.requests[routeStatus{route, strconv.Itoa(status)}]++
	m.observeSecondsLocked(route, elapsed.Seconds())
	if actor != "" {
		m.actors[actor]++
	}
	if tag = strings.TrimSpace(tag); tag != "" {
		m.tags[m.tagValueLocked(tag)]++
	}
	m.mu.Unlock()
	if slow {
		m.log.Printf("slow request: route=%s status=%d duration=%s actor=%s", route, status, elapsed, actor)
	}
}

// observeSecondsLocked files one observation under the first bucket that
// bounds it; buckets past that stay untouched, so rendering reads them
// cumulatively.
func (m *metrics) observeSecondsLocked(route string, seconds float64) {
	h, ok := m.seconds[route]
	if !ok {
		h = &requestHistogram{buckets: make([]uint64, len(requestSecondsBuckets))}
		m.seconds[route] = h
	}
	for i, bound := range requestSecondsBuckets {
		if seconds <= bound {
			h.buckets[i]++
			break
		}
	}
	h.sum += seconds
	h.count++
}

// tagValueLocked is the label value a tag counts under: itself until
// maxTagValues distinct tags are in use, then other, so a caller that mints a
// tag per request cannot grow the scrape without bound.
func (m *metrics) tagValueLocked(tag string) string {
	if _, known := m.tags[tag]; known {
		return tag
	}
	if tag != otherTagValue && len(m.tags) >= maxTagValues {
		return otherTagValue
	}
	return tag
}

// serve is GET /metrics: unauthenticated like the probes, because the
// listener is loopback-only, and Prometheus text so a scraper reads it.
func (m *metrics) serve(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(m.render()))
}

// render is the current state as Prometheus text, sorted so two scrapes of the
// same state read the same.
func (m *metrics) render() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	b.WriteString("# HELP tracker_requests_total Authenticated requests by registered route and status.\n")
	b.WriteString("# TYPE tracker_requests_total counter\n")
	for _, k := range sortedRoutes(m.requests) {
		fmt.Fprintf(&b, "tracker_requests_total{route=%s,status=%s} %d\n", quote(k.route), quote(k.status), m.requests[k])
	}

	b.WriteString("# HELP tracker_request_seconds Request latency in seconds by registered route.\n")
	b.WriteString("# TYPE tracker_request_seconds histogram\n")
	for _, route := range sortedKeys(m.seconds) {
		h := m.seconds[route]
		cumulative := uint64(0)
		for i, bound := range requestSecondsBuckets {
			cumulative += h.buckets[i]
			fmt.Fprintf(&b, "tracker_request_seconds_bucket{route=%s,le=%s} %d\n",
				quote(route), quote(strconv.FormatFloat(bound, 'g', -1, 64)), cumulative)
		}
		fmt.Fprintf(&b, "tracker_request_seconds_bucket{route=%s,le=\"+Inf\"} %d\n", quote(route), h.count)
		fmt.Fprintf(&b, "tracker_request_seconds_sum{route=%s} %s\n", quote(route), strconv.FormatFloat(h.sum, 'g', -1, 64))
		fmt.Fprintf(&b, "tracker_request_seconds_count{route=%s} %d\n", quote(route), h.count)
	}

	b.WriteString("# HELP tracker_requests_by_actor_total Requests by the actor the bearer token names.\n")
	b.WriteString("# TYPE tracker_requests_by_actor_total counter\n")
	for _, actor := range sortedKeys(m.actors) {
		fmt.Fprintf(&b, "tracker_requests_by_actor_total{actor=%s} %d\n", quote(actor), m.actors[actor])
	}

	fmt.Fprintf(&b, "# HELP tracker_requests_by_tag_total Requests by the %s header value; tags past %d are counted under %q.\n",
		api.RequestTagHeader, maxTagValues, otherTagValue)
	b.WriteString("# TYPE tracker_requests_by_tag_total counter\n")
	for _, tag := range sortedKeys(m.tags) {
		fmt.Fprintf(&b, "tracker_requests_by_tag_total{tag=%s} %d\n", quote(tag), m.tags[tag])
	}
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func quote(v string) string { return `"` + labelEscaper.Replace(v) + `"` }

func sortedRoutes(requests map[routeStatus]uint64) []routeStatus {
	keys := make([]routeStatus, 0, len(requests))
	for k := range requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].route != keys[j].route {
			return keys[i].route < keys[j].route
		}
		return keys[i].status < keys[j].status
	})
	return keys
}

func sortedKeys[V any](counts map[string]V) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// statusWriter records the first status a handler sends. A handler that never
// calls WriteHeader answers 200, which is the zero value's meaning here.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Flush keeps the response a Flusher for a streaming handler; Unwrap keeps
// http.ResponseController reaching the real writer.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
