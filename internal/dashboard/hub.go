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
	gating   map[string]bool      // beads the landing worker is gating right now
	polecats []Polecat

	// alerts decides what needs the operator's attention, and prev is the
	// snapshot it compares the next one against.
	alerts *Alerter
	prev   State

	// wake nudges every worker when a page connects after an idle spell, so
	// the first page does not wait out a whole interval for fresh data.
	wake chan struct{}

	detailMu    sync.Mutex
	detailCache map[string]detailEntry
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
		gating: map[string]bool{},
		wake:   make(chan struct{}),
		alerts: NewAlerter(cfg.Now),
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
	if h.cfg.OM != nil {
		start(h.cfg.OMEvery, h.pollOM)
	}
	if h.cfg.TierSweep != nil {
		start(h.cfg.TierSweepEvery, h.pollTierSweep)
	}
	if h.cfg.Dispatch != nil {
		start(h.cfg.DispatchEvery, h.pollDispatch)
	}
	if h.cfg.Queue != nil {
		start(h.cfg.QueueEvery, h.pollQueue)
	}
	if h.cfg.Trend != nil {
		start(h.cfg.TrendEvery, h.pollTrend)
	}
	wg.Wait()
}

// work runs fn every interval, but only while a page is watching. A page that
// connects wakes the worker, which runs fn at once if its last run is a whole
// interval old.
func (h *Hub) work(ctx context.Context, every time.Duration, fn func()) {
	var last time.Time
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.wake:
		}
		if h.due(&last, every) {
			fn()
		}
	}
}

// due reports whether a worker whose last run was at *last should run now,
// and if so records the run. It is false when no page is watching, and false
// when the last run is less than a whole interval old.
func (h *Hub) due(last *time.Time, every time.Duration) bool {
	if h.viewers() == 0 {
		return false
	}
	now := h.cfg.Now()
	if now.Sub(*last) < every {
		return false
	}
	*last = now
	return true
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
	if len(h.subs) == 0 {
		// Polling starts here. The snapshot from before the gap is not a
		// baseline for the alert rules: a polecat that stalled while nobody was
		// watching must not read as a change that just happened.
		h.prev = State{}
		h.alerts.Baseline()
	}
	h.subs[s] = struct{}{}
	initial := [][]byte{frame("state", h.stateLocked())}
	if len(h.ring) > 0 {
		initial = append(initial, frame("backlog", h.ring))
	}
	h.mu.Unlock()
	// Wake every worker; a non-blocking send per worker is enough because
	// each one re-checks its own age. The count covers the workers Config can
	// start, with room to spare.
	for i := 0; i < 12; i++ {
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
	st.Polecats = h.polecatsLocked(st.Now)
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

// observeLocked runs the alert rules over the state change this poll just made,
// broadcasts each new alert to the open pages, and keeps the last few for
// display. The snapshot it is compared against is the previous poll's, so a
// rule that needs a run length or a transition sees one poll at a time. It
// returns how many alerts it raised, which is a state change of its own.
func (h *Hub) observeLocked(entries []Entry) int {
	next := h.state
	next.Now = h.cfg.Now()
	next.Polecats = h.polecatsLocked(next.Now)
	raised := 0
	for _, al := range h.alerts.Observe(h.prev, next, entries) {
		raised++
		h.state.Alerts = append(h.state.Alerts, al)
		if n := len(h.state.Alerts); n > alertsKept {
			h.state.Alerts = append([]Alert(nil), h.state.Alerts[n-alertsKept:]...)
		}
		h.broadcastLocked(frame("alert", al))
	}
	h.prev = next
	return raised
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
	h.observeLocked(nil)
	h.publishLocked()
}

func (h *Hub) pollMachine() {
	m, err := h.cfg.Machine()
	if err != nil {
		return
	}
	if h.cfg.LoadSample != nil {
		h.cfg.LoadSample(m.At, m.Load1)
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

func (h *Hub) pollOM() {
	om := h.cfg.OM()
	if om == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.OM = om
	h.publishLocked()
}

func (h *Hub) pollTierSweep() {
	ts := h.cfg.TierSweep()
	if ts == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.TierSweep = ts
	h.publishLocked()
}

func (h *Hub) pollDispatch() {
	d := h.cfg.Dispatch()
	if d == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Dispatch = d
	h.observeLocked(nil)
	h.publishLocked()
}

func (h *Hub) pollQueue() {
	q := h.cfg.Queue()
	if q == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Queue = q
	h.state.Rigs = RigRows(q, h.polecats)
	h.publishLocked()
}

func (h *Hub) pollTrend() {
	tr := h.cfg.Trend()
	if tr == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Trend = tr
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
	h.mu.Lock()
	defer h.mu.Unlock()
	h.polecats = s.Polecats
	h.state.Summary = &s
	h.state.SummaryAt = h.cfg.Now()
	// The seats are half of the Rigs panel: a fresh seat reading re-joins it.
	h.state.Rigs = RigRows(h.state.Queue, h.polecats)
	h.observeLocked(nil)
	h.publishLocked()
}

// polecatsLocked adds the live parts to the summary's polecats: the dispatch
// time off the feed, how long the session has been silent, and the gating
// phase off the landing worker's lines. Working becomes quiet here, not in the
// summary, so a session that goes silent shows within a poll, not a minute.
func (h *Hub) polecatsLocked(now time.Time) []Polecat {
	out := make([]Polecat, len(h.polecats))
	copy(out, h.polecats)
	for i := range out {
		p := &out[i]
		if p.Bead == "" {
			continue
		}
		if at, ok := h.slung[p.Bead]; ok {
			at := at
			p.Slung = &at
			if d := now.Sub(at); d > 0 {
				p.ElapsedSec = int64(d / time.Second)
			}
		}
		if p.LastActive != nil {
			if d := now.Sub(*p.LastActive); d > 0 {
				p.QuietSec = int64(d / time.Second)
			}
		}
		switch {
		case h.gating[p.Bead] && p.State != StateIdle:
			p.State = StateGating
		case p.State == StateWorking && p.HasSession && time.Duration(p.QuietSec)*time.Second >= QuietAfter:
			p.State = StateQuiet
		}
	}
	return out
}

var (
	slungRe = regexp.MustCompile(`([A-Za-z0-9][\w.-]*):? slung to `)
	// A bead is gated from the worker's "merged ... gating" line until it lands,
	// is rejected, or reports its stage times.
	gateStartRe = regexp.MustCompile(`\[land\] (\S+): merged .*gating`)
	gateEndRe   = regexp.MustCompile(`\[land\] (\S+): (?:stages: |landed |rejected )`)
)

func (h *Hub) pollFeed() {
	entries := h.cfg.Feed()
	if len(entries) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := false
	for i := range entries {
		h.seq++
		entries[i].Seq = h.seq
		e := entries[i]
		h.ring = append(h.ring, e)
		if m := slungRe.FindStringSubmatch(e.Text); m != nil {
			h.slung[m[1]] = e.At
			changed = true
		}
		if m := gateStartRe.FindStringSubmatch(e.Text); m != nil {
			h.gating[m[1]] = true
			changed = true
		} else if m := gateEndRe.FindStringSubmatch(e.Text); m != nil && h.gating[m[1]] {
			delete(h.gating, m[1])
			changed = true
		}
		h.broadcastLocked(frame("entry", e))
	}
	if n := len(h.ring); n > h.cfg.RingSize {
		h.ring = append([]Entry(nil), h.ring[n-h.cfg.RingSize:]...)
	}
	if alerts := h.observeLocked(entries); alerts > 0 {
		changed = true
	}
	if changed {
		h.publishLocked()
	}
}
