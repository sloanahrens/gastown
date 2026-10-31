package dashboard

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var alertBase = time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)

// healthAt is one health reading: a new ReadAt is what makes it a poll.
func healthAt(verdict string, at time.Time) State {
	return State{Health: Health{Line: verdict + " summary", Verdict: verdict, ReadAt: at}}
}

// observeHealth feeds one health reading taken d after alertBase, moving the
// alerter's clock to the same instant.
func observeHealth(a *Alerter, clock *time.Time, verdict string, d time.Duration) []Alert {
	*clock = alertBase.Add(d)
	return a.Observe(State{}, healthAt(verdict, *clock), nil)
}

func alertsWithKey(alerts []Alert, key string) []Alert {
	var out []Alert
	for _, a := range alerts {
		if a.Key == key {
			out = append(out, a)
		}
	}
	return out
}

// The health verdict is debounced: one bad read is noise, two are the town.
func TestHealthAlertsOnlyAfterTwoPolls(t *testing.T) {
	t.Parallel()
	a := NewAlerter(func() time.Time { return alertBase })
	red1 := healthAt("red", alertBase)
	red2 := healthAt("red", alertBase.Add(5*time.Second))
	red3 := healthAt("red", alertBase.Add(10*time.Second))

	if got := a.Observe(State{}, red1, nil); len(got) != 0 {
		t.Fatalf("one red poll alerted: %+v", got)
	}
	got := a.Observe(red1, red2, nil)
	if len(got) != 1 || got[0].Level != LevelCrit || got[0].Key != "health:red" {
		t.Fatalf("two red polls -> %+v, want one crit on health:red", got)
	}
	if got := a.Observe(red2, red3, nil); len(got) != 0 {
		t.Fatalf("a third red poll repeated the alert: %+v", got)
	}

	// A repeat of the health in State — a feed tick that read no health — is
	// not a poll and must not advance the debounce.
	b := NewAlerter(func() time.Time { return alertBase })
	b.Observe(State{}, red1, nil)
	if got := b.Observe(red1, red1, nil); len(got) != 0 {
		t.Fatalf("a repeated reading counted as a second poll: %+v", got)
	}

	// Degraded is a warn, and still needs two polls.
	c := NewAlerter(func() time.Time { return alertBase })
	deg := healthAt("degraded", alertBase)
	c.Observe(State{}, deg, nil)
	if got := c.Observe(deg, healthAt("degraded", alertBase.Add(5*time.Second)), nil); len(got) != 1 || got[0].Level != LevelWarn || got[0].Key != "health:degraded" {
		t.Fatalf("two degraded polls -> %+v, want one warn on health:degraded", got)
	}
}

// A return to green after an alert is worth saying, once.
func TestHealthRecoveryAfterAlert(t *testing.T) {
	t.Parallel()
	a := NewAlerter(func() time.Time { return alertBase })
	red1 := healthAt("red", alertBase)
	red2 := healthAt("red", alertBase.Add(5*time.Second))
	green := healthAt("green", alertBase.Add(10*time.Second))

	a.Observe(State{}, red1, nil)
	a.Observe(red1, red2, nil)
	got := a.Observe(red2, green, nil)
	if len(got) != 1 || got[0].Level != LevelInfo || got[0].Key != "health:recovered" {
		t.Fatalf("recovery -> %+v, want one info on health:recovered", got)
	}
	if got := a.Observe(green, healthAt("green", alertBase.Add(15*time.Second)), nil); len(got) != 0 {
		t.Fatalf("green after green alerted: %+v", got)
	}
}

// UNKNOWN is a restart artefact for one tick; only five minutes of it counts.
func TestUnknownAlertsOnlyAfterFiveMinutes(t *testing.T) {
	t.Parallel()
	now := alertBase
	a := NewAlerter(func() time.Time { return now })
	unknown := func(d time.Duration) State { return healthAt("unknown", alertBase.Add(d)) }
	// the one tick right after a daemon restart
	if got := a.Observe(State{}, unknown(0), nil); len(got) != 0 {
		t.Fatalf("a single UNKNOWN tick alerted: %+v", got)
	}
	now = alertBase.Add(time.Minute)
	if got := a.Observe(unknown(0), unknown(time.Minute), nil); len(got) != 0 {
		t.Fatalf("UNKNOWN for a minute alerted: %+v", got)
	}
	now = alertBase.Add(5 * time.Minute)
	got := a.Observe(unknown(time.Minute), unknown(5*time.Minute), nil)
	if len(got) != 1 || got[0].Level != LevelWarn || got[0].Key != "health:unknown" {
		t.Fatalf("UNKNOWN for five minutes -> %+v, want one warn on health:unknown", got)
	}
}

// An UNKNOWN alert stands for the UNKNOWN run only: a town that then goes red
// without passing through green still raises the red crit, once. A lower
// verdict after a higher one stays quiet.
func TestUnknownAlertDoesNotSuppressRed(t *testing.T) {
	t.Parallel()
	now := alertBase
	a := NewAlerter(func() time.Time { return now })
	observe := func(verdict string, d time.Duration) []Alert {
		return observeHealth(a, &now, verdict, d)
	}

	observe("unknown", 0)
	if got := observe("unknown", 5*time.Minute); len(got) != 1 || got[0].Key != "health:unknown" || got[0].Level != LevelWarn {
		t.Fatalf("UNKNOWN for five minutes -> %+v, want one warn on health:unknown", got)
	}
	if got := observe("red", 6*time.Minute); len(got) != 0 {
		t.Fatalf("one red poll alerted: %+v", got)
	}
	got := observe("red", 6*time.Minute+5*time.Second)
	if len(got) != 1 || got[0].Level != LevelCrit || got[0].Key != "health:red" {
		t.Fatalf("second red poll -> %+v, want exactly one crit on health:red", got)
	}
	if got := observe("red", 6*time.Minute+10*time.Second); len(got) != 0 {
		t.Fatalf("a third red poll repeated the alert: %+v", got)
	}
	// A lower verdict inside the same run raises nothing.
	for i, v := range []string{"degraded", "degraded", "unknown"} {
		if got := observe(v, 7*time.Minute+time.Duration(i)*time.Second); len(got) != 0 {
			t.Fatalf("%s after red alerted: %+v", v, got)
		}
	}
	got = observe("green", 8*time.Minute)
	if len(got) != 1 || got[0].Key != "health:recovered" {
		t.Fatalf("green -> %+v, want one health:recovered", got)
	}
}

// Degraded then red: red is its own verdict, so it takes two red polls of its
// own to confirm, and then it outranks the degraded alert with its own key.
func TestDegradedThenRedConfirmsRedSeparately(t *testing.T) {
	t.Parallel()
	now := alertBase
	b := NewAlerter(func() time.Time { return now })
	observe := func(verdict string, d time.Duration) []Alert {
		return observeHealth(b, &now, verdict, d)
	}

	observe("degraded", 0)
	if got := observe("degraded", 5*time.Second); len(got) != 1 || got[0].Key != "health:degraded" {
		t.Fatalf("two degraded polls -> %+v, want one health:degraded", got)
	}
	if got := observe("red", 10*time.Second); len(got) != 0 {
		t.Fatalf("one red poll after degraded alerted: %+v", got)
	}
	got := observe("red", 15*time.Second)
	if len(got) != 1 || got[0].Key != "health:red" || got[0].Level != LevelCrit {
		t.Fatalf("two red polls after degraded -> %+v, want one crit on health:red", got)
	}
	if got := observe("red", 20*time.Second); len(got) != 0 {
		t.Fatalf("a third red poll repeated the alert: %+v", got)
	}
}

// A polecat already stalled when the page connects is not news; a polecat that
// newly stalls is, and the alert names it and its bead.
func TestPolecatAlertsOnNewStallOnly(t *testing.T) {
	t.Parallel()
	stalled := Polecat{Rig: "gastown", Name: "agate", Bead: "gt-1", Title: "some work", State: StateStalled}
	working := stalled
	working.State = StateWorking
	needsHuman := stalled
	needsHuman.State = StateNeedsHuman

	// The first snapshot is the baseline.
	a := NewAlerter(func() time.Time { return alertBase })
	a.Observe(State{}, State{Polecats: []Polecat{stalled}}, nil)
	if got := a.Observe(State{Polecats: []Polecat{stalled}}, State{Polecats: []Polecat{stalled}}, nil); len(got) != 0 {
		t.Fatalf("a polecat already stalled alerted: %+v", got)
	}

	// It works, then newly stalls.
	a.Observe(State{Polecats: []Polecat{stalled}}, State{Polecats: []Polecat{working}}, nil)
	got := a.Observe(State{Polecats: []Polecat{working}}, State{Polecats: []Polecat{stalled}}, nil)
	if len(got) != 1 || got[0].Level != LevelWarn {
		t.Fatalf("a newly stalled polecat -> %+v, want one warn", got)
	}
	if !strings.Contains(got[0].Title, "agate") || !strings.Contains(got[0].Text, "gt-1") {
		t.Errorf("alert %+v does not name the polecat and the bead", got[0])
	}

	// needs-human is the same class of news, and a second polecat is its own key.
	b := NewAlerter(func() time.Time { return alertBase })
	b.Observe(State{}, State{Polecats: []Polecat{working}}, nil)
	got = b.Observe(State{Polecats: []Polecat{working}}, State{Polecats: []Polecat{needsHuman}}, nil)
	if len(got) != 1 || got[0].Key != "polecat:gastown/agate" || got[0].Level != LevelWarn {
		t.Fatalf("newly needs-human -> %+v, want one warn on the polecat key", got)
	}
}

// gt-slmaz: the two labels that name who must act are the same news as the old
// one: a polecat newly held under either alerts once, on its own polecat key.
func TestPolecatAlertsOnNeedsOverseerAndOperator(t *testing.T) {
	t.Parallel()
	working := Polecat{Rig: "gastown", Name: "agate", Bead: "gt-1", Title: "some work", State: StateWorking}
	for _, state := range []string{StateNeedsOverseer, StateNeedsOperator} {
		held := working
		held.State = state
		a := NewAlerter(func() time.Time { return alertBase })
		a.Observe(State{}, State{Polecats: []Polecat{working}}, nil)
		got := a.Observe(State{Polecats: []Polecat{working}}, State{Polecats: []Polecat{held}}, nil)
		if len(got) != 1 || got[0].Key != "polecat:gastown/agate" || got[0].Level != LevelWarn {
			t.Fatalf("newly %s -> %+v, want one warn on the polecat key", state, got)
		}
	}
}

// A rising escalation count is critical; the first count is a baseline.
func TestEscalationIncreaseAlerts(t *testing.T) {
	t.Parallel()
	esc := func(n int) State {
		s := State{Summary: &Summary{}}
		s.Summary.Escalations = &n
		return s
	}
	a := NewAlerter(func() time.Time { return alertBase })
	if got := a.Observe(State{}, esc(2), nil); len(got) != 0 {
		t.Fatalf("the first escalation count alerted: %+v", got)
	}
	if got := a.Observe(esc(2), esc(2), nil); len(got) != 0 {
		t.Fatalf("a flat escalation count alerted: %+v", got)
	}
	got := a.Observe(esc(2), esc(3), nil)
	if len(got) != 1 || got[0].Level != LevelCrit || got[0].Key != "escalations" {
		t.Fatalf("a rising escalation count -> %+v, want one crit", got)
	}
}

// Every rejection line is its own alert, and a redelivered line is not.
func TestRejectionEntriesAlertOnce(t *testing.T) {
	t.Parallel()
	gate := Entry{Seq: 7, At: alertBase, Kind: "landings", Text: "[land] gt-1: rejected (gate): lint failed"}
	review := Entry{Seq: 8, At: alertBase, Kind: "landings", Text: "[land] gt-2: rejected (review): score 0.4"}
	landed := Entry{Seq: 9, At: alertBase, Kind: "landings", Text: "[land] gt-3: landed ok"}

	a := NewAlerter(func() time.Time { return alertBase })
	got := a.Observe(State{}, State{}, []Entry{gate, review, landed})
	if len(got) != 2 {
		t.Fatalf("rejections -> %+v, want two", got)
	}
	for _, al := range got {
		if al.Level != LevelWarn {
			t.Errorf("rejection %+v is not a warn", al)
		}
	}
	if !strings.Contains(got[0].Title, "gate") || !strings.Contains(got[1].Title, "review") {
		t.Errorf("rejections %+v do not name the kind", got)
	}
	if again := a.Observe(State{}, State{}, []Entry{gate, review}); len(again) != 0 {
		t.Fatalf("redelivered entries alerted again: %+v", again)
	}
}

// An alert about a bead names the bead on its own field, so the EVENTS feed can
// show it as a column: a rejected landing and a polecat holding work both do.
func TestAlertsNameTheBeadTheyAreAbout(t *testing.T) {
	t.Parallel()
	a := NewAlerter(func() time.Time { return alertBase })
	got := a.Observe(State{}, State{}, []Entry{
		{Seq: 20, At: alertBase, Kind: "daemon", Text: "landing_worker: [land] gt-7: rejected (review): score 0.4"},
	})
	if len(got) != 1 || got[0].Bead != "gt-7" {
		t.Fatalf("rejection -> %+v, want one alert naming gt-7", got)
	}

	b := NewAlerter(func() time.Time { return alertBase })
	healthy := State{Polecats: []Polecat{{Rig: "gastown", Name: "ace", Bead: "gt-8", State: StateWorking}}}
	b.Observe(State{}, healthy, nil)
	stalled := State{Polecats: []Polecat{{Rig: "gastown", Name: "ace", Bead: "gt-8", State: StateStalled}}}
	stall := b.Observe(healthy, stalled, nil)
	if len(stall) != 1 || stall[0].Bead != "gt-8" {
		t.Fatalf("stalled polecat -> %+v, want one alert naming gt-8", stall)
	}
}

// A queue stuck past half an hour alerts once per jam, and the cooldown holds a
// repeat back until it expires once the jam clears and returns.
func TestStuckQueueAlertsOnceAndRearms(t *testing.T) {
	t.Parallel()
	now := alertBase
	a := NewAlerter(func() time.Time { return now })
	oldest := alertBase.Add(-40 * time.Minute)
	stuck := State{Summary: &Summary{OldestReady: &oldest}}
	empty := State{Summary: &Summary{}}

	if got := a.Observe(State{}, empty, nil); len(got) != 0 {
		t.Fatalf("an empty queue alerted: %+v", got)
	}
	if got := a.Observe(empty, State{Summary: &Summary{OldestReady: timePtr(alertBase.Add(-20 * time.Minute))}}, nil); len(got) != 0 {
		t.Fatalf("a 20-minute wait alerted: %+v", got)
	}
	got := a.Observe(empty, stuck, nil)
	if len(got) != 1 || got[0].Level != LevelWarn || got[0].Key != "queue:stuck" {
		t.Fatalf("a 40-minute wait -> %+v, want one warn on queue:stuck", got)
	}
	if got := a.Observe(stuck, stuck, nil); len(got) != 0 {
		t.Fatalf("a queue still stuck repeated the alert: %+v", got)
	}

	// The queue drains, then jams again inside the cooldown: held back.
	a.Observe(stuck, empty, nil)
	if got := a.Observe(empty, stuck, nil); len(got) != 0 {
		t.Fatalf("a repeat inside the cooldown was not suppressed: %+v", got)
	}
	// Past the cooldown it fires again.
	now = alertBase.Add(11 * time.Minute)
	if got := a.Observe(empty, stuck, nil); len(got) != 1 {
		t.Fatalf("a repeat after the cooldown -> %+v, want one warn", got)
	}
}

// A dispatcher that stops ticking alerts once, and a fresh tick re-arms it.
func TestDispatcherSilenceAlertsAndRearms(t *testing.T) {
	t.Parallel()
	now := alertBase
	a := NewAlerter(func() time.Time { return now })
	tick := func(at time.Time) State { return State{Dispatch: &Dispatch{At: at}} }

	if got := a.Observe(State{}, tick(alertBase), nil); len(got) != 0 {
		t.Fatalf("a fresh dispatcher tick alerted: %+v", got)
	}
	now = alertBase.Add(6 * time.Minute)
	got := a.Observe(tick(alertBase), tick(alertBase), nil)
	if len(got) != 1 || got[0].Level != LevelWarn || got[0].Key != "dispatch:silent" {
		t.Fatalf("six minutes of silence -> %+v, want one warn on dispatch:silent", got)
	}
	if got := a.Observe(tick(alertBase), tick(alertBase), nil); len(got) != 0 {
		t.Fatalf("silence repeated the alert: %+v", got)
	}

	// A new tick re-arms the rule; that silence then goes quiet and, once the
	// cooldown has passed, alerts again.
	now = alertBase.Add(7 * time.Minute)
	fresh := alertBase.Add(7 * time.Minute)
	a.Observe(tick(alertBase), tick(fresh), nil)
	now = alertBase.Add(17 * time.Minute)
	if got := a.Observe(tick(fresh), tick(fresh), nil); len(got) != 1 {
		t.Fatalf("a second silence after the cooldown -> %+v, want one warn", got)
	}
}

// The baseline rules never announce state that already existed.
func TestFirstSnapshotBaselinesPolecatsAndEscalations(t *testing.T) {
	t.Parallel()
	a := NewAlerter(func() time.Time { return alertBase })
	escalations := 4
	first := State{
		Polecats: []Polecat{{Rig: "g", Name: "agate", Bead: "gt-1", State: StateStalled}},
		Summary:  &Summary{Escalations: &escalations},
	}
	if got := a.Observe(State{}, first, nil); len(got) != 0 {
		t.Fatalf("the baseline snapshot alerted: %+v", got)
	}
	if got := a.Observe(first, first, nil); len(got) != 0 {
		t.Fatalf("a snapshot with no change alerted: %+v", got)
	}
}

// Baseline makes the next snapshot a baseline again, which is what a hub does
// when the first page connects after a polling gap.
func TestBaselineReestablishesTheSnapshot(t *testing.T) {
	t.Parallel()
	a := NewAlerter(func() time.Time { return alertBase })
	stalled := State{Polecats: []Polecat{{Rig: "g", Name: "agate", Bead: "gt-1", State: StateStalled}}}
	a.Observe(State{}, State{}, nil) // baseline with no polecats
	a.Baseline()
	if got := a.Observe(State{}, stalled, nil); len(got) != 0 {
		t.Fatalf("a polecat stalled across a polling gap alerted: %+v", got)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// The hub turns the alerter's decisions into an event the page can hear, and
// keeps the recent ones in the state the page renders from.
func TestHubBroadcastsAlertsAndCapsState(t *testing.T) {
	t.Parallel()
	now := alertBase
	reads := 0
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Health: func() Health {
			reads++
			return Health{Line: "RED disk", Verdict: "red", ReadAt: alertBase.Add(time.Duration(reads) * time.Second)}
		},
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollHealth() // the first bad read is held by the debounce
	drainFrames(page)
	now = now.Add(time.Second)
	h.pollHealth() // the second raises it
	frames := drainFrames(page)

	if !strings.Contains(frames, "event: alert") || !strings.Contains(frames, `"key":"health:red"`) {
		t.Fatalf("no alert frame reached the page: %q", frames)
	}
	if !strings.Contains(frames, `"level":"crit"`) {
		t.Errorf("alert frame is not a crit: %q", frames)
	}
	st := h.State()
	if len(st.Alerts) != 1 || st.Alerts[0].Key != "health:red" {
		t.Fatalf("State.Alerts = %+v, want the health:red alert", st.Alerts)
	}
}

// State.Alerts keeps the last alertsKept, newest last.
func TestHubCapsRecentAlerts(t *testing.T) {
	t.Parallel()
	var entries []Entry
	for i := 0; i < alertsKept+2; i++ {
		entries = append(entries, Entry{At: alertBase, Kind: "landings",
			Text: fmt.Sprintf("[land] gt-%d: rejected (gate): failed", i)})
	}
	h := NewHub(Config{
		Now:  func() time.Time { return alertBase },
		Feed: func() []Entry { return entries },
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollFeed()
	st := h.State()
	if len(st.Alerts) != alertsKept {
		t.Fatalf("State.Alerts holds %d, want %d", len(st.Alerts), alertsKept)
	}
	if !strings.Contains(st.Alerts[len(st.Alerts)-1].Text, fmt.Sprintf("gt-%d", alertsKept+1)) {
		t.Errorf("the newest alert was dropped: %+v", st.Alerts[len(st.Alerts)-1])
	}
}

// The page carries the alert controls the operator uses. Every alert lands in the
// EVENTS feed (gt-wpgga), and the switch controls the title marker and browser
// notifications; what is waiting rather than merely noticed is the Attention
// panel's (gt-bi6w0). There is no JS harness, so this is a presence check.
func TestPageCarriesTheAlertControls(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:8787"
	NewHub(Config{}).Handler().ServeHTTP(rec, req)
	page := rec.Body.String()
	for _, want := range []string{
		`id="alertsctl"`, `id="events"`, `id="attention"`,
		`localStorage.getItem("gt-alerts")`,          // persisted, default off
		`"(!) " + BASE_TITLE`,                        // the title marker
		`es.addEventListener("alert"`,                // the server's alert event
		`renderAttention(state)`,                     // the one panel that lists what waits
		`Notification.permission !== "granted"`,      // degrades without the API
		`localStorage.setItem("gt-alerts", alertsOn`, // the toggle writes it back
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the served page is missing %s", want)
		}
	}
}

// drainFrames takes every frame the page has buffered, without blocking.
func drainFrames(s *Sub) string {
	var b strings.Builder
	for {
		select {
		case f := <-s.C:
			b.Write(f)
		default:
			return b.String()
		}
	}
}

// The feed hands its backlog to the first page that connects. A rejection from
// before the page opened is history, not an alert.
func TestOldRejectionsAreNotAlerts(t *testing.T) {
	t.Parallel()
	old := Entry{Seq: 1, At: alertBase.Add(-time.Hour), Kind: "landings", Text: "[land] gt-1: rejected (review): score 0.4"}
	recent := Entry{Seq: 2, At: alertBase.Add(-time.Minute), Kind: "landings", Text: "[land] gt-2: rejected (gate): lint failed"}
	a := NewAlerter(func() time.Time { return alertBase })
	got := a.Observe(State{}, State{}, []Entry{old, recent})
	if len(got) != 1 || !strings.Contains(got[0].Title, "gate") {
		t.Fatalf("backlog -> %+v, want only the recent rejection", got)
	}
}

// The landing worker logs one rejection as two lines, and both are in the feed.
func TestOneRejectionLoggedTwiceAlertsOnce(t *testing.T) {
	t.Parallel()
	first := Entry{Seq: 10, At: alertBase, Kind: "daemon", Text: "landing_worker: [land] gt-9: rejected (review): om requested changes (score 0.50)"}
	second := Entry{Seq: 11, At: alertBase, Kind: "daemon", Text: "landing_worker: gastown: gt-9: landing rejected (review): om requested changes (score 0.50)"}
	other := Entry{Seq: 12, At: alertBase, Kind: "daemon", Text: "landing_worker: [land] gt-10: rejected (review): om requested changes (score 0.55)"}
	a := NewAlerter(func() time.Time { return alertBase })
	got := a.Observe(State{}, State{}, []Entry{first, second, other})
	if len(got) != 2 {
		t.Fatalf("two beads rejected (one logged twice) -> %d alerts: %+v", len(got), got)
	}
}

// The first snapshot after a page connects is taken before the polecat summary
// has been read, so it lists no polecats. The baseline is the first snapshot
// that does: a polecat that was already stalled is not news.
func TestPolecatBaselineWaitsUntilPolecatsHaveBeenRead(t *testing.T) {
	t.Parallel()
	a := NewAlerter(func() time.Time { return alertBase })
	stalled := Polecat{Rig: "beads", Name: "mutant", Bead: "be-1", State: StateStalled}
	working := stalled
	working.State = StateWorking

	if got := a.Observe(State{}, State{}, nil); len(got) != 0 {
		t.Fatalf("an empty first snapshot alerted: %+v", got)
	}
	read := State{Polecats: []Polecat{stalled}}
	if got := a.Observe(State{}, read, nil); len(got) != 0 {
		t.Fatalf("a polecat already stalled when the summary was first read alerted: %+v", got)
	}
	// real news after the baseline still alerts
	a.Observe(read, State{Polecats: []Polecat{working}}, nil)
	if got := a.Observe(State{Polecats: []Polecat{working}}, read, nil); len(got) != 1 {
		t.Fatalf("a polecat that newly stalled -> %+v, want one alert", got)
	}
}

// A parked rig is stood down on purpose. Its polecats stalling is the expected
// state, not news, so they never alert; a live rig's polecat still does.
func TestParkedRigPolecatsDoNotAlert(t *testing.T) {
	t.Parallel()
	mutant := Polecat{Rig: "beads", Name: "mutant", Bead: "be-1", State: StateWorking, RigParked: true}
	agate := Polecat{Rig: "gastown", Name: "agate", Bead: "gt-1", State: StateWorking}
	a := NewAlerter(func() time.Time { return alertBase })
	prev := State{Polecats: []Polecat{mutant, agate}}
	a.Observe(State{}, prev, nil) // baseline

	mutant.State, agate.State = StateStalled, StateStalled
	got := a.Observe(prev, State{Polecats: []Polecat{mutant, agate}}, nil)
	if len(got) != 1 || got[0].Key != "polecat:gastown/agate" {
		t.Fatalf("both stalled -> %+v, want one alert, for the live rig only", got)
	}
}
