package dashboard

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// The EVENTS panel is the page's history of the alerts that used to stack over
// the panels as toasts (gt-wpgga). These tests run index.html's own script over
// the stream stub and the fake clock (page_harness_test.go): an alert arrives on
// the stream the way the server sends it, a state refresh carries the recent
// list the way the hub sends it, and each test reads back the rows and the
// header the page drew.

// feedLine is one row of the EVENTS panel as the page drew it: the row's classes
// and the text of each cell, in the order time, kind, bead, reason.
type feedLine struct {
	Cls   string   `json:"cls"`
	Cells []string `json:"cells"`
}

// feedLines reads the rows of #events, newest first as the panel draws them.
func feedLines(t *testing.T, dom *pageDOM) []feedLine {
	t.Helper()

	js := `JSON.stringify(__byId["events"].children.map(r => ({
		cls: r.className,
		cells: r.children.map(c => c.textContent)})))`
	var out []feedLine
	if err := json.Unmarshal([]byte(pageExpr(t, dom, js)), &out); err != nil {
		t.Fatalf("reading the EVENTS rows: %v", err)
	}
	return out
}

// eventsHeaderNote is the EVENTS header's note, the unread count the page draws.
func eventsHeaderNote(t *testing.T, dom *pageDOM) string {
	t.Helper()

	return dom.element(t, "eventsnote").Text
}

// eventsSummary is the one line a shut panel shows in place of its note.
func eventsSummary(t *testing.T, dom *pageDOM) string {
	t.Helper()

	return pageExpr(t, dom, `__foldRead("events").summary`)
}

// rejectedAlert is a rejection the server would alert on. It is numbered so each
// one reads apart on the page, and the clock is the test's, not the wall's.
func rejectedAlert(n int, bead string) Alert {
	return Alert{
		ID:    fmt.Sprintf("alert-%d", n),
		At:    alertBase.Add(time.Duration(n) * time.Minute),
		Level: LevelWarn,
		Key:   "reject:" + bead + ":review",
		Title: "a landing was rejected (review)",
		Text:  fmt.Sprintf("reason %d", n),
		Bead:  bead,
	}
}

// streamAlert sends one alert on the live stream, the "alert" event the server
// pushes as each alert is raised.
func streamAlert(t *testing.T, dom *pageDOM, a Alert) {
	t.Helper()

	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("encoding the alert: %v", err)
	}
	deliverStream(t, dom, "alert", string(b))
}

// stateAlerts hands the page a state reading whose recent alerts are the given
// ones, oldest first, the way the hub's state carries them.
func stateAlerts(t *testing.T, dom *pageDOM, alerts ...Alert) {
	t.Helper()

	list := []map[string]any{}
	for _, a := range alerts {
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("encoding the alert: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("encoding the alert: %v", err)
		}
		list = append(list, m)
	}
	dom.call(t, "renderEvents", map[string]any{"alerts": list})
}

// An alert that used to raise a toast adds one line to the EVENTS feed: its time,
// kind, bead and reason. The page draws no overlay for it, so nothing on the page
// has to be closed by hand to reveal another panel.
func TestAnAlertIsAFeedLineAndNoOverlay(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	a := rejectedAlert(1, "gt-x")
	streamAlert(t, dom, a)

	lines := feedLines(t, dom)
	if len(lines) != 1 {
		t.Fatalf("one alert drew %d feed lines, want 1: %+v", len(lines), lines)
	}
	want := []string{pageExpr(t, dom, "clock('"+a.At.Format(time.RFC3339)+"')"), a.Title, a.Bead, a.Text}
	if !slices.Equal(lines[0].Cells, want) {
		t.Errorf("the feed line reads %q, want time, kind, bead and reason %q", lines[0].Cells, want)
	}
	if !strings.Contains(lines[0].Cls, "warn") {
		t.Errorf("the feed line is %q, want the alert's level on it", lines[0].Cls)
	}
	if got := pageExpr(t, dom, "String(__snap('toasts') === null && typeof showToast === 'undefined')"); got != "true" {
		t.Error("the page still draws a toast container or a function that draws one")
	}
	if strings.Contains(string(indexHTML), `id="toasts"`) {
		t.Error("index.html still carries the toast container")
	}
}

// The EVENTS panel starts shut. Its header counts what has arrived since the
// reader last opened it: the count shows in the header and as the summary of the
// shut panel, opening the panel reads it, and an alert that lands while the panel
// is open is read as it arrives.
func TestTheEventsPanelStartsShutAndCountsUnreadUntilOpened(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if !foldClosed(t, dom, "events") {
		t.Fatal("the EVENTS panel starts open, want it shut by default")
	}
	if got := eventsHeaderNote(t, dom); got != "" {
		t.Fatalf("the header reads %q before any alert, want nothing", got)
	}

	streamAlert(t, dom, rejectedAlert(1, "gt-x"))
	streamAlert(t, dom, rejectedAlert(2, "gt-y"))
	if got := eventsHeaderNote(t, dom); got != "2 unread" {
		t.Errorf("the header reads %q with two unread, want %q", got, "2 unread")
	}
	if got := eventsSummary(t, dom); got != "2 unread" {
		t.Errorf("the shut panel's summary reads %q, want the count %q", got, "2 unread")
	}

	pageExpr(t, dom, "__pressFold('events', 'click')")
	if foldClosed(t, dom, "events") {
		t.Fatal("pressing the header did not open the EVENTS panel")
	}
	if got := eventsHeaderNote(t, dom); got != "" {
		t.Errorf("opening the panel left the header at %q, want the count cleared", got)
	}

	streamAlert(t, dom, rejectedAlert(3, "gt-z"))
	if got := eventsHeaderNote(t, dom); got != "" {
		t.Errorf("an alert that lands while the panel is open reads %q, want it read as it lands", got)
	}

	pageExpr(t, dom, "__pressFold('events', 'click')")
	streamAlert(t, dom, rejectedAlert(4, "gt-w"))
	if got := eventsHeaderNote(t, dom); got != "1 unread" {
		t.Errorf("after shutting the panel again the header reads %q, want %q", got, "1 unread")
	}
	if n := len(feedLines(t, dom)); n != 4 {
		t.Errorf("the feed holds %d lines, want all four alerts", n)
	}
}

// A state refresh carries the recent alerts again. The feed takes each one once,
// so an alert the stream already delivered adds no line, a new one is news, and
// the feed keeps the last hundred and no more.
func TestAStateRefreshAddsNoDuplicateAndKeepsTheLastHundred(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	stateAlerts(t, dom) // the first reading is the baseline, so nothing in it is news
	var all []Alert
	for n := 1; n <= 150; n++ {
		all = append(all, rejectedAlert(n, "gt-x"))
		streamAlert(t, dom, all[n-1])
	}

	lines := feedLines(t, dom)
	if len(lines) != 100 {
		t.Fatalf("the feed holds %d lines after 150 alerts, want the last 100", len(lines))
	}
	if lines[0].Cells[3] != "reason 150" || lines[99].Cells[3] != "reason 51" {
		t.Errorf("the feed runs from %q to %q, want newest first from %q to %q",
			lines[0].Cells[3], lines[99].Cells[3], "reason 150", "reason 51")
	}

	// The hub's recent list is the last twenty alerts, all of which the stream
	// has already delivered: a refresh of them adds nothing.
	stateAlerts(t, dom, all[130:]...)
	if again := feedLines(t, dom); len(again) != 100 || again[0].Cells[3] != "reason 150" {
		t.Errorf("a refresh repeating delivered alerts changed the feed to %d lines, newest %q",
			len(again), again[0].Cells[3])
	}

	// A refresh that carries one alert the page has not seen adds that one, at the top.
	stateAlerts(t, dom, append(all[131:], rejectedAlert(151, "gt-x"))...)
	again := feedLines(t, dom)
	if len(again) != 100 || again[0].Cells[3] != "reason 151" || again[99].Cells[3] != "reason 52" {
		t.Errorf("a refresh with one new alert left the feed %d lines, from %q to %q",
			len(again), again[0].Cells[3], again[99].Cells[3])
	}
	seen := map[string]bool{}
	for _, l := range again {
		if seen[l.Cells[3]] {
			t.Fatalf("the feed holds %q twice", l.Cells[3])
		}
		seen[l.Cells[3]] = true
	}
	if got := eventsHeaderNote(t, dom); got != "151 unread" {
		t.Errorf("the header reads %q, want the 151 alerts that came in unread", got)
	}
}

// A page that connects to a town with alerts already raised shows them in the
// feed without counting them as unread: they were raised before the page opened,
// so they are history, not news.
func TestAlertsRaisedBeforeThePageOpenedAreReadNotUnread(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	stateAlerts(t, dom, rejectedAlert(1, "gt-x"), rejectedAlert(2, "gt-y"))

	if n := len(feedLines(t, dom)); n != 2 {
		t.Fatalf("the feed holds %d lines for the two alerts the state carried, want 2", n)
	}
	if got := eventsHeaderNote(t, dom); got != "" {
		t.Errorf("the header reads %q for alerts raised before the page opened, want no count", got)
	}
}

// A page outlives the hub it reads: the hub is restarted under it and the new
// process numbers its alerts from one again. The page shows an alert once by id,
// so each hub names its alerts with its own instance; a restart that reused the
// old ids would have the page take the new hub's first alerts for repeats and
// drop them (gt-wpgga).
func TestAlertsFromARestartedHubAreNotTakenForRepeats(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	ev := wakeEvent(attention.RuleSlotHeld, "slot:gastown", "slot held 3h", attention.SeverityHigh)
	for i, hub := range []*Alerter{NewAlerter(nil), NewAlerter(nil)} {
		raised, _ := hub.ObserveAttention([]attention.Event{ev}, attention.Acks{})
		if len(raised) != 1 {
			t.Fatalf("hub %d raised %d alerts for one wake line, want 1", i, len(raised))
		}
		streamAlert(t, dom, raised[0])
	}

	lines := feedLines(t, dom)
	if len(lines) != 2 {
		t.Fatalf("the feed holds %d lines for one alert from each of two hubs, want 2: %+v", len(lines), lines)
	}
}
