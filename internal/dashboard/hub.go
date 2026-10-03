package dashboard

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"
)

const (
	loadHistory = 360 // samples kept: an hour at the default 10s
	gateHistory = 30
)

// Hub polls the readers while a page is open and fans their results out.
type Hub struct {
	cfg Config

	mu       sync.Mutex
	state    State
	ring     []Entry
	seq      int64
	subs     map[*Sub]struct{}
	slung    map[string]time.Time // bead -> when a dispatch line named it
	titles   map[string]string
	seatRefs []SeatRef

	// wake nudges every worker when a page connects after an idle spell, so
	// the first page does not wait out a whole interval for fresh data.
	wake chan struct{}
}

// Sub is one open page.
type Sub struct {
	C      chan []byte // framed SSE events; closed when the hub drops the page
	closed bool
}

// NewHub builds a hub. Call Run to start it.
func NewHub(cfg Config) *Hub {
	cfg.defaults()
	return &Hub{
		cfg:    cfg,
		subs:   map[*Sub]struct{}{},
		slung:  map[string]time.Time{},
		titles: map[string]string{},
		wake:   make(chan struct{}),
	}
}

// Run starts the workers and blocks until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(every time.Duration, fn func()) {
		if fn == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.work(ctx, every, fn)
		}()
	}
	if h.cfg.Feed != nil {
		start(h.cfg.FeedEvery, h.pollFeed)
	}
	if h.cfg.Summary != nil {
		start(h.cfg.SummaryEvery, h.pollSummary)
	}
	if h.cfg.Health != nil {
		start(h.cfg.HealthEvery, h.pollHealth)
	}
	if h.cfg.Machine != nil {
		start(h.cfg.MachineEvery, h.pollMachine)
	}
	if h.cfg.Spend != nil {
		start(h.cfg.SpendEvery, h.pollSpend)
	}
	wg.Wait()
}

// work runs fn every interval, but only while a page is watching. A page that
// connects wakes the worker, which runs fn at once if its last run is a whole
// interval old.
func (h *Hub) work(ctx context.Context, every time.Duration, fn func()) {
	var last time.Time
	run := func() {
		if h.viewers() == 0 {
			return
		}
		if h.cfg.Now().Sub(last) < every {
			return
		}
		last = h.cfg.Now()
		fn()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.wake:
		}
		run()
	}
}

func (h *Hub) viewers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Subscribe registers a page and returns the frames that bring it up to date.
func (h *Hub) Subscribe() (*Sub, [][]byte) {
	s := &Sub{C: make(chan []byte, 64)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	initial := [][]byte{frame("state", h.stateLocked())}
	if len(h.ring) > 0 {
		initial = append(initial, frame("backlog", h.ring))
	}
	h.mu.Unlock()
	// Wake every worker; a non-blocking send per worker is enough because
	// each one re-checks its own age.
	for i := 0; i < 5; i++ {
		select {
		case h.wake <- struct{}{}:
		default:
		}
	}
	return s, initial
}

// Unsubscribe drops a page.
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(s)
}

func (h *Hub) dropLocked(s *Sub) {
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	if !s.closed {
		s.closed = true
		close(s.C)
	}
}

// State is a copy of the current state, for /api/state and tests.
func (h *Hub) State() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stateLocked()
}

func (h *Hub) stateLocked() State {
	st := h.state
	st.Now = h.cfg.Now()
	st.Viewers = len(h.subs)
	st.Seats = h.seatsLocked(st.Now)
	return st
}

// broadcastLocked sends one frame to every page. A page whose buffer is full
// is dropped; its EventSource reconnects and is brought up to date again.
func (h *Hub) broadcastLocked(f []byte) {
	for s := range h.subs {
		select {
		case s.C <- f:
		default:
			h.dropLocked(s)
		}
	}
}

func (h *Hub) publishLocked() {
	h.broadcastLocked(frame("state", h.stateLocked()))
}

func frame(event string, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`null`)
	}
	return append(append([]byte("event: "+event+"\ndata: "), b...), '\n', '\n')
}

func (h *Hub) pollHealth() {
	hl := h.cfg.Health()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Health = hl
	h.publishLocked()
}

func (h *Hub) pollMachine() {
	m, err := h.cfg.Machine()
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Machine = m
	h.state.Loads = append(h.state.Loads, LoadPoint{At: m.At, Load: m.Load1})
	if n := len(h.state.Loads); n > loadHistory {
		h.state.Loads = append([]LoadPoint(nil), h.state.Loads[n-loadHistory:]...)
	}
	h.publishLocked()
}

func (h *Hub) pollSpend() {
	sp := h.cfg.Spend()
	if len(sp) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Spend = sp
	h.publishLocked()
}

func (h *Hub) pollSummary() {
	s := h.cfg.Summary()
	titles := map[string]string{}
	if h.cfg.Title != nil {
		for _, r := range s.Seats {
			if t := h.cfg.Title(r.Bead); t != "" {
				titles[r.Bead] = t
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seatRefs = s.Seats
	for id, t := range titles {
		h.titles[id] = t
	}
	h.state.Summary = &s
	h.state.SummaryAt = h.cfg.Now()
	h.publishLocked()
}

// seatsLocked joins the seat list with the titles and dispatch times known.
func (h *Hub) seatsLocked(now time.Time) []Seat {
	out := make([]Seat, 0, len(h.seatRefs))
	for _, r := range h.seatRefs {
		s := Seat{SeatRef: r, Title: h.titles[r.Bead]}
		if at, ok := h.slung[r.Bead]; ok {
			at := at
			s.Slung = &at
			if d := now.Sub(at); d > 0 {
				s.Elapsed = int64(d / time.Second)
			}
		}
		out = append(out, s)
	}
	return out
}

var (
	slungRe  = regexp.MustCompile(`([A-Za-z0-9][\w.-]*):? slung to `)
	stagesRe = regexp.MustCompile(`\bstages: `)
	gateRe   = regexp.MustCompile(`\bgate ((?:\d+h)?(?:\d+m)?\d+(?:\.\d+)?s)\b`)
)

func (h *Hub) pollFeed() {
	entries := h.cfg.Feed()
	if len(entries) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := false
	for _, e := range entries {
		h.seq++
		e.Seq = h.seq
		h.ring = append(h.ring, e)
		if m := slungRe.FindStringSubmatch(e.Text); m != nil {
			h.slung[m[1]] = e.At
			changed = true
		}
		if g, ok := gatePoint(e, h.loadAtLocked(e.At)); ok {
			h.state.Gates = append(h.state.Gates, g)
			if n := len(h.state.Gates); n > gateHistory {
				h.state.Gates = append([]GatePoint(nil), h.state.Gates[n-gateHistory:]...)
			}
			changed = true
		}
		h.broadcastLocked(frame("entry", e))
	}
	if n := len(h.ring); n > h.cfg.RingSize {
		h.ring = append([]Entry(nil), h.ring[n-h.cfg.RingSize:]...)
	}
	if changed {
		h.publishLocked()
	}
}

// gatePoint reads the gate's wall time off a "stages: lint 18s, gate 92s"
// line, the one the landing worker writes for every gated merge.
func gatePoint(e Entry, load *float64) (GatePoint, bool) {
	if e.Kind != "daemon" && e.Kind != "landings" {
		return GatePoint{}, false
	}
	m := gateRe.FindStringSubmatch(e.Text)
	if m == nil || !stagesRe.MatchString(e.Text) {
		return GatePoint{}, false
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		return GatePoint{}, false
	}
	return GatePoint{At: e.At, Secs: d.Seconds(), Load: load, Text: e.Text}, true
}

// loadAtLocked is the load sample nearest at or before t. A gate older than
// the first sample has no load to report, and says so with nil rather than
// borrowing the current reading.
func (h *Hub) loadAtLocked(t time.Time) *float64 {
	pts := h.state.Loads
	for i := len(pts) - 1; i >= 0; i-- {
		if !pts[i].At.After(t) {
			v := pts[i].Load
			return &v
		}
	}
	return nil
}
