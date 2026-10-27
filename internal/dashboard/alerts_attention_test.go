package dashboard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// wakeEvent is a raised line the way the daemon writes one: the rule's own
// flags, which for every waking rule are operator_action and wake.
func wakeEvent(rule, key, text string, sev attention.Severity) attention.Event {
	return attention.Event{
		TS: alertBase, Class: attention.KindRedMain, Severity: sev, Text: text, Key: key,
		State: attention.EventNew, Rule: rule, Handled: attention.HandledOperatorAction, Wake: true,
	}
}

func clearedEvent(e attention.Event) attention.Event {
	e.State, e.Handled, e.Wake = attention.EventCleared, attention.HandledInfo, false
	return e
}

// A wake=true new line is an alert keyed by the event's key, titled with its
// rule id and carrying its text, at the level its severity names.
func TestAttentionWakeEventRaisesAnAlert(t *testing.T) {
	t.Parallel()
	a := NewAlerter(func() time.Time { return alertBase.Add(time.Hour) })
	high := wakeEvent(attention.RuleSlotHeld, "slot:gastown", "slot held 3h", attention.SeverityHigh)
	low := wakeEvent(attention.RuleBDSlow, "bd-slow:x", "bd took 9s", attention.SeverityLow)

	raised, removed := a.ObserveAttention([]attention.Event{high, low}, attention.Acks{})
	if len(raised) != 2 || len(removed) != 0 {
		t.Fatalf("raised %+v removed %v, want two alerts and no removals", raised, removed)
	}
	if g := raised[0]; g.Key != "slot:gastown" || g.Title != attention.RuleSlotHeld || g.Text != "slot held 3h" || g.Level != LevelCrit || !g.At.Equal(alertBase) {
		t.Errorf("high alert = %+v", g)
	}
	if g := raised[1]; g.Key != "bd-slow:x" || g.Title != attention.RuleBDSlow || g.Level != LevelWarn {
		t.Errorf("low alert = %+v", g)
	}
	if raised[0].ID == "" || raised[0].ID == raised[1].ID {
		t.Errorf("alert ids %q %q must be set and distinct", raised[0].ID, raised[1].ID)
	}

	// The daemon repeats a standing item ("still holding"); it stays one alert.
	again, _ := a.ObserveAttention([]attention.Event{high}, attention.Acks{})
	if len(again) != 0 {
		t.Errorf("a repeat line raised %+v", again)
	}
}

// A cleared line removes the alert, and the condition returning raises it again.
func TestAttentionClearedEventRemovesTheAlert(t *testing.T) {
	t.Parallel()
	a := NewAlerter(nil)
	e := wakeEvent(attention.RuleSlotHeld, "slot:gastown", "slot held", attention.SeverityHigh)
	a.ObserveAttention([]attention.Event{e}, attention.Acks{})

	raised, removed := a.ObserveAttention([]attention.Event{clearedEvent(e)}, attention.Acks{})
	if len(raised) != 0 || !reflect.DeepEqual(removed, []string{"slot:gastown"}) {
		t.Fatalf("cleared -> raised %+v removed %v, want the key removed", raised, removed)
	}
	if raised, _ := a.ObserveAttention([]attention.Event{e}, attention.Acks{}); len(raised) != 1 {
		t.Errorf("a returning condition raised %+v, want one alert", raised)
	}

	// A line that raises and clears in one read shows nothing.
	b := NewAlerter(nil)
	raised, _ = b.ObserveAttention([]attention.Event{e, clearedEvent(e)}, attention.Acks{})
	if len(raised) != 0 {
		t.Errorf("raised and cleared in one read still alerted: %+v", raised)
	}
	// A cleared line for a key that never stood removes nothing.
	if _, removed := b.ObserveAttention([]attention.Event{clearedEvent(e)}, attention.Acks{}); len(removed) != 0 {
		t.Errorf("a stray cleared line removed %v", removed)
	}
}

// An acked key is not alerted, and an ack made while the alert stands hides it.
func TestAttentionAckedKeyIsNotShown(t *testing.T) {
	t.Parallel()
	e := wakeEvent(attention.RuleSlotHeld, "slot:gastown", "slot held", attention.SeverityHigh)
	acked := attention.Acks{Acks: []attention.Ack{{Key: "slot:gastown", At: alertBase}}}

	a := NewAlerter(nil)
	if raised, _ := a.ObserveAttention([]attention.Event{e}, acked); len(raised) != 0 {
		t.Fatalf("an acked key raised %+v", raised)
	}

	b := NewAlerter(nil)
	b.ObserveAttention([]attention.Event{e}, attention.Acks{})
	raised, removed := b.ObserveAttention(nil, acked)
	if len(raised) != 0 || !reflect.DeepEqual(removed, []string{"slot:gastown"}) {
		t.Fatalf("an ack on a standing alert -> raised %+v removed %v, want it removed", raised, removed)
	}
}

// Lines that do not wake the overseer raise nothing: info records, lines with
// no wake flag, and lines written before the fields existed.
func TestAttentionInfoAndNonWakeEventsRaiseNothing(t *testing.T) {
	t.Parallel()
	base := wakeEvent(attention.RuleSlotHeld, "slot:gastown", "slot held", attention.SeverityHigh)
	noWake := base
	noWake.Wake = false
	infoWake := base
	infoWake.Handled = attention.HandledInfo
	selfHealed := base
	selfHealed.Handled, selfHealed.Wake = attention.HandledSelfHealed, false
	old := attention.Event{TS: alertBase, Class: attention.KindSlotHeld, Severity: attention.SeverityHigh, Text: "old", Key: "slot:old", State: attention.EventNew}

	a := NewAlerter(nil)
	raised, removed := a.ObserveAttention([]attention.Event{noWake, infoWake, selfHealed, old}, attention.Acks{})
	if len(raised) != 0 || len(removed) != 0 {
		t.Fatalf("raised %+v removed %v, want nothing", raised, removed)
	}
}

// The table of the Alerter's alerts each attention rule stands in for.
func TestAttentionCoveredTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rule, key string
		want      []string
	}{
		{attention.RuleRedMain, "red-main:gastown:internal/cmd", []string{"health:red", "health:degraded"}},
		{attention.RuleEscalation, "esc:hq-1", []string{"escalations"}},
		{attention.RulePolecatStall, "stall:gastown/opal", []string{"polecat:gastown/opal"}},
		{attention.RuleQueueStuck, "queue-stuck:gastown", []string{"queue:stuck"}},
		{attention.RuleSlotHeld, "slot:gastown", nil},
		{attention.RulePolecatStall, "unexpected", nil},
	}
	for _, c := range cases {
		if got := attentionCovered(c.rule, c.key); !reflect.DeepEqual(got, c.want) {
			t.Errorf("attentionCovered(%s, %s) = %v, want %v", c.rule, c.key, got, c.want)
		}
	}
}

// An alert the Alerter already raised for the same condition is removed when
// the attention one arrives, and one it would raise afterwards is held back:
// either way the condition is shown once.
func TestAttentionAlertSupersedesTheAlertersOwn(t *testing.T) {
	t.Parallel()
	esc := func(n int) State {
		s := State{Summary: &Summary{}}
		s.Summary.Escalations = &n
		return s
	}
	ev := wakeEvent(attention.RuleEscalation, "esc:hq-1", "hq-1 awaits the overseer", attention.SeverityHigh)

	// The Alerter's alert stands first.
	a := NewAlerter(func() time.Time { return alertBase })
	a.Observe(State{}, esc(1), nil)
	if got := a.Observe(esc(1), esc(2), nil); len(got) != 1 || got[0].Key != "escalations" {
		t.Fatalf("setup: %+v", got)
	}
	raised, removed := a.ObserveAttention([]attention.Event{ev}, attention.Acks{})
	if len(raised) != 1 || raised[0].Key != "esc:hq-1" || !reflect.DeepEqual(removed, []string{"escalations"}) {
		t.Fatalf("raised %+v removed %v, want the attention alert replacing escalations", raised, removed)
	}

	// The attention alert stands first. The escalation rule then stays quiet,
	// and speaks again once the attention alert has cleared.
	b := NewAlerter(func() time.Time { return alertBase.Add(time.Hour) })
	b.Observe(State{}, esc(1), nil)
	b.ObserveAttention([]attention.Event{ev}, attention.Acks{})
	if got := b.Observe(esc(1), esc(2), nil); len(got) != 0 {
		t.Fatalf("the escalation rule alerted beside the attention alert: %+v", got)
	}
	b.ObserveAttention([]attention.Event{clearedEvent(ev)}, attention.Acks{})
	if got := b.Observe(esc(2), esc(3), nil); len(got) != 1 || got[0].Key != "escalations" {
		t.Errorf("after the clear, a rise -> %+v, want the escalations alert", got)
	}
}

// The polecat stall, the stuck queue and red main are held back the same way.
func TestAttentionAlertCoversStallQueueAndMain(t *testing.T) {
	t.Parallel()
	now := alertBase.Add(2 * time.Hour)
	stalled := Polecat{Rig: "gastown", Name: "opal", Bead: "gt-1", State: StateStalled}
	working := stalled
	working.State = StateWorking
	oldest := now.Add(-time.Hour)

	a := NewAlerter(func() time.Time { return now })
	a.ObserveAttention([]attention.Event{
		wakeEvent(attention.RulePolecatStall, "stall:gastown/opal", "opal stalled", attention.SeverityHigh),
		wakeEvent(attention.RuleQueueStuck, "queue-stuck:gastown", "queue silent", attention.SeverityHigh),
		wakeEvent(attention.RuleRedMain, "red-main:gastown:x", "main red", attention.SeverityHigh),
	}, attention.Acks{})

	a.Observe(State{}, State{Polecats: []Polecat{working}}, nil)
	next := State{Polecats: []Polecat{stalled}, Summary: &Summary{OldestReady: &oldest},
		Health: Health{Verdict: "red", Line: "red", ReadAt: now}}
	if got := a.Observe(State{Polecats: []Polecat{working}}, next, nil); len(got) != 0 {
		t.Fatalf("covered rules alerted anyway: %+v", got)
	}
	// Health needs two reads to alert, so give it the second.
	next.Health.ReadAt = now.Add(time.Second)
	if got := a.Observe(next, next, nil); len(got) != 0 {
		t.Fatalf("the health rule alerted beside the red-main alert: %+v", got)
	}
}

// writeAttentionLines appends events to events.jsonl under a temp town.
func writeAttentionEvents(t *testing.T, town string, evs ...attention.Event) {
	t.Helper()
	if err := attention.AppendEvents(town, evs); err != nil {
		t.Fatal(err)
	}
}

func eventKeys(evs []attention.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, string(e.State)+" "+e.Key)
	}
	return out
}

// Items reads the queue's items off state.json, where each standing row's class,
// owner and waiting-on come from. A state the daemon has not written yet is no
// items and no error, the same as the queue itself reads it; a state that is
// there and cannot be read returns its error rather than passing for an empty
// queue (gt-tv5v2).
func TestAttentionFollowerReadsTheQueueItems(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f := NewAttentionFollower(town)
	if items, err := f.Items(); err != nil || len(items) != 0 {
		t.Fatalf("a missing state read %+v, %v, want none and no error", items, err)
	}

	item := attention.Item{Key: "rejected-twice:gt-9", Kind: attention.KindRejectedTwice,
		Owner: "opal", WaitingOn: "rework by opal, attempt 2", FirstSeen: alertBase, LastSeen: alertBase}
	if err := attention.WriteState(town, attention.State{Updated: alertBase, Items: []attention.Item{item}}); err != nil {
		t.Fatalf("writing the queue's state: %v", err)
	}
	items, err := f.Items()
	if err != nil {
		t.Fatalf("reading the queue's state: %v", err)
	}
	if len(items) != 1 || items[0].Key != item.Key || items[0].Owner != "opal" || items[0].WaitingOn != item.WaitingOn {
		t.Errorf("the follower read %+v, want the queue's item with its owner and waiting-on", items)
	}

	// .runtime is a file here, so the state under it cannot be opened.
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, ".runtime"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if items, err := NewAttentionFollower(broken).Items(); err == nil || items != nil {
		t.Errorf("an unreadable state read %+v, %v, want none and the error", items, err)
	}
}

// A town with no events file, or one that cannot be read, raises nothing, and
// the follower picks the file up when it appears.
func TestAttentionFollowerSurvivesAMissingOrUnreadableFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f := NewAttentionFollower(town)
	if evs, _ := f.Read(); len(evs) != 0 {
		t.Fatalf("a missing events file read %+v", evs)
	}
	if _, err := os.Stat(attention.EventsPath(town)); err == nil {
		t.Error("reading created the events file")
	}

	// .runtime is a file here, so nothing under it can be opened.
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, ".runtime"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := NewAttentionFollower(broken)
	if evs, _ := g.Read(); len(evs) != 0 {
		t.Fatalf("an unreadable events file read %+v", evs)
	}

	e := wakeEvent(attention.RuleSlotHeld, "slot:gastown", "slot held", attention.SeverityHigh)
	writeAttentionEvents(t, town, e)
	evs, _ := f.Read()
	if got := eventKeys(evs); !reflect.DeepEqual(got, []string{"new slot:gastown"}) {
		t.Errorf("after the file appeared, read %v", got)
	}
}

// The first read hands over the retained history, so a condition raised before
// the page opened is on it; later reads hand over only what was appended, and
// follow the file across its rotation.
func TestAttentionFollowerReadsHistoryThenAppendsAcrossRotation(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	standing := wakeEvent(attention.RuleSlotHeld, "slot:gastown", "slot held", attention.SeverityHigh)
	writeAttentionEvents(t, town, standing)

	f := NewAttentionFollower(town)
	evs, _ := f.Read()
	if got := eventKeys(evs); !reflect.DeepEqual(got, []string{"new slot:gastown"}) {
		t.Fatalf("history = %v", got)
	}
	if evs, _ := f.Read(); len(evs) != 0 {
		t.Fatalf("a second read with no appends returned %v", eventKeys(evs))
	}

	// Fill past the 1 MB cap with records that wake nothing, then raise another.
	filler := attention.Event{TS: alertBase, Class: attention.KindRiskPath, Severity: attention.SeverityLow,
		Text: strings.Repeat("x", 4000), State: attention.EventNew, Rule: attention.RuleRiskPath, Handled: attention.HandledInfo}
	for i := 0; i < 300; i++ {
		filler.Key = "risk:" + string(rune('a'+i%26))
		writeAttentionEvents(t, town, filler)
	}
	if _, err := os.Stat(attention.RotatedEventsPath(town)); err != nil {
		t.Fatalf("the log did not rotate: %v", err)
	}
	fresh := wakeEvent(attention.RuleEscalation, "esc:hq-9", "hq-9", attention.SeverityHigh)
	writeAttentionEvents(t, town, fresh)

	evs, _ = f.Read()
	var seen int
	for _, e := range evs {
		if e.Key == "esc:hq-9" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the line appended after the rotation was read %d times, want 1", seen)
	}

	// Acks are read each time.
	if err := attention.Acknowledge(town, "esc:hq-9", alertBase); err != nil {
		t.Fatal(err)
	}
	if _, acks := f.Read(); !attention.AckedKey(acks, "esc:hq-9") {
		t.Error("the follower did not return the ack")
	}
}

// End to end through the hub: a wake line reaches the page and State.Alerts,
// and the cleared line takes it off both.
func TestHubShowsAndClearsAttentionAlerts(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	f := NewAttentionFollower(town)
	h := NewHub(Config{Now: func() time.Time { return alertBase }, Attention: f.Read})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollAttention() // no file yet
	if st := h.State(); len(st.Alerts) != 0 {
		t.Fatalf("a missing events file left alerts: %+v", st.Alerts)
	}

	e := wakeEvent(attention.RuleSlotHeld, "slot:gastown", "slot held 3h", attention.SeverityHigh)
	writeAttentionEvents(t, town, attention.Event{TS: alertBase, Class: attention.KindRiskPath, Severity: attention.SeverityLow,
		Text: "record", Key: "risk:x", State: attention.EventNew, Rule: attention.RuleRiskPath, Handled: attention.HandledInfo})
	h.pollAttention() // opens the tail; the file held only an info line
	drainFrames(page)
	writeAttentionEvents(t, town, e)
	h.pollAttention()
	frames := drainFrames(page)
	if !strings.Contains(frames, "event: alert") || !strings.Contains(frames, `"title":"slot.held"`) || !strings.Contains(frames, "slot held 3h") {
		t.Fatalf("the page did not hear the alert: %q", frames)
	}
	if st := h.State(); len(st.Alerts) != 1 || st.Alerts[0].Key != "slot:gastown" {
		t.Fatalf("State.Alerts = %+v, want the slot alert", st.Alerts)
	}

	writeAttentionEvents(t, town, clearedEvent(e))
	h.pollAttention()
	if st := h.State(); len(st.Alerts) != 0 {
		t.Fatalf("State.Alerts = %+v after the clear, want none", st.Alerts)
	}
	if !strings.Contains(drainFrames(page), "event: state") {
		t.Error("the page was not sent the state without the alert")
	}
}
