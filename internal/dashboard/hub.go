package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

const (
	loadHistory = 360 // samples kept: an hour at the default 10s
)

// The lists the state frame leaves out, each sent as its own event. They are the
// bulk of the frame and change at most about once a minute, so each goes out only
// when its bytes changed (gt-6ecd0).
const (
	evTrendRecent  = "trend.recent"
	evQueueReady   = "queue.ready"
	evQueueBlocked = "queue.blocked"
)

// Hub polls the readers while a page is open and fans their results out.
type Hub struct {
	cfg Config

	mu       sync.Mutex
	state    State
	ring     []Entry
	seq      int64
	subs     map[*Sub]struct{}
	gating   map[string]bool // beads the landing worker is gating right now
	polecats []Polecat
	jevGood  map[string]time.Time // battery -> when its line last read good

	// cloudAt is the last cloud read and cloudMissing whether that machine had
	// no cloud patrol and no app power line, which is what re-reads one at the
	// slow interval only.
	cloudAt      *time.Time
	cloudMissing bool

	// listSent is the bytes each list event last carried, by event name, so a
	// poll sends a list again only when it changed (gt-6ecd0).
	listSent map[string][]byte

	// alerts decides what needs the operator's attention, and prev is the
	// snapshot it compares the next one against.
	alerts *Alerter
	prev   State
	// queueItems is the attention queue's items as the last poll read them,
	// which classify the panel's rows when it next publishes, and queueFailed
	// whether that read could be made at all (gt-tv5v2).
	queueItems  []attention.Item
	queueFailed bool

	// wake releases every parked worker when a page connects after an idle
	// spell, so the first page does not wait out a whole interval for fresh
	// data. Closing the channel a worker is parked on is what releases all of
	// them; a shared channel of tokens released whichever workers happened to
	// be parked and left the rest to their ticker (gt-faml5).
	wakeMu sync.Mutex
	wake   chan struct{}

	detailMu    sync.Mutex
	detailCache map[string]detailEntry
	detailGate  detailGate // bead-detail reads join Run's lifecycle (gt-fekq1)

	// costProviders builds the Cost panel's provider rows, in order. Nil is the
	// default list (BuildCostProviders); a test injects its own list here.
	costProviders []costProviderBuilder

	// costRows is the Cost panel's rows as last built, nil once a cost input has
	// changed since (the spend, the plan, the charts or the Jev value clear it).
	// costKey is what the rows were built under. The rows are rebuilt when either
	// is out of date, so State() does not rebuild them on every call.
	costRows []CostProvider
	costKey  costRowsKey
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
		cfg:      cfg,
		subs:     map[*Sub]struct{}{},
		gating:   map[string]bool{},
		jevGood:  map[string]time.Time{},
		listSent: map[string][]byte{},
		wake:     make(chan struct{}),
		alerts:   NewAlerter(cfg.Now),
	}
}

// Run starts the workers and blocks until ctx ends and every poll and bead-detail
// read in flight has returned. Once ctx ends, new detail reads are refused with a
// retryable error until Run is called again. It may be called again on the same
// hub once it has returned: the in-place restart stops the hub before exec and
// starts it again if exec fails.
func (h *Hub) Run(ctx context.Context) {
	var wg sync.WaitGroup
	h.detailGate.open()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		h.detailGate.closeAndDrain()
	}()
	start := func(every time.Duration, fn func()) {
		if fn == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			h.work(ctx, t.C, every, fn)
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
	if h.cfg.Cloud != nil {
		start(h.cfg.CloudEvery, h.pollCloud)
	}
	if h.cfg.Mango != nil {
		start(h.cfg.MangoEvery, h.pollMango)
	}
	if h.cfg.Spend != nil {
		start(h.cfg.SpendEvery, h.pollSpend)
	}
	if h.cfg.JevSpend != nil {
		start(h.cfg.JevSpendEvery, h.pollJevSpend)
	}
	if h.cfg.JevScorecard != nil {
		start(h.cfg.JevScorecardEvery, h.pollJevScorecard)
	}
	if h.cfg.OM != nil {
		start(h.cfg.OMEvery, h.pollOM)
	}
	if h.cfg.TierSweep != nil {
		start(h.cfg.TierSweepEvery, h.pollTierSweep)
	}
	if h.cfg.Forgejo != nil {
		start(h.cfg.ForgejoEvery, h.pollForgejo)
	}
	if h.cfg.News != nil {
		start(h.cfg.NewsEvery, h.pollNews)
	}
	if h.cfg.Deploys != nil {
		start(h.cfg.DeploysEvery, h.pollDeploys)
	}
	if h.cfg.Reports != nil {
		start(h.cfg.ReportsEvery, h.pollReports)
	}
	if h.cfg.Questions != nil {
		start(h.cfg.QuestionsEvery, h.pollQuestions)
	}
	if h.cfg.Overseer != nil {
		start(h.cfg.OverseerEvery, h.pollOverseer)
	}
	if h.cfg.DeepReview != nil {
		start(h.cfg.DeepReviewEvery, h.pollDeepReview)
	}
	if h.cfg.Usage != nil {
		start(h.cfg.UsageEvery, h.pollUsage)
	}
	if h.cfg.ClaudePlan != nil {
		start(h.cfg.ClaudePlanEvery, h.pollClaudePlan)
	}
	if h.cfg.Rework != nil {
		start(h.cfg.ReworkEvery, h.pollRework)
	}
	if h.cfg.Escalation != nil {
		start(h.cfg.EscalationEvery, h.pollEscalation)
	}
	if h.cfg.Dispatch != nil {
		start(h.cfg.DispatchEvery, h.pollDispatch)
	}
	if h.cfg.Attention != nil {
		start(h.cfg.AttentionEvery, h.pollAttention)
	}
	if h.cfg.Models != nil {
		start(h.cfg.ModelsEvery, h.pollModels)
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
// interval old. Once ctx ends no further fn starts and the one running is
// waited out, so Run returning means every poll, and every child process a poll
// started, has finished (gt-9rwye).
//
// A tick is the interval itself, so it runs fn whenever a page is open. Asking
// due there too compared the wall clock against the last run, which the tick
// lands on within scheduler jitter: roughly every other tick fell a hair short
// of a whole interval and was thrown away, halving every reader's real rate
// (gt-faml5, measured live on the 10s machine worker).
func (h *Hub) work(ctx context.Context, ticks <-chan time.Time, every time.Duration, fn func()) {
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if h.viewers() == 0 {
				continue
			}
			if ctx.Err() != nil { // select picks at random when both are ready
				return
			}
			last = h.cfg.Now()
			fn()
		case <-h.wakeChan():
			if ctx.Err() != nil {
				return
			}
			if h.due(&last, every) {
				fn()
			}
		}
	}
}

// due reports whether a worker whose last run was at *last should run now,
// and if so records the run. It is false when no page is watching, and false
// when the last run is less than a whole interval old. It rate-limits the wake
// path only: a tick has already waited out its interval.
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

// wakeAll releases every worker parked on the wake channel by closing it, and
// leaves a fresh one for the next connect to close.
func (h *Hub) wakeAll() {
	h.wakeMu.Lock()
	defer h.wakeMu.Unlock()
	close(h.wake)
	h.wake = make(chan struct{})
}

// wakeChan is the wake channel a worker parks on. Reading it under the same
// lock as the swap is what keeps a worker from parking on a channel that was
// already closed: the closed one is released at once, and a worker that reaches
// this after the swap has just finished an iteration, so it is already current.
func (h *Hub) wakeChan() <-chan struct{} {
	h.wakeMu.Lock()
	defer h.wakeMu.Unlock()
	return h.wake
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
	// Every list goes out whatever its bytes were, and ahead of the state that
	// draws them, so a page that connects late has both lists in its first frames.
	initial := h.listFramesLocked(true)
	initial = append(initial, frame("state", h.stateFrameLocked()))
	if len(h.ring) > 0 {
		initial = append(initial, frame("backlog", h.ring))
	}
	h.mu.Unlock()
	// One close releases every worker, whatever race it is in; each re-checks
	// its own age.
	h.wakeAll()
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
	st.Reads = h.readsLocked(st.Now)
	st.CostProviders = h.costProviderRowsLocked(st.Now, st.Reads)
	st.Mango = mangoAt(st.Mango, st.Now)
	st.Strip = stripAlarms(&st)
	return st
}

// stateFrameLocked is the body of the state event: stateLocked without the lists
// that travel as their own events. The Queue and Trend it trims are copies, so
// the lists the hub keeps, and what State returns, are untouched.
func (h *Hub) stateFrameLocked() State {
	st := h.stateLocked()
	if q := st.Queue; q != nil {
		trimmed := *q
		trimmed.Ready, trimmed.Blocked = nil, nil
		st.Queue = &trimmed
	}
	if t := st.Trend; t != nil {
		trimmed := *t
		trimmed.Recent = nil
		st.Trend = &trimmed
	}
	return st
}

// listEvent is one list the state frame leaves out, under the event that sends it.
type listEvent struct {
	name  string
	value any
}

// listEvents names the lists that travel as their own events, each with the value
// the state frame would have carried. A list whose reader has not reported is
// left out.
func (h *Hub) listEvents() []listEvent {
	var out []listEvent
	if t := h.state.Trend; t != nil {
		out = append(out, listEvent{evTrendRecent, t.Recent})
	}
	if q := h.state.Queue; q != nil {
		out = append(out, listEvent{evQueueReady, q.Ready}, listEvent{evQueueBlocked, q.Blocked})
	}
	return out
}

// listFramesLocked returns the list events to send: every list when all is set,
// otherwise only the lists whose bytes differ from what was last sent. What it
// returns is recorded as sent, so the next call compares against it.
func (h *Hub) listFramesLocked(all bool) [][]byte {
	var out [][]byte
	for _, ev := range h.listEvents() {
		b, err := json.Marshal(ev.value)
		if err != nil {
			b = []byte(`null`)
		}
		if !all && bytes.Equal(b, h.listSent[ev.name]) {
			continue
		}
		h.listSent[ev.name] = b
		out = append(out, rawFrame(ev.name, b))
	}
	return out
}

// readsLocked stamps each source's staleness against now, so a pane whose
// poller has stopped reporting reads as stale without waiting for the next
// publish. The map is copied, so a page serializing its snapshot cannot race a
// poller writing the hub's own.
func (h *Hub) readsLocked(now time.Time) map[string]ReadStatus {
	if len(h.state.Reads) == 0 {
		return nil
	}
	out := make(map[string]ReadStatus, len(h.state.Reads))
	for source, r := range h.state.Reads {
		r.Stale = r.StaleNow(now)
		out[source] = r
	}
	return out
}

// readOKLocked records a successful read of source: the value in the state is
// current as of now.
func (h *Hub) readOKLocked(source string, every time.Duration) {
	h.setReadLocked(source, ReadStatus{At: h.cfg.Now(), Every: every.Seconds()})
}

// readFailedLocked records that source's newest read failed. The last good
// value stays in the state and At stays at the last successful read, so the
// page ages the value it is showing instead of reading the failure as a
// present-but-empty reading (gt-q6h8e).
func (h *Hub) readFailedLocked(source string, every time.Duration) {
	r := h.state.Reads[source]
	r.Error = true
	r.Every = every.Seconds()
	h.setReadLocked(source, r)
}

func (h *Hub) setReadLocked(source string, r ReadStatus) {
	if h.state.Reads == nil {
		h.state.Reads = map[string]ReadStatus{}
	}
	h.state.Reads[source] = r
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
	// The Attention list is rebuilt here rather than where each of its readers
	// lands: a queue item's age is the published instant against the one it was
	// raised at, so a list built once and published again would show a wait that
	// stopped growing (gt-bi6w0).
	h.attentionLocked()
	// The lists go out first, so the page holds them when the state that draws
	// them lands, and each goes out only when it changed (gt-6ecd0).
	for _, f := range h.listFramesLocked(false) {
		h.broadcastLocked(f)
	}
	h.broadcastLocked(frame("state", h.stateFrameLocked()))
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
		h.addAlertLocked(al)
	}
	h.prev = next
	return raised
}

// addAlertLocked keeps al among the recent alerts and tells the open pages.
func (h *Hub) addAlertLocked(al Alert) {
	h.state.Alerts = append(h.state.Alerts, al)
	if n := len(h.state.Alerts); n > alertsKept {
		h.state.Alerts = append([]Alert(nil), h.state.Alerts[n-alertsKept:]...)
	}
	h.broadcastLocked(frame("alert", al))
}

// pollAttention feeds the attention queue's wake events to the alerts, so the
// Attention panel lists what the overseer's wake stream holds and the page
// hears each new one. An alert that the events clear, or that the Alerter's own
// alert for the same condition gives way to, leaves the recent list.
func (h *Hub) pollAttention() {
	evs, acks := h.cfg.Attention()
	var queue []attention.Item
	var queueFailed bool
	if h.cfg.AttentionItems != nil {
		items, err := h.cfg.AttentionItems()
		queue, queueFailed = items, err != nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queueItems, h.queueFailed = queue, queueFailed
	raised, removed := h.alerts.ObserveAttention(evs, acks)
	if len(removed) > 0 {
		gone := make(map[string]bool, len(removed))
		for _, k := range removed {
			gone[k] = true
		}
		kept := h.state.Alerts[:0:0]
		for _, al := range h.state.Alerts {
			if !gone[al.Key] {
				kept = append(kept, al)
			}
		}
		h.state.Alerts = kept
	}
	for _, al := range raised {
		h.addAlertLocked(al)
	}
	if len(removed) > 0 || len(raised) > 0 {
		h.publishLocked()
	}
}

func frame(event string, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`null`)
	}
	return rawFrame(event, b)
}

// rawFrame frames data, which is already JSON, as one event.
func rawFrame(event string, data []byte) []byte {
	return append(append([]byte("event: "+event+"\ndata: "), data...), '\n', '\n')
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
	if err == nil && h.cfg.LoadSample != nil {
		h.cfg.LoadSample(m.At, m.Load1)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		// A dropped sample keeps the last good one: the page ages and greys it
		// rather than reading the failure as a calm machine (gt-q6h8e).
		h.readFailedLocked(SourceMachine, h.cfg.MachineEvery)
		h.publishLocked()
		return
	}
	h.state.Machine = m
	h.state.Loads = append(h.state.Loads, LoadPoint{At: m.At, Load: m.Load1})
	if n := len(h.state.Loads); n > loadHistory {
		h.state.Loads = append([]LoadPoint(nil), h.state.Loads[n-loadHistory:]...)
	}
	h.readOKLocked(SourceMachine, h.cfg.MachineEvery)
	h.publishLocked()
}

// cloudSlowEvery is how often the hub looks again at a machine with no cloud
// patrol. The read itself is two stats, so the cost is not the point: the point
// is that a page open on a machine the patrol does not run on is not told every
// minute that nothing has changed, and the directory appearing is the only news
// there could be.
const cloudSlowEvery = 10 * time.Minute

func (h *Hub) pollCloud() {
	h.mu.Lock()
	now := h.cfg.Now()
	skip := h.cloudAt != nil && h.cloudMissing && now.Sub(*h.cloudAt) < cloudSlowEvery
	h.mu.Unlock()
	if skip {
		return
	}
	c := h.cfg.Cloud()
	if c == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Cloud = c
	h.cloudAt = &now
	// The app power line rides this poll, so a machine with apps to show is
	// read at the page's rate even with no patrol: a stop or start must not sit
	// stale for the slow interval.
	h.cloudMissing = c.NoPatrol && c.Apps == nil
	h.publishLocked()
}

// pollMango holds the town-metrics panel the reader read. The reader names every
// state it cannot read, so the panel is replaced on each poll, missing and error
// included. A reader that hands back nothing named no state at all, so the hub
// names it here: keeping the previous panel would pass an old analysis off as
// the one just read (gt-1wz1k).
func (h *Hub) pollMango() {
	m := h.cfg.Mango()
	if m == nil {
		m = &Mango{At: h.cfg.Now(), State: MangoError, Note: mangoNoPanelRead, Targets: []MangoTarget{}}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Mango = m
	h.publishLocked()
}

// mangoAt stamps the panel with the freshness its generation time earns at now.
// Freshness is a fact about the clock rather than the poll, so a page open across
// the stale mark sees the panel turn old without waiting for the next read. A
// panel the reader could not use keeps the state its read named: there is no
// generation time to judge (gt-1wz1k).
func mangoAt(m *Mango, now time.Time) *Mango {
	if m == nil || m.GeneratedAt == nil || (m.State != MangoFresh && m.State != MangoStale) {
		return m
	}
	out := *m
	out.State, out.Note = mangoFreshness(*m.GeneratedAt, now)
	return &out
}

func (h *Hub) pollOM() {
	om := h.cfg.OM()
	h.mu.Lock()
	defer h.mu.Unlock()
	if om == nil {
		h.readFailedLocked(SourceOM, h.cfg.OMEvery)
		h.publishLocked()
		return
	}
	h.state.OM = om
	h.readOKLocked(SourceOM, h.cfg.OMEvery)
	h.publishLocked()
}

// pollModels fills the header strip. The models come from the reader; the seat
// counts come from the dispatcher's roster, the occupancy a sling is refused
// against (gt-ly90n). The polecat list cannot count them: it carries polecats
// queued, spawning or already released, which hold no working seat.
func (h *Hub) pollModels() {
	m := h.cfg.Models()
	if m == nil {
		return
	}
	// The roster is a bd-backed read, so it runs outside the lock and does not
	// hold up a publish.
	var roster SeatRoster
	if h.cfg.SeatRoster != nil {
		roster = h.cfg.SeatRoster()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg.SeatRoster != nil {
		if roster.Read {
			m.Seats = &ModelsSeats{Live: roster.Total, Cap: m.SeatCap}
			for i := range m.SeatRows {
				if m.SeatRows[i].Closed || m.SeatRows[i].Unknown {
					continue
				}
				n := roster.Live[m.SeatRows[i].Agent]
				m.SeatRows[i].Live = &n
			}
		} else {
			m.SeatsUnreadable = true
		}
	}
	h.state.Models = m
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

func (h *Hub) pollForgejo() {
	f := h.cfg.Forgejo()
	if f == nil {
		return
	}
	// The hub owns the poll cadence, so it is what tells the page when a
	// snapshot has gone longer than two intervals without a refresh (gt-faml5).
	f.EverySec = h.cfg.ForgejoEvery.Seconds()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Forgejo = f
	h.publishLocked()
}

// pollNews reads the top Hacker News stories for the News pane. It publishes
// without observing: the pane carries its own failures, since Hacker News being
// slow or down is not a town condition the health pill or an alert is for.
func (h *Hub) pollNews() {
	n := h.cfg.News()
	if n == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.News = n
	h.publishLocked()
}

// pollDeploys reads the deploy workflows' runs. It is a poller of its own
// rather than part of the Forgejo feed's refresh so the block keeps its own
// 60s clock: a stage change and a run picking up a runner are the two things
// on this page an operator watches in minutes, not three.
func (h *Hub) pollDeploys() {
	d := h.cfg.Deploys()
	if d == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Deploys = d
	h.publishLocked()
}

// pollReports reads the overseer's latest report. It is a poller of its own
// rather than part of the feed's: the report is written on the hour, so its
// clock is the hour, not the three minutes the feed keeps.
func (h *Hub) pollReports() {
	rep := h.cfg.Reports()
	if rep == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Reports = rep
	h.publishLocked()
}

// pollQuestions reads the overseer's open questions. It runs bd per open
// question, so it keeps the minute clock rather than the feed's three.
func (h *Hub) pollQuestions() {
	q := h.cfg.Questions()
	if q == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Questions = q
	h.publishLocked()
}

// pollOverseer reads the overseer's heartbeat and journal.
func (h *Hub) pollOverseer() {
	o := h.cfg.Overseer()
	if o == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Overseer = o
	h.publishLocked()
}

// pollDeepReview reads the scheduled deep review's standing and last runs. It
// runs bd per listed run, so it keeps the minute clock rather than the feed's
// three.
func (h *Hub) pollDeepReview() {
	d := h.cfg.DeepReview()
	if d == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.DeepReview = d
	h.publishLocked()
}

// pollUsage reads the town's spend over the last day from the usage scan's
// records. It only reads files, so it keeps the minute clock the other slow
// readers use rather than the feed's three seconds.
func (h *Hub) pollUsage() {
	u := h.cfg.Usage()
	if u == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Usage = u
	h.publishLocked()
}

// pollClaudePlan reads the Claude plan meter, and the samples the Cost charts'
// Claude series is folded from, in one poll: the payload carries the series its
// own read returned, so the file is opened once and the charts cannot be drawn
// from a read that did not happen. The status line appends one at most every ten
// minutes, so it keeps the minute clock the other slow readers use.
func (h *Hub) pollClaudePlan() {
	c := h.cfg.ClaudePlan()
	if c == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.ClaudePlan = c
	h.state.PlanSamples = c.Samples
	h.rebuildCostChartsLocked()
	h.publishLocked()
}

// pollRework reads the om rework table from the landing worker's review log. It
// reads one small file, so it keeps the minute clock the other slow readers use.
func (h *Hub) pollRework() {
	r := h.cfg.Rework()
	if r == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Rework = r
	h.publishLocked()
}

func (h *Hub) pollEscalation() {
	e := h.cfg.Escalation()
	if e == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Escalations = e
	h.publishLocked()
}

// attentionLocked rebuilds the Attention panel from the three readers that feed
// it, so its one list is never a picture of a reader the hub has already moved
// past. publishLocked calls it, so every published list is drawn from the
// readers as they stand at that instant.
func (h *Hub) attentionLocked() {
	scores, err := h.needsHuman()
	risks, riskErr := h.landingRisk()
	h.state.Attention = buildAttention(h.state.Escalations, h.state.Questions, h.alerts.Standing(),
		AttentionQueue{Items: h.queueItems, Failed: h.queueFailed}, ScoresRead{Scores: scores, Err: err},
		LandingRiskRead{Risks: risks, Err: riskErr}, h.cfg.Now())
}

// needsHuman reads Jev's needs_human answers, with no reader leaving every item
// unscored: the panel then shows the queue by its alert levels as before. An
// error says the answers log exists but could not be read, which the panel
// names among the failed reads rather than drawing every row as unanswered.
func (h *Hub) needsHuman() (attention.Scores, error) {
	if h.cfg.NeedsHuman == nil {
		return nil, nil
	}
	return h.cfg.NeedsHuman()
}

// landingRisk reads the landing battery's risk answers, with no reader leaving
// every risk-path row unranked: the panel then keeps the queue's own order. An
// error says the landing log exists but could not be read, which the panel
// names rather than drawing the last answers as this read's.
func (h *Hub) landingRisk() (attention.LandingRisks, error) {
	if h.cfg.LandingRisk == nil {
		return nil, nil
	}
	return h.cfg.LandingRisk()
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
	h.mu.Lock()
	defer h.mu.Unlock()
	if q == nil {
		h.readFailedLocked(SourceQueue, h.cfg.QueueEvery)
		h.publishLocked()
		return
	}
	h.state.Queue = q
	h.state.Rigs = RigRows(q, h.polecats, h.cfg.RigTheme)
	h.readOKLocked(SourceQueue, h.cfg.QueueEvery)
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
	var read SpendRead
	if h.cfg.SpendHours != nil {
		read = h.cfg.SpendHours()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg.SpendHours != nil {
		h.state.SpendHours = read.Hours
		h.state.SpendSkipped = read.Skipped
	}
	// The hours move on every poll, so the charts are rebuilt on every poll,
	// whether or not the spend report answered: a failed report keeps its last
	// good figures, and the hours are whatever the ledger read gave, or none.
	if len(sp) == 0 {
		h.rebuildCostChartsLocked()
		h.readFailedLocked(SourceSpend, h.cfg.SpendEvery)
		h.publishLocked()
		return
	}
	h.state.Spend = sp
	h.rebuildCostChartsLocked()
	h.readOKLocked(SourceSpend, h.cfg.SpendEvery)
	h.publishLocked()
}

// rebuildCostChartsLocked rebuilds the Cost panel's charts from the spend
// report, the day's hours, the Claude plan and the samples its dollars come
// from. The caller holds h.mu.
func (h *Hub) rebuildCostChartsLocked() {
	charts := BuildCostCharts(h.cfg.Now(), SpendPerDay(h.state.Spend), h.state.SpendHours, h.state.SpendSkipped, h.state.ClaudePlan, h.state.PlanSamples)
	h.state.CostCharts = &charts
	h.costRows = nil
}

// costProviderRowsLocked builds the panel's rows from the state the charts are
// built from: the default list through BuildCostProviders, or the list a test
// handed the hub so it can add a provider without touching the town's. A
// provider whose reader this hub was not given has no row. The rows are kept
// between snapshots: they are rebuilt when a cost input has changed (costRows is
// cleared) or when the clock has moved what they say about it (costRowsKeyOf),
// since the Jev row's age and the Claude row's unread reason both read now. The
// caller holds h.mu.
func (h *Hub) costProviderRowsLocked(now time.Time, reads map[string]ReadStatus) []CostProvider {
	jev := jevSpendForRows(h.state.JevSpend, reads[SourceJevSpend])
	if key := costRowsKeyOf(jev, h.state.ClaudePlan, now); h.costRows == nil || key != h.costKey {
		h.costRows = h.buildCostRowsLocked(now, jev)
		h.costKey = key
	}
	// The caller gets its own slice, so editing its snapshot cannot reach the
	// kept rows. make, not append to nil, so an empty list still encodes as [].
	rows := make([]CostProvider, len(h.costRows))
	copy(rows, h.costRows)
	return rows
}

// buildCostRowsLocked builds the panel's rows from the state the charts are
// built from, with jev as the Jev row's value. The caller holds h.mu.
func (h *Hub) buildCostRowsLocked(now time.Time, jev *JevSpend) []CostProvider {
	unwired := map[string]bool{
		costProviderDeepSeek: h.cfg.Spend == nil,
		costProviderClaude:   h.cfg.ClaudePlan == nil,
		costProviderJev:      h.cfg.JevSpend == nil,
	}
	var built []CostProvider
	if h.costProviders == nil {
		built = BuildCostProviders(now, h.state.Spend, h.state.ClaudePlan, h.state.CostCharts, jev)
	} else {
		built = buildCostProviders(h.costProviders, now, h.state.Spend, h.state.ClaudePlan, h.state.CostCharts, jev)
	}
	// A town with no provider reader still sends an empty list, so the page can
	// tell none from a reading that did not arrive.
	rows := make([]CostProvider, 0, len(built))
	for _, p := range built {
		if !unwired[p.ID] {
			rows = append(rows, p)
		}
	}
	return rows
}

// jevRowMark is what the Jev row says about its read: whether the read failed
// and whether the figures are the last good ones. The read's age moves it with
// the clock, so it is part of the key the rows are kept under.
type jevRowMark struct {
	failed, stale bool
}

// jevRowMarkOf is the mark a Jev value carries into the rows: none when the
// value is absent or current.
func jevRowMarkOf(jev *JevSpend) jevRowMark {
	if jev == nil {
		return jevRowMark{}
	}
	return jevRowMark{failed: jev.ReadFailed, stale: jev.Stale}
}

// costRowsKey is what the Cost rows read from the clock, besides their cost
// inputs: the Jev row's mark, and the Claude row's reason while its plan is
// unread (the age of the newest sample, and whether its week has reset). No
// other part of the rows moves with now.
type costRowsKey struct {
	jev    jevRowMark
	claude string
}

// costRowsKeyOf is the key the rows are built under at now. The Claude reason is
// taken only while the plan is unread, the same test the Claude row makes before
// it reads the clock, so a fresh plan's rows are not rebuilt as time passes.
func costRowsKeyOf(jev *JevSpend, plan *ClaudePlan, now time.Time) costRowsKey {
	key := costRowsKey{jev: jevRowMarkOf(jev)}
	if plan != nil && (plan.Unavailable || plan.Stale) {
		key.claude = claudePlanUnreadWhy(plan, now)
	}
	return key
}

// jevSpendForRows is the Jev value the Cost rows are built from, marked when
// the stamped read is not current so the row says why instead of drawing the
// last good figures as today's. The state's own value stays as the last good
// read wrote it: the mark is the row's, not the ledger's.
func jevSpendForRows(jev *JevSpend, r ReadStatus) *JevSpend {
	// A failed read is marked whether or not its stamp has aged yet: the mark
	// rests on the read's own error, not only on the stale flag.
	if jev == nil || !(r.Error || r.Stale) {
		return jev
	}
	marked := *jev
	marked.ReadFailed = r.Error
	marked.Stale = true
	return &marked
}

// pollJevSpend keeps the last good Jev row across a failed read, like the
// DeepSeek spend beside it. The two reads fail apart: neither touches the
// other's value or status.
func (h *Hub) pollJevSpend() {
	j := h.cfg.JevSpend()
	h.mu.Lock()
	defer h.mu.Unlock()
	if j == nil {
		h.readFailedLocked(SourceJevSpend, h.cfg.JevSpendEvery)
		h.publishLocked()
		return
	}
	h.state.JevSpend = j
	h.costRows = nil
	h.readOKLocked(SourceJevSpend, h.cfg.JevSpendEvery)
	h.publishLocked()
}

// pollJevScorecard keeps the last good scorecard lines across a failed read.
// A read that lost only some batteries keeps each lost battery's last good
// line, marked Failed and Kept, and leaves the others as the read drew them.
// It fails apart from the spend row: neither touches the other's value or
// status.
func (h *Hub) pollJevScorecard() {
	sc := h.cfg.JevScorecard()
	h.mu.Lock()
	defer h.mu.Unlock()
	if sc == nil {
		h.readFailedLocked(SourceJevScorecard, h.cfg.JevScorecardEvery)
		h.publishLocked()
		return
	}
	keepLastGoodJevBatteries(h.state.JevScorecard, sc, h.jevGood, h.cfg.Now())
	h.state.JevScorecard = sc
	h.readOKLocked(SourceJevScorecard, h.cfg.JevScorecardEvery)
	h.publishLocked()
}

// jevKeptMaxAge is how old a kept battery line may be, counted from the last
// good read of its battery, before the page drops it to the name alone. An
// hour is four default polls, so a verdict nobody has re-read that long is no
// longer the battery's current one.
const jevKeptMaxAge = time.Hour

// keepLastGoodJevBatteries puts prev's line back for every battery that sc
// reports failed, so one battery's failed read does not blank its line, unless
// that line is older than jevKeptMaxAge at now. A battery prev never read stays
// a placeholder. goodAt records each battery's last good read and is updated
// from sc. sc is retallied.
func keepLastGoodJevBatteries(prev, sc *JevScorecard, goodAt map[string]time.Time, now time.Time) {
	last := map[string]JevBattery{}
	if prev != nil {
		for _, b := range prev.Batteries {
			if !b.Failed || b.Kept {
				last[b.Name] = b
			}
		}
	}
	for i, b := range sc.Batteries {
		if !b.Failed {
			goodAt[b.Name] = now
			continue
		}
		old, ok := last[b.Name]
		if !ok || now.Sub(goodAt[b.Name]) > jevKeptMaxAge {
			continue
		}
		old.Failed, old.Kept = true, true
		sc.Batteries[i] = old
	}
	sc.Tally()
}

// pollSummary keeps the last good seats and ready-to-land readings across a
// failed one. Letting either empty out would read a dropped read as a town with
// no polecats and a drained queue, and would also make the alert rules act on a
// change that never happened: every stalled polecat announced again on
// recovery, and the stuck-queue rule re-armed (gt-q6h8e).
func (h *Hub) pollSummary() {
	s := h.cfg.Summary()
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.state.Summary
	if s.SeatsError {
		h.readFailedLocked(SourceSeats, h.cfg.SummaryEvery)
		if prev != nil {
			s.SeatsCap = prev.SeatsCap
		}
	} else {
		h.polecats = s.Polecats
		h.state.SummaryAt = h.cfg.Now()
		h.readOKLocked(SourceSeats, h.cfg.SummaryEvery)
	}
	if s.ReadyError {
		h.readFailedLocked(SourceReady, h.cfg.SummaryEvery)
		if prev != nil {
			s.ReadyToLand, s.OldestReady = prev.ReadyToLand, prev.OldestReady
		}
	} else {
		h.readOKLocked(SourceReady, h.cfg.SummaryEvery)
	}
	h.state.Summary = &s
	// The seats are half of the Rigs panel: a fresh seat reading re-joins it.
	h.state.Rigs = RigRows(h.state.Queue, h.polecats, h.cfg.RigTheme)
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
		// The seat's dispatch time is the bead's own clock, read from the
		// town's dispatch record, and it is the same clock the overrun guard
		// judges — see Polecat.Hooked. The feed's dispatch lines are not used
		// for it: the feed is a rolling window with no record of the bead's
		// done events, so its earliest line can belong to an earlier run and
		// would date the Age from a start the guard never uses (gt-tzbtf).
		// Every dispatch writes that record, the scheduled slings included: a
		// scheduled dispatch runs the same sling, which logs the sling event
		// beside its scheduler_dispatch line.
		at := p.Hooked
		if at != nil {
			when := *at
			p.Slung = &when
			if d := now.Sub(when); d > 0 {
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
	// A bead is gated from the worker's "merged ... gating" line until it lands,
	// is rejected, or reports its stage times.
	gateStartRe = regexp.MustCompile(`\[land\] (\S+): merged .*gating`)
	gateEndRe   = regexp.MustCompile(`\[land\] (\S+): (?:stages: |landed |rejected )`)
)

// feedBurstMax is the most entries pollFeed sends as frames of their own; it
// leaves room in a page's 64-frame buffer for the state and alert frames that
// follow.
const feedBurstMax = 32

func (h *Hub) pollFeed() {
	entries := h.cfg.Feed()
	if len(entries) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := false
	// A batch longer than a page's buffer (the 2h preface of the first poll on a
	// busy town) goes out as one backlog frame: one entry frame each would fill
	// the buffer and drop the page. The frame carries the whole ring, not just
	// this batch, which is safe because the page's backlog handler replaces its
	// feed rather than appending to it (TestBacklogHandlerReplacesTheFeed).
	burst := len(entries) > feedBurstMax
	for i := range entries {
		h.seq++
		entries[i].Seq = h.seq
		e := entries[i]
		h.ring = append(h.ring, e)
		if m := gateStartRe.FindStringSubmatch(e.Text); m != nil {
			h.gating[m[1]] = true
			changed = true
		} else if m := gateEndRe.FindStringSubmatch(e.Text); m != nil && h.gating[m[1]] {
			delete(h.gating, m[1])
			changed = true
		}
		if !burst {
			h.broadcastLocked(frame("entry", e))
		}
	}
	if n := len(h.ring); n > h.cfg.RingSize {
		h.ring = append([]Entry(nil), h.ring[n-h.cfg.RingSize:]...)
	}
	if burst {
		h.broadcastLocked(frame("backlog", h.ring))
	}
	if alerts := h.observeLocked(entries); alerts > 0 {
		changed = true
	}
	if changed {
		h.publishLocked()
	}
}
