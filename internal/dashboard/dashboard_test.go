package dashboard

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoopbackHost(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]bool{
		"127.0.0.1:8787": true, "localhost:8787": true, "[::1]:8787": true, "localhost": true,
		"evil.example.com": false, "evil.example.com:8787": false, "10.0.0.5:8787": false, "": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
	if IsLoopbackAddr("0.0.0.0:80") || !IsLoopbackAddr("127.0.0.1:80") {
		t.Error("IsLoopbackAddr wrong")
	}
}

func TestGuardRefusesRebindingAndWrites(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{}).Handler()
	cases := []struct {
		method, host string
		want         int
	}{
		{"GET", "127.0.0.1:8787", 200},
		{"GET", "evil.example.com:8787", 403},
		{"POST", "127.0.0.1:8787", 405},
		{"DELETE", "localhost:8787", 405},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "/", nil)
		req.Host = c.host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s Host=%s: %d, want %d", c.method, c.host, rec.Code, c.want)
		}
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("missing CSP: %q", csp)
	}
}

// With no page open a worker must not run, however old its last run is; with
// one open it runs once per whole interval.
func TestWorkersRunOnlyWhileAPageIsOpenAndDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	h := NewHub(Config{Now: func() time.Time { return now }})
	var last time.Time
	every := time.Minute

	if h.due(&last, every) {
		t.Fatal("an idle hub ran a worker")
	}
	sub, _ := h.Subscribe()
	if !h.due(&last, every) {
		t.Fatal("a page is open and the worker has never run: it is due")
	}
	if h.due(&last, every) {
		t.Fatal("ran twice inside one interval")
	}
	now = now.Add(every - time.Second)
	if h.due(&last, every) {
		t.Fatal("ran before a whole interval passed")
	}
	now = now.Add(time.Second)
	if !h.due(&last, every) {
		t.Fatal("not due after a whole interval")
	}
	h.Unsubscribe(sub)
	now = now.Add(time.Hour)
	if h.due(&last, every) {
		t.Fatal("ran after the last page left")
	}
}

func TestPollsFillStateAndFeedReachesAPage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	fed := false
	h := NewHub(Config{
		Feed: func() []Entry {
			if fed {
				return nil
			}
			fed = true
			return []Entry{
				{At: now, Rig: "gastown", Kind: "daemon", Text: "gt-abc slung to agate (gastown)", Class: "dispatch"},
				{At: now, Rig: "gastown", Kind: "daemon", Text: "stages: lint 18s, gate 5m1s, om 2m", Class: "plain"},
			}
		},
		Health:  func() Health { return Health{Line: "GREEN ok", Verdict: "green"} },
		Machine: func() (Machine, error) { return Machine{Load1: 7.5, At: now}, nil },
		Summary: func() Summary {
			return Summary{Polecats: []Polecat{{Rig: "gastown", Name: "agate", Bead: "gt-abc", Title: "the title", State: StateWorking}}}
		},
		Now: func() time.Time { return now.Add(10 * time.Minute) },
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollMachine() // the load sample precedes the gate line, as it does in a running hub
	h.pollHealth()
	h.pollSummary()
	h.pollFeed()

	st := h.State()
	if st.Health.Verdict != "green" || st.Machine.Load1 != 7.5 || st.Viewers != 1 {
		t.Fatalf("state = %+v", st)
	}
	if len(st.Polecats) != 1 {
		t.Fatalf("polecats = %+v", st.Polecats)
	}
	if p := st.Polecats[0]; p.Title != "the title" || p.ElapsedSec != 600 {
		t.Errorf("polecat = %+v (want its title, and 600s from the dispatch line to now)", p)
	}

	var frames []string
	for len(page.C) > 0 {
		frames = append(frames, string(<-page.C))
	}
	joined := strings.Join(frames, "")
	if strings.Count(joined, "event: entry") != 2 || !strings.Contains(joined, "event: state") {
		t.Errorf("page frames: %q", joined)
	}

	late, initial := h.Subscribe()
	defer h.Unsubscribe(late)
	var backlog bool
	for _, f := range initial {
		backlog = backlog || strings.HasPrefix(string(f), "event: backlog")
	}
	if !backlog {
		t.Error("a page that connects late got no backlog")
	}
}

// A page that stops reading is dropped, not waited on.
func TestSlowPageIsDropped(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{})
	sub, _ := h.Subscribe()
	h.mu.Lock()
	for i := 0; i < 100; i++ {
		h.broadcastLocked(frame("entry", Entry{Seq: int64(i)}))
	}
	_, still := h.subs[sub]
	h.mu.Unlock()
	if still {
		t.Fatal("slow page still subscribed")
	}
	for range sub.C { // buffered frames drain, then the channel is closed
	}
}

func TestParseLoadavgAndTop(t *testing.T) {
	t.Parallel()
	var m Machine
	if err := parseLoadavg("{ 6.71 16.49 28.11 }\n", &m); err != nil || m.Load1 != 6.71 || m.Load15 != 28.11 {
		t.Fatalf("parseLoadavg: %v %+v", err, m)
	}
	if err := parseLoadavg("garbage", &m); err == nil {
		t.Error("garbage load accepted")
	}
	ps := " 95.0 /usr/local/go/bin/go\n 40.2 /usr/bin/go\n 30.0 /usr/bin/dolt\n  0.1 launchd\n"
	top := parseTop(ps, 5)
	if len(top) != 2 || top[0].Name != "go" || top[0].CPU < 135 || top[1].Name != "dolt" {
		t.Fatalf("parseTop = %+v", top)
	}
}

func TestSlungRegexReadsBothDispatchLineShapes(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"gt-m36as slung to agate (gastown)":                                   "gt-m36as",
		"spec_dispatch: dispatched: gt-4k3fj.14: slung to gastown/agate on x": "gt-4k3fj.14",
	} {
		m := slungRe.FindStringSubmatch(text)
		if m == nil || m[1] != want {
			t.Errorf("%q -> %v, want %q", text, m, want)
		}
	}
}

// streamWriter is a ResponseWriter that cancels the request once it has been
// written to, so serveStream returns without a goroutine or a socket.
type streamWriter struct {
	mu     sync.Mutex
	header http.Header
	buf    bytes.Buffer
	cancel context.CancelFunc
}

func (w *streamWriter) Header() http.Header { return w.header }
func (w *streamWriter) WriteHeader(int)     {}
func (w *streamWriter) Flush()              { w.cancel() }
func (w *streamWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(b)
}

func TestStreamSendsStateFirstAndUnsubscribes(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	w := &streamWriter{header: http.Header{}, cancel: cancel}
	req := httptest.NewRequest("GET", "/stream", nil).WithContext(ctx)
	h.serveStream(w, req)
	if ct := w.header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	if !strings.HasPrefix(w.buf.String(), "event: state") {
		t.Fatalf("first frame %q", w.buf.String())
	}
	if n := h.viewers(); n != 0 {
		t.Fatalf("%d pages still subscribed after the stream ended", n)
	}
}

func TestPolecatOverlaysQuietAndGating(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	active := now.Add(-11 * time.Minute)
	recent := now.Add(-time.Minute)
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Summary: func() Summary {
			return Summary{Polecats: []Polecat{
				{Rig: "g", Name: "silent", Bead: "gt-1", State: StateWorking, HasSession: true, LastActive: &active},
				{Rig: "g", Name: "busy", Bead: "gt-2", State: StateWorking, HasSession: true, LastActive: &recent},
				{Rig: "g", Name: "landing", Bead: "gt-3", State: StateQueued},
				{Rig: "g", Name: "free", State: StateIdle},
			}}
		},
		Feed: func() []Entry {
			return []Entry{{At: now, Kind: "daemon", Text: "landing_worker: [land] gt-3: merged abc onto origin/main (def) as ghi; gating the merged tree, then om review"}}
		},
	})
	h.pollSummary()
	h.pollFeed()
	got := map[string]string{}
	for _, p := range h.State().Polecats {
		got[p.Name] = p.State
	}
	want := map[string]string{"silent": StateQuiet, "busy": StateWorking, "landing": StateGating, "free": StateIdle}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	// the landing worker's verdict ends the gating phase
	h.cfg.Feed = func() []Entry {
		return []Entry{{At: now, Kind: "daemon", Text: "landing_worker: [land] gt-3: landed abc on origin/main (patch-id x)"}}
	}
	h.pollFeed()
	for _, p := range h.State().Polecats {
		if p.Name == "landing" && p.State != StateQueued {
			t.Errorf("after landing, state = %q, want queued (the summary's own view)", p.State)
		}
	}
}
