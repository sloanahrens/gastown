package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// A worker runs on every tick a page is watching. Asking the wall clock whether
// a whole interval had passed threw the tick away whenever it landed a hair
// short of one, which is about half of them: the live dashboard's 10s machine
// worker sampled at an effective 16s, so the Forgejo pane's 3min refresh became
// 5-6min and a landing could sit unseen for two intervals (gt-faml5).
func TestWorkersRunOnEveryTickWhileAPageIsOpen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	h := NewHub(Config{Now: func() time.Time { return now }})
	// The page is open before the worker starts, so the connect's wake cannot
	// race the ticks: every run below comes from a tick.
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	ticks := make(chan time.Time)
	ran := make(chan struct{})
	var runs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.work(ctx, ticks, time.Minute, func() {
			runs.Add(1)
			select {
			case ran <- struct{}{}:
			case <-ctx.Done(): // a failed test stops listening for runs
			}
		})
	}()
	t.Cleanup(func() { cancel(); <-stopped })

	// A send returning means the worker has taken the tick, not that it has run
	// it: the worker checks its context after taking a tick and drops the tick if
	// the context has already ended. So each tick is run to completion (the
	// worker reports it on ran) before the next is sent, and the context is
	// cancelled only after the last one. The deadline below is a failure guard: it
	// fires only when the worker has stopped taking or running ticks, so a passing
	// run never waits on it and no pass depends on timing.
	const stuck = 5 * time.Second
	for i := 0; i < 5; i++ {
		select {
		case ticks <- now:
		//testpolicy:allow no-sleep — failure-only watchdog: a passing run never reaches it
		case <-time.After(stuck):
			t.Fatalf("worker did not take tick %d within %v", i+1, stuck)
		}
		select {
		case <-ran:
		//testpolicy:allow no-sleep — failure-only watchdog: a passing run never reaches it
		case <-time.After(stuck):
			t.Fatalf("worker took tick %d but did not run it within %v", i+1, stuck)
		}
	}
	cancel()
	<-stopped

	if got := runs.Load(); got != 5 {
		t.Fatalf("%d runs for 5 ticks, want 5: a tick IS the interval and may not be skipped", got)
	}
}

// A worker whose context has ended starts no further poll, even with a tick
// waiting and a page open. select picks at random among ready cases, so without
// the check a stopped hub started one more poll about half the time, and that
// poll's children were in flight when the restart replaced the process
// (gt-9rwye). The repetition makes the random pick reproducible.
func TestWorkerStartsNoPollOnceItsContextEnded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	h := NewHub(Config{Now: func() time.Time { return now }})
	page, _ := h.Subscribe() // the connect's wake is spent before the worker starts
	defer h.Unsubscribe(page)

	var runs atomic.Int64
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ticks := make(chan time.Time, 1)
		ticks <- now
		h.work(ctx, ticks, time.Minute, func() { runs.Add(1) })
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("%d polls started after the context ended, want 0", got)
	}
}

// A page connecting releases every worker at once. Handing one token per worker
// down a shared channel released whichever workers happened to be parked and
// left the rest to their ticker, so a pane could sit on a stale snapshot for a
// whole interval after the page was opened (gt-faml5).
func TestSubscribeReleasesEveryParkedWorker(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{})
	// The channel a worker captured as it parked, before the page connected.
	parked := h.wakeChan()

	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	select {
	case <-parked:
	default:
		t.Fatal("the connecting page left the parked workers waiting for their ticker")
	}
	if next := h.wakeChan(); next == parked {
		t.Fatal("the wake channel was not replaced, so the next page would release nothing")
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
			return Summary{Polecats: []Polecat{{Rig: "gastown", Name: "agate", Bead: "gt-abc", Title: "the title", State: StateWorking, Hooked: &now}}}
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

// The seat's dispatch time is carried to the pane as its age, so the row reads
// the same clock the overrun guard judges.
func TestTheSeatsDispatchTimeDatesTheAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)
	hooked := now.Add(-90 * time.Minute)
	h := NewHub(Config{
		Summary: func() Summary {
			return Summary{Polecats: []Polecat{{Rig: "gastown", Name: "agate", Bead: "gt-abc", State: StateWorking, Hooked: &hooked}}}
		},
		Now: func() time.Time { return now },
	})
	h.pollSummary()

	p := h.State().Polecats[0]
	if p.Slung == nil || !p.Slung.Equal(hooked) || p.ElapsedSec != 5400 {
		t.Errorf("polecat = %+v (want the seat's hook time, 5400s ago)", p)
	}
}

// The seat's dispatch time is the bead's own clock — the first dispatch across
// seats, which the overrun guard judges — so a feed line for a later sling of
// the same bead does not shorten the age (gt-tzbtf).
func TestTheSeatsDispatchTimeDatesTheAgeOverALaterFeedLine(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)
	first, reslung := now.Add(-90*time.Minute), now.Add(-10*time.Minute)
	h := NewHub(Config{
		Feed: func() []Entry {
			return []Entry{{At: reslung, Rig: "gastown", Kind: "daemon", Text: "gt-abc slung to agate (gastown)"}}
		},
		Summary: func() Summary {
			return Summary{Polecats: []Polecat{{Rig: "gastown", Name: "agate", Bead: "gt-abc", State: StateWorking, Hooked: &first}}}
		},
		Now: func() time.Time { return now },
	})
	h.pollSummary()
	h.pollFeed()

	p := h.State().Polecats[0]
	if p.Slung == nil || !p.Slung.Equal(first) || p.ElapsedSec != 5400 {
		t.Errorf("polecat = %+v (want the bead's first dispatch, 5400s ago)", p)
	}
}

// A seat with no dispatch time is not dated by the feed. The feed is a rolling
// window with no record of the bead's done events, so one of its lines can
// belong to an earlier run; the Age is left undated rather than dated from a
// start the overrun guard does not use (gt-tzbtf).
func TestAFeedDispatchLineDoesNotDateASeatWithNoDispatchTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Feed: func() []Entry {
			return []Entry{
				{At: now.Add(-90 * time.Minute), Rig: "gastown", Kind: "daemon", Text: "gt-abc slung to agate (gastown)"},
				{At: now.Add(-10 * time.Minute), Rig: "gastown", Kind: "daemon", Text: "gt-abc slung to nux (gastown)"},
			}
		},
		Summary: func() Summary {
			return Summary{Polecats: []Polecat{{Rig: "gastown", Name: "nux", Bead: "gt-abc", State: StateWorking}}}
		},
		Now: func() time.Time { return now },
	})
	h.pollSummary()
	h.pollFeed()

	if p := h.State().Polecats[0]; p.Slung != nil || p.ElapsedSec != 0 {
		t.Errorf("polecat = %+v (want no dispatch time, not the feed's line)", p)
	}
}

// The Rigs panel is published from the readers the hub already polls, whichever
// order they report in: the queue names the rigs and carries their work, the
// summary's polecats are the seats.
func TestRigsPanelIsJoinedFromTheQueueAndTheSeats(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Queue: func() *Queue {
			return &Queue{At: now, Rigs: []Rig{
				{Name: "gastown", Ready: intp(2), Landing: intp(1)},
				{Name: "mango", Parked: true}, // unreadable: counts stay unknown
			}}
		},
		Summary: func() Summary {
			return Summary{Polecats: []Polecat{
				{Rig: "gastown", Name: "free", State: StateIdle},
				{Rig: "gastown", Name: "busy", State: StateWorking},
			}}
		},
		RigTheme: func(rig string) (string, []string) {
			return rig + "-theme", []string{"one", "two"}
		},
	})
	h.pollSummary() // the seats report first: the next queue poll must still join them
	h.pollQueue()
	rows := h.State().Rigs
	if len(rows) != 2 {
		t.Fatalf("rigs = %+v", rows)
	}
	if r := rows[0]; r.Name != "gastown" || r.Parked || *r.Seats != 1 || *r.Ready != 2 || *r.Landing != 1 {
		t.Errorf("gastown = %+v", r)
	}
	// The Rigs panel names each rig's theme, so the page can place a short
	// polecat name back in its rig: the theme and its samples ride in the state
	// the page is sent (gt-yieek).
	if r := rows[0]; r.Theme != "gastown-theme" || len(r.Names) != 2 {
		t.Errorf("gastown theme = %q %v, want the reader's theme and samples", r.Theme, r.Names)
	}
	b, err := json.Marshal(h.State())
	if err != nil {
		t.Fatal(err)
	}
	if want := `"theme":"gastown-theme","names":["one","two"]`; !strings.Contains(string(b), want) {
		t.Errorf("state payload has no %s", want)
	}
	if r := rows[1]; r.Name != "mango" || !r.Parked || *r.Seats != 0 || r.Ready != nil || r.Landing != nil {
		t.Errorf("mango = %+v", r)
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
