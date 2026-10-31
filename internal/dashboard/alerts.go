package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/events"
)

// Alert levels, in ascending severity.
const (
	LevelInfo = "info"
	LevelWarn = "warn"
	LevelCrit = "crit"
)

// Alert is one thing the operator should look at. The page lists it in the
// EVENTS feed and, while the operator has alerts switched on, raises a browser
// notification for it. State.Alerts keeps the recent ones.
type Alert struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Level string    `json:"level"` // info, warn, crit
	Key   string    `json:"key"`
	Title string    `json:"title"`
	Text  string    `json:"text"`
	// Bead is the bead the alert is about, where the alert names one: a rejected
	// landing and a polecat holding work. The EVENTS feed shows it as its own
	// column; an alert about no bead leaves it empty.
	Bead string `json:"bead,omitempty"`
	// Rule is the attention rule an alert raised from the attention queue
	// carries, which is what the Attention panel tags and orders the row by. The
	// Alerter's own rules leave it empty: they are the EVENTS feed's lines, and the
	// conditions behind them are either a queue item already or a feed line.
	Rule string `json:"rule,omitempty"`
}

const (
	// alertsKept is how many alerts the hub keeps in State.Alerts.
	alertsKept = 20
	// alertCooldown is the floor between two alerts that share a key. A rule
	// whose condition is still standing re-arms below it only when the rule
	// says so.
	alertCooldown = 10 * time.Minute
	// healthUnknownAfter is how long an UNKNOWN verdict must persist before it
	// is worth mentioning. The daemon reports UNKNOWN for one tick while it
	// restarts, and that must never alert.
	healthUnknownAfter = 5 * time.Minute
	// healthConfirmPolls is how many consecutive health polls must read the
	// same non-green verdict before the town is called red or degraded.
	healthConfirmPolls = 2
	// stuckQueueAfter is how long a bead may wait to land before it alerts.
	stuckQueueAfter = 30 * time.Minute
	// dispatcherSilentAfter is how long the spec dispatcher may go without a
	// tick before it alerts; it normally ticks every minute.
	dispatcherSilentAfter = 5 * time.Minute
)

// rejectionRe matches the landing worker's rejection lines, which name why a
// landing was sent back: a failed review, gate, conflict, policy or empty diff.
var rejectionRe = regexp.MustCompile(`rejected \((review|gate|conflict|policy|empty)\)`)

// rejectionBeadRe reads the bead off the same lines. The landing worker logs a
// rejection twice, as "[land] gt-x: rejected (review)" and "gastown: gt-x:
// landing rejected (review)", and both name the bead.
var rejectionBeadRe = regexp.MustCompile(`(\S+): (?:landing )?rejected \((?:review|gate|conflict|policy|empty)\)`)

// The severities of the health alerts, each outranking the one before it.
// healthNone is no alert standing.
const (
	healthNone = iota
	healthUnknownSev
	healthDegradedSev
	healthRedSev
)

// The keys of the alerts the Alerter raises more than once or that the
// attention table below names.
const (
	keyHealthRed       = "health:red"
	keyHealthDegraded  = "health:degraded"
	keyEscalations     = "escalations"
	keyQueueStuck      = "queue:stuck"
	polecatAlertPrefix = "polecat:"
)

// rejectionFreshWithin is how old a rejection line may be and still alert. The
// feed delivers its backlog (the last couple of hours) to the first page that
// connects, and a rejection from before the page opened is history, not news.
const rejectionFreshWithin = 10 * time.Minute

// Alerter decides, on the server, when the town needs attention. It is a state
// machine over the snapshot sequence rather than a pure function of one
// snapshot: debounce needs the run length, cooldown needs the last time a key
// fired, and a transition needs the state before it. Every decision is taken
// from the arguments and the injected clock, so a test can drive it a poll at a
// time.
type Alerter struct {
	now func() time.Time

	// instance names this alerter in the id of every alert it raises, so the id
	// stays unique across a hub restart (see newAlertInstance).
	instance string
	// seq numbers the alerts this alerter raises.
	seq int64

	// health is the health rule's run length: a new reading counts once, and
	// only a fresh reading (a new ReadAt) counts as a poll.
	lastHealthAt time.Time
	lastVerdict  string
	healthStreak int
	// healthLive is the severity of the health alert that stands for the
	// current non-green run, healthNone when none does. A verdict raises an
	// alert only above it, so a run keeps one alert per severity step; a
	// return to green while one stands raises the recovery.
	healthLive   int
	unknownSince time.Time

	// polecatsSeen is false until the first snapshot has been taken as a
	// baseline, so a state that already existed is not announced as a change.
	polecatsSeen bool

	// escalations is the last count seen, and escalationsSet is false until one
	// has been taken as a baseline.
	escalations    int
	escalationsSet bool

	// rejectSeen is the feed entries already reported, deduplicated by Seq.
	rejectSeen map[int64]bool

	// stuckFired is set while an alert stands for the current stuck queue; the
	// rule clears it when the queue drains.
	stuckFired bool

	// dispatchAt is the newest tick seen and dispatchFired stands while an
	// alert stands for the current silence; a new tick re-arms it.
	dispatchAt    time.Time
	dispatchFired bool

	// cooldown is the last time each key fired.
	cooldown map[string]time.Time

	// standing is the attention wake alerts that stand now, by event key.
	standing map[string]standingAttention
}

// standingAttention is one attention alert that stands, with the keys of the
// Alerter's own alerts it covers (see attentionCovered).
type standingAttention struct {
	alert  Alert
	covers []string
}

// NewAlerter returns an alerter reading the time from now.
func NewAlerter(now func() time.Time) *Alerter {
	if now == nil {
		now = time.Now
	}
	return &Alerter{
		now:        now,
		instance:   newAlertInstance(),
		rejectSeen: map[int64]bool{},
		cooldown:   map[string]time.Time{},
		standing:   map[string]standingAttention{},
	}
}

// newAlertInstance names one hub in the id of the alerts it raises. The page
// keeps each EVENTS line once, keyed by the alert's id, and the numbering starts
// over with every hub process: without the instance the page would take the first
// alerts after a restart for ones it already shows and drop them (gt-wpgga).
func newAlertInstance() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// alertID names the next alert this alerter raises.
func (a *Alerter) alertID() string {
	a.seq++
	return fmt.Sprintf("alert-%s-%d", a.instance, a.seq)
}

// Baseline forgets the run lengths and baselines of the rules. The hub calls it
// when the first page connects after a gap in polling, so the first snapshot
// after a page connects is a baseline: a polecat that was already stalled, or
// an escalation count, is not announced as a change the page just missed.
// Cooldowns survive, so a re-connect does not repeat an alert already raised.
func (a *Alerter) Baseline() {
	a.polecatsSeen = false
	a.escalationsSet = false
	a.healthStreak = 0
	a.unknownSince = time.Time{}
	a.rejectSeen = map[int64]bool{}
}

// Observe takes one snapshot of the town against the one before it and returns
// the alerts to show now, in the order the rules are written. prev is the
// previous snapshot (a zero State on the first call), next is the snapshot just
// taken, and entries are the feed lines that arrived with it (empty for a poll
// that read no feed).
func (a *Alerter) Observe(prev, next State, entries []Entry) []Alert {
	now := a.now()
	var out []Alert

	if al, ok := a.healthAlert(next, now); ok {
		out = append(out, al)
	}
	out = append(out, a.polecatAlerts(prev, next, now)...)
	if al, ok := a.escalationAlert(next, now); ok {
		out = append(out, al)
	}
	out = append(out, a.rejectionAlerts(entries, now)...)
	if al, ok := a.stuckQueueAlert(next, now); ok {
		out = append(out, al)
	}
	if al, ok := a.dispatchAlert(next, now); ok {
		out = append(out, al)
	}
	return out
}

// fire returns one alert for key, unless that key is still inside its cooldown
// or an attention alert that covers it stands, and records the firing. A
// suppressed alert is not recorded, so a rule whose condition is still standing
// may try again once the cooldown expires or the attention alert clears.
func (a *Alerter) fire(now time.Time, level, key, title, text string) (Alert, bool) {
	if last, ok := a.cooldown[key]; ok && now.Sub(last) < alertCooldown {
		return Alert{}, false
	}
	if a.coveredByAttention(key) {
		return Alert{}, false
	}
	a.cooldown[key] = now
	return Alert{ID: a.alertID(), At: now, Level: level, Key: key, Title: title, Text: text}, true
}

// healthAlert reads the health rule off next.Health. A reading is fresh when
// its ReadAt or verdict differs from the last one, which is what stops a feed
// tick that merely repeated the health in State from counting as a poll.
func (a *Alerter) healthAlert(next State, now time.Time) (Alert, bool) {
	h := next.Health
	if h.ReadAt.Equal(a.lastHealthAt) && h.Verdict == a.lastVerdict {
		return Alert{}, false
	}
	prev := a.lastVerdict
	a.lastHealthAt, a.lastVerdict = h.ReadAt, h.Verdict

	switch h.Verdict {
	case "green":
		a.healthStreak = 0
		a.unknownSince = time.Time{}
		live := a.healthLive > healthNone
		a.healthLive = healthNone
		if live {
			return a.fire(now, LevelInfo, "health:recovered", "town health recovered", h.Line)
		}
	case "unknown":
		a.healthStreak = 0
		if a.unknownSince.IsZero() {
			a.unknownSince = now
		}
		if a.healthLive >= healthUnknownSev || now.Sub(a.unknownSince) < healthUnknownAfter {
			return Alert{}, false
		}
		al, ok := a.fire(now, LevelWarn, "health:unknown", "town health has been UNKNOWN for five minutes", h.Line)
		if ok {
			a.healthLive = healthUnknownSev
		}
		return al, ok
	case "red", "degraded":
		a.unknownSince = time.Time{}
		if h.Verdict != prev {
			// The streak counts polls of one verdict, so red after degraded
			// confirms on its own polls.
			a.healthStreak = 0
		}
		a.healthStreak++
		sev, level, key, title := healthRedSev, LevelCrit, keyHealthRed, "town health is red"
		if h.Verdict == "degraded" {
			sev, level, key, title = healthDegradedSev, LevelWarn, keyHealthDegraded, "town health is degraded"
		}
		if a.healthLive >= sev || a.healthStreak < healthConfirmPolls {
			return Alert{}, false
		}
		al, ok := a.fire(now, level, key, title, h.Line)
		if ok {
			a.healthLive = sev
		}
		return al, ok
	}
	return Alert{}, false
}

// holdsForPerson reports a state that means the polecat's bead waits on a
// person: the old needs-human label and the two labels naming who must act
// (gt-slmaz).
func holdsForPerson(state string) bool {
	switch state {
	case StateNeedsHuman, StateNeedsOverseer, StateNeedsOperator:
		return true
	}
	return false
}

// polecatAlerts reports each polecat that has newly stalled or started waiting
// on a human, against the previous snapshot. The first snapshot is a baseline,
// so a polecat that was already in one of those states raises nothing.
func (a *Alerter) polecatAlerts(prev, next State, now time.Time) []Alert {
	list := append([]Polecat(nil), next.Polecats...)
	sort.Slice(list, func(i, j int) bool { return polecatKey(list[i]) < polecatKey(list[j]) })

	was := make(map[string]string, len(prev.Polecats))
	for _, p := range prev.Polecats {
		was[polecatKey(p)] = p.State
	}
	// The baseline is the first snapshot that has polecats in it. A snapshot
	// taken before the summary was read lists none, and baselining on it would
	// announce every polecat that was already stalled as a change. A failed
	// seats read is no change at all: the hub keeps the previous list, so the
	// snapshot here still carries it (gt-q6h8e).
	if len(next.Polecats) == 0 {
		return nil
	}
	baseline := a.polecatsSeen
	a.polecatsSeen = true
	if !baseline {
		return nil
	}

	var out []Alert
	for _, p := range list {
		if p.State != StateStalled && !holdsForPerson(p.State) {
			continue
		}
		if p.RigParked {
			continue // a parked rig is stood down on purpose; its polecats are not news
		}
		key := polecatKey(p)
		if old, ok := was[key]; ok && old == p.State {
			continue
		}
		text := "holds no bead"
		if p.Bead != "" {
			text = "holds " + p.Bead
			if p.Title != "" {
				text += " — " + p.Title
			}
		}
		title := key + " is " + p.State
		if al, ok := a.fire(now, LevelWarn, polecatAlertPrefix+key, title, text); ok {
			al.Bead = p.Bead
			out = append(out, al)
		}
	}
	return out
}

func polecatKey(p Polecat) string { return p.Rig + "/" + p.Name }

// escalationAlert reports a rising escalation count. The first count is a
// baseline, so a town that was already holding escalations when the page
// connected raises nothing until the count moves again.
func (a *Alerter) escalationAlert(next State, now time.Time) (Alert, bool) {
	if next.Summary == nil || next.Summary.Escalations == nil {
		return Alert{}, false
	}
	n := *next.Summary.Escalations
	was, known := a.escalations, a.escalationsSet
	a.escalations, a.escalationsSet = n, true
	if !known || n <= was {
		return Alert{}, false
	}
	return a.fire(now, LevelCrit, keyEscalations, "escalations rose",
		fmt.Sprintf("open escalations rose from %d to %d", was, n))
}

// rejectionAlerts reports each feed line that says a landing was rejected,
// once. Seq de-duplicates: the feed delivers a line once, but a re-read must
// not alert twice.
func (a *Alerter) rejectionAlerts(entries []Entry, now time.Time) []Alert {
	var out []Alert
	for _, e := range entries {
		m := rejectionRe.FindStringSubmatch(e.Text)
		if m == nil || a.rejectSeen[e.Seq] {
			continue
		}
		a.rejectSeen[e.Seq] = true
		if !e.At.IsZero() && now.Sub(e.At) > rejectionFreshWithin {
			continue // history from before the page opened
		}
		// One rejection is two log lines. Keying on the bead and the kind lets the
		// cooldown fold them into one alert; a line that names no bead keys on its
		// own number.
		key := fmt.Sprintf("reject:%d", e.Seq)
		bead := ""
		if b := rejectionBeadRe.FindStringSubmatch(e.Text); b != nil {
			bead = b[1]
			key = "reject:" + b[1] + ":" + m[1]
		}
		al, ok := a.fire(now, LevelWarn, key,
			"a landing was rejected ("+m[1]+")", e.Text)
		if ok {
			al.Bead = bead
			out = append(out, al)
		}
	}
	return out
}

// stuckQueueAlert reports a bead that has waited to land for over half an hour,
// once per jam: the flag clears when the queue drains below the threshold, so
// the next jam alerts again. A ready-to-land read that failed keeps the previous
// oldest bead through the hub, so a dropped read leaves the flag standing
// rather than re-arming the alert (gt-q6h8e).
func (a *Alerter) stuckQueueAlert(next State, now time.Time) (Alert, bool) {
	var oldest *time.Time
	if next.Summary != nil {
		oldest = next.Summary.OldestReady
	}
	if oldest == nil {
		a.stuckFired = false
		return Alert{}, false
	}
	age := now.Sub(*oldest)
	if age < stuckQueueAfter {
		a.stuckFired = false
		return Alert{}, false
	}
	if a.stuckFired {
		return Alert{}, false
	}
	al, ok := a.fire(now, LevelWarn, keyQueueStuck, "a bead has waited over 30 minutes to land",
		fmt.Sprintf("the oldest bead waiting to land was last touched %d minutes ago", int(age.Minutes())))
	if ok {
		a.stuckFired = true
	}
	return al, ok
}

// dispatchAlert reports a spec dispatcher that has stopped ticking, once per
// silence: a newer tick than the last one seen re-arms the rule, so a
// dispatcher that goes quiet again later alerts again.
func (a *Alerter) dispatchAlert(next State, now time.Time) (Alert, bool) {
	if next.Dispatch == nil {
		return Alert{}, false
	}
	if next.Dispatch.At.After(a.dispatchAt) {
		a.dispatchAt = next.Dispatch.At
		a.dispatchFired = false
	}
	age := now.Sub(next.Dispatch.At)
	if age <= dispatcherSilentAfter || a.dispatchFired {
		return Alert{}, false
	}
	al, ok := a.fire(now, LevelWarn, "dispatch:silent", "the spec dispatcher has not ticked",
		fmt.Sprintf("its last tick was %d minutes ago; it ticks every minute", int(age.Minutes())))
	if ok {
		a.dispatchFired = true
	}
	return al, ok
}

// attentionCovered is the rule-to-existing-alert table: the keys of the
// Alerter's own alerts that report the same condition as an attention item of
// rule, whose event key is key. When the attention alert stands, these are not
// shown, so the condition appears once and the attention alert is the one kept.
//
//	main.red             -> health:red, health:degraded (the town health reads main red)
//	escalation.open      -> escalations
//	polecat.stall        -> polecat:<rig>/<name> (event key stall:<rig>/<name>)
//	landing.queue_stuck  -> queue:stuck
//
// Health red or degraded also has other causes; while a red-main alert stands
// they are not told apart, and the health alert fires again if it is still
// red once the attention alert clears.
func attentionCovered(rule, key string) []string {
	switch rule {
	case attention.RuleRedMain:
		return []string{keyHealthRed, keyHealthDegraded}
	case attention.RuleEscalation:
		return []string{keyEscalations}
	case attention.RulePolecatStall:
		if seat, ok := strings.CutPrefix(key, "stall:"); ok {
			return []string{polecatAlertPrefix + seat}
		}
	case attention.RuleQueueStuck:
		return []string{keyQueueStuck}
	}
	return nil
}

// coveredByAttention reports whether a standing attention alert covers key.
func (a *Alerter) coveredByAttention(key string) bool {
	for _, s := range a.standing {
		for _, k := range s.covers {
			if k == key {
				return true
			}
		}
	}
	return false
}

// attentionLevel is the alert level of an attention severity. An event that
// wakes the overseer is never info.
func attentionLevel(s attention.Severity) string {
	if s == attention.SeverityHigh {
		return LevelCrit
	}
	return LevelWarn
}

// ObserveAttention takes the attention transitions that arrived since the last
// call, with the acks as they stand now, and returns the alerts to add and the
// keys of the alerts to remove.
//
// A new line with wake=true and a handled value other than info raises an
// alert keyed by the event's key, titled with its rule id and carrying its
// text. A cleared line for the key removes it. A key that is acked raises
// nothing, and an ack made while the alert stands removes it. A key that
// already stands is left as it is, so a repeat line (the daemon's "still
// holding") does not show it twice. Raising one also removes the Alerter's own
// alert for the same condition (attentionCovered).
//
// Attention alerts bypass the cooldown: the stream states the transitions, so a
// condition that clears and returns shows again.
func (a *Alerter) ObserveAttention(evs []attention.Event, acks attention.Acks) (raised []Alert, removed []string) {
	now := a.now()
	drop := func(key string) {
		for i := range raised {
			if raised[i].Key == key {
				raised = append(raised[:i], raised[i+1:]...)
				break
			}
		}
		removed = append(removed, key)
	}
	for _, e := range evs {
		switch e.State {
		case attention.EventCleared:
			if _, ok := a.standing[e.Key]; ok {
				delete(a.standing, e.Key)
				drop(e.Key)
			}
		case attention.EventNew:
			if !e.Wake || e.EffectiveHandled() == attention.HandledInfo {
				continue
			}
			if _, ok := a.standing[e.Key]; ok || attention.AckedKey(acks, e.Key) {
				continue
			}
			title := e.Rule
			if title == "" {
				title = string(e.Class)
			}
			al := Alert{ID: a.alertID(), At: e.TS, Level: attentionLevel(e.Severity), Key: e.Key, Title: title, Text: e.Text, Rule: e.Rule}
			if al.At.IsZero() {
				al.At = now
			}
			covers := attentionCovered(e.Rule, e.Key)
			a.standing[e.Key] = standingAttention{alert: al, covers: covers}
			raised = append(raised, al)
			removed = append(removed, covers...)
		}
	}
	// An ack made while an alert stands hides it.
	keys := make([]string, 0, len(a.standing))
	for k := range a.standing {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if attention.AckedKey(acks, k) {
			delete(a.standing, k)
			drop(k)
		}
	}
	return raised, removed
}

// Standing returns the attention items that stand now, sorted by key, for the
// Attention panel: a wake line the queue has not cleared and nobody has acked
// is a condition that still holds, whatever the recent-alerts list has since
// pushed to the top (gt-bi6w0).
func (a *Alerter) Standing() []Alert {
	keys := make([]string, 0, len(a.standing))
	for k := range a.standing {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Alert, 0, len(keys))
	for _, k := range keys {
		out = append(out, a.standing[k].alert)
	}
	return out
}

// AttentionFollower reads the attention queue's events.jsonl for the hub. The
// first read returns what the retained logs hold, so a condition that was
// raised before the page opened and has not cleared is on it; every later read
// returns the lines appended since, followed by path across the file's 1 MB
// rotation the way gt attention -f follows it.
type AttentionFollower struct {
	townRoot string
	tail     *events.Tail
}

// NewAttentionFollower follows the events.jsonl of the town at townRoot.
func NewAttentionFollower(townRoot string) *AttentionFollower {
	return &AttentionFollower{townRoot: townRoot}
}

// Items returns the queue's items as the daemon's state.json holds them: the
// class, owner and waiting-on each row of the panel carries (gt-tv5v2). A state
// that is missing is no items and no error, the same as the queue itself reads
// it; a state that is there and cannot be read returns the error, so the panel
// names the read among the ones behind the list that failed rather than passing
// a queue nobody could classify off as empty (gt-tv5v2).
func (f *AttentionFollower) Items() ([]attention.Item, error) {
	state, err := attention.ReadState(f.townRoot)
	if err != nil {
		return nil, err
	}
	return state.Items, nil
}

// Read returns the new transitions and the acks as they stand. A missing or
// unreadable events file yields no transitions and is tried again on the next
// read; unreadable acks read as none, so an alert is shown rather than hidden.
func (f *AttentionFollower) Read() ([]attention.Event, attention.Acks) {
	acks, _ := attention.ReadAcks(f.townRoot)
	if f.tail == nil {
		path := attention.EventsPath(f.townRoot)
		if _, err := os.Stat(path); err != nil {
			return nil, acks
		}
		// Open the tail before reading the history: a line appended in between
		// is then seen twice, which the Alerter takes in its stride, rather
		// than not at all.
		t, err := events.OpenTail(path)
		if err != nil {
			return nil, acks
		}
		f.tail = t
		history, _ := attention.ReadRetainedEvents(f.townRoot)
		return history, acks
	}
	lines, _ := f.tail.Poll()
	var out []attention.Event
	for _, line := range lines {
		if e, ok := attention.ParseEventLine(line); ok {
			out = append(out, e)
		}
	}
	return out, acks
}
