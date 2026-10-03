package dashboard

import (
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Alert levels, in ascending severity.
const (
	LevelInfo = "info"
	LevelWarn = "warn"
	LevelCrit = "crit"
)

// Alert is one thing the operator should look at. The page shows it only when
// the operator has switched alerts on, but State.Alerts keeps the recent ones
// for display either way.
type Alert struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Level string    `json:"level"` // info, warn, crit
	Key   string    `json:"key"`
	Title string    `json:"title"`
	Text  string    `json:"text"`
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
	// healthConfirmPolls is how many consecutive health polls must read
	// non-green before the town is called red or degraded.
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

// Alerter decides, on the server, when the town needs attention. It is a state
// machine over the snapshot sequence rather than a pure function of one
// snapshot: debounce needs the run length, cooldown needs the last time a key
// fired, and a transition needs the state before it. Every decision is taken
// from the arguments and the injected clock, so a test can drive it a poll at a
// time.
type Alerter struct {
	now func() time.Time

	// seq numbers the alerts this alerter raises.
	seq int64

	// health is the health rule's run length: a new reading counts once, and
	// only a fresh reading (a new ReadAt) counts as a poll.
	lastHealthAt time.Time
	lastVerdict  string
	healthStreak int
	// healthLive is true while an alert stands for the current non-green run.
	// A return to green while it is true raises the recovery.
	healthLive   bool
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
}

// NewAlerter returns an alerter reading the time from now.
func NewAlerter(now func() time.Time) *Alerter {
	if now == nil {
		now = time.Now
	}
	return &Alerter{
		now:        now,
		rejectSeen: map[int64]bool{},
		cooldown:   map[string]time.Time{},
	}
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

// fire returns one alert for key, unless that key is still inside its cooldown,
// and records the firing. A suppressed alert is not recorded, so a rule whose
// condition is still standing may try again once the cooldown expires.
func (a *Alerter) fire(now time.Time, level, key, title, text string) (Alert, bool) {
	if last, ok := a.cooldown[key]; ok && now.Sub(last) < alertCooldown {
		return Alert{}, false
	}
	a.cooldown[key] = now
	a.seq++
	return Alert{ID: fmt.Sprintf("alert-%d", a.seq), At: now, Level: level, Key: key, Title: title, Text: text}, true
}

// healthAlert reads the health rule off next.Health. A reading is fresh when
// its ReadAt or verdict differs from the last one, which is what stops a feed
// tick that merely repeated the health in State from counting as a poll.
func (a *Alerter) healthAlert(next State, now time.Time) (Alert, bool) {
	h := next.Health
	if h.ReadAt.Equal(a.lastHealthAt) && h.Verdict == a.lastVerdict {
		return Alert{}, false
	}
	a.lastHealthAt, a.lastVerdict = h.ReadAt, h.Verdict

	switch h.Verdict {
	case "green":
		a.healthStreak = 0
		a.unknownSince = time.Time{}
		live := a.healthLive
		a.healthLive = false
		if live {
			return a.fire(now, LevelInfo, "health:recovered", "town health recovered", h.Line)
		}
	case "unknown":
		a.healthStreak = 0
		if a.unknownSince.IsZero() {
			a.unknownSince = now
		}
		if a.healthLive || now.Sub(a.unknownSince) < healthUnknownAfter {
			return Alert{}, false
		}
		al, ok := a.fire(now, LevelWarn, "health:unknown", "town health has been UNKNOWN for five minutes", h.Line)
		a.healthLive = a.healthLive || ok
		return al, ok
	case "red", "degraded":
		a.unknownSince = time.Time{}
		a.healthStreak++
		if a.healthLive || a.healthStreak < healthConfirmPolls {
			return Alert{}, false
		}
		level, key, title := LevelCrit, "health:red", "town health is red"
		if h.Verdict == "degraded" {
			level, key, title = LevelWarn, "health:degraded", "town health is degraded"
		}
		al, ok := a.fire(now, level, key, title, h.Line)
		a.healthLive = a.healthLive || ok
		return al, ok
	}
	return Alert{}, false
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
	baseline := a.polecatsSeen
	a.polecatsSeen = true
	if !baseline {
		return nil
	}

	var out []Alert
	for _, p := range list {
		if p.State != StateStalled && p.State != StateNeedsHuman {
			continue
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
		if al, ok := a.fire(now, LevelWarn, "polecat:"+key, title, text); ok {
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
	return a.fire(now, LevelCrit, "escalations", "escalations rose",
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
		al, ok := a.fire(now, LevelWarn, fmt.Sprintf("reject:%d", e.Seq),
			"a landing was rejected ("+m[1]+")", e.Text)
		if ok {
			out = append(out, al)
		}
	}
	return out
}

// stuckQueueAlert reports a bead that has waited to land for over half an hour,
// once per jam: the flag clears when the queue drains below the threshold, so
// the next jam alerts again.
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
	al, ok := a.fire(now, LevelWarn, "queue:stuck", "a bead has waited over 30 minutes to land",
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
