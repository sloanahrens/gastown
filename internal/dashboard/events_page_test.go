package dashboard

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
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

	js := `JSON.stringify(__byId["events"].children.__arr.map(r => ({
		cls: r.className,
		cells: r.children.__arr.map(c => c.textContent)})))`
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

// chipLine is one of the stats chips the EVENTS header draws: its class, which
// carries the level's colour, and the words on it.
type chipLine struct {
	Cls  string `json:"cls"`
	Text string `json:"text"`
}

// eventChips reads the chips the EVENTS header holds, in the order the page
// drew them.
func eventChips(t *testing.T, dom *pageDOM) []chipLine {
	t.Helper()

	js := `JSON.stringify(__byId["eventschips"].children.__arr.map(c => ({cls: c.className, text: c.textContent})))`
	var out []chipLine
	if err := json.Unmarshal([]byte(pageExpr(t, dom, js)), &out); err != nil {
		t.Fatalf("reading the EVENTS header's chips: %v", err)
	}
	return out
}

// pageNow is the clock the page reads, so a test dates an alert against it and
// the header's age comes out exact rather than near.
func pageNow(t *testing.T, dom *pageDOM) time.Time {
	t.Helper()

	ms, err := strconv.ParseInt(pageExpr(t, dom, "String(Date.now())"), 10, 64)
	if err != nil {
		t.Fatalf("reading the page's clock: %v", err)
	}
	return time.UnixMilli(ms).UTC()
}

// alertAt redates an alert to at and gives it the level the header counts it
// under.
func alertAt(a Alert, at time.Time, level string) Alert {
	a.At = at
	a.Level = level
	return a
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

// newestFirst is the order the hub serves its recent alerts in. A test that
// builds a list oldest first hands the page the reverse of it.
func newestFirst(alerts []Alert) []Alert {
	out := make([]Alert, 0, len(alerts))
	for i := len(alerts) - 1; i >= 0; i-- {
		out = append(out, alerts[i])
	}
	return out
}

// stateAlerts hands the page a state reading whose recent alerts are the given
// ones, in the order given, which is the order the hub serves them: newest first.
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
	stateAlerts(t, dom, newestFirst(all[130:])...)
	if again := feedLines(t, dom); len(again) != 100 || again[0].Cells[3] != "reason 150" {
		t.Errorf("a refresh repeating delivered alerts changed the feed to %d lines, newest %q",
			len(again), again[0].Cells[3])
	}

	// A refresh that carries one alert the page has not seen adds that one, at the top.
	stateAlerts(t, dom, append([]Alert{rejectedAlert(151, "gt-x")}, newestFirst(all[131:])...)...)
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
	stateAlerts(t, dom, rejectedAlert(2, "gt-y"), rejectedAlert(1, "gt-x"))

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

// The EVENTS header says how busy and how bad the last hour has been: the
// events of each level inside the hour, then how long ago the newest event
// arrived, which is the feed's first row. The count is taken from the alerts the
// feed holds, so an event older than the hour is in the feed and out of the
// count, and one the page cannot date is in neither (gt-y3fau).
func TestEventsStatsCountTheLastHourByLevelAndAgeTheNewest(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	ev := func(id, level, when string) map[string]any {
		return map[string]any{"id": id, "at": when, "level": level, "title": "a thing"}
	}
	none := map[string]int{"info": 0, "warn": 0, "crit": 0}

	cases := []struct {
		name  string
		lines []map[string]any
		want  map[string]int
		age   string
	}{
		{
			name:  "the hour's events are counted by level",
			lines: []map[string]any{ev("a", "crit", at(5*time.Minute)), ev("b", "warn", at(30*time.Minute)), ev("c", "warn", at(50*time.Minute))},
			want:  map[string]int{"info": 0, "warn": 2, "crit": 1},
			age:   "5m",
		},
		{
			name:  "an event older than the hour is not counted",
			lines: []map[string]any{ev("c", "info", at(10*time.Minute)), ev("a", "warn", at(61*time.Minute)), ev("b", "crit", at(90*time.Minute))},
			want:  map[string]int{"info": 1, "warn": 0, "crit": 0},
			age:   "10m",
		},
		{
			name:  "the hour ends at exactly an hour: that event counts and the second past it does not",
			lines: []map[string]any{ev("a", "info", at(30*time.Minute)), ev("b", "warn", at(time.Hour)), ev("c", "crit", at(time.Hour+time.Second))},
			want:  map[string]int{"info": 1, "warn": 1, "crit": 0},
			age:   "30m",
		},
		{
			name:  "a level the page has no colour for counts as info, as its row draws",
			lines: []map[string]any{ev("a", "bad", at(5*time.Minute))},
			want:  map[string]int{"info": 1, "warn": 0, "crit": 0},
			age:   "5m",
		},
		{
			name:  "an event with no readable time is counted in neither the window nor the age",
			lines: []map[string]any{{"id": "a", "at": "not a time", "level": "warn"}},
			want:  none,
			age:   "–",
		},
		{
			name:  "an event dated ahead of the clock is not an age before now",
			lines: []map[string]any{ev("a", "crit", at(-5*time.Minute))},
			want:  map[string]int{"info": 0, "warn": 0, "crit": 1},
			age:   "0s",
		},
		{
			name:  "an empty feed has nothing to count and no newest",
			lines: nil,
			want:  none,
			age:   "–",
		},
	}

	dom := newPageDOM(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.lines)
			if err != nil {
				t.Fatalf("encoding the alerts: %v", err)
			}
			js := fmt.Sprintf(`(function(){ const st = eventStats(%s, %d); return JSON.stringify({counts: st.counts, age: fmtDur(st.ageSec)}); })()`,
				b, now.UnixMilli())
			var got struct {
				Counts map[string]int `json:"counts"`
				Age    string         `json:"age"`
			}
			if err := json.Unmarshal([]byte(pageExpr(t, dom, js)), &got); err != nil {
				t.Fatalf("reading eventStats: %v", err)
			}
			for lvl, n := range tc.want {
				if got.Counts[lvl] != n {
					t.Errorf("eventStats counted %d %s events, want %d: %+v", got.Counts[lvl], lvl, n, got.Counts)
				}
			}
			if got.Age != tc.age {
				t.Errorf("the newest event reads %q, want %q", got.Age, tc.age)
			}
		})
	}
}

// The stats ride the EVENTS heading beside the unread count, so a shut panel —
// how the panel starts — still says what the last hour brought. They redraw
// when an alert lands on the stream and when a state reading carries one in,
// with no timer of their own (gt-y3fau).
func TestTheEventsHeaderShowsTheLastHourStatsBesideTheUnreadCount(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if !foldClosed(t, dom, "events") {
		t.Fatal("the EVENTS panel starts open, want it shut")
	}
	now := pageNow(t, dom)

	// One alert from a reading, raised before the page opened, so it is history
	// rather than news.
	stateAlerts(t, dom, alertAt(rejectedAlert(1, "gt-old"), now.Add(-3*time.Hour), LevelWarn))

	// Three from the stream, inside the hour, one of each level.
	streamAlert(t, dom, alertAt(rejectedAlert(2, "gt-a"), now.Add(-20*time.Minute), LevelInfo))
	streamAlert(t, dom, alertAt(rejectedAlert(3, "gt-b"), now.Add(-5*time.Minute), LevelWarn))
	streamAlert(t, dom, alertAt(rejectedAlert(4, "gt-c"), now.Add(-10*time.Minute), LevelCrit))
	// The hub sends the state behind each alert, with the recent list in the
	// order it serves: newest first. The NEWEST age reads that list's first row.
	stateAlerts(t, dom,
		alertAt(rejectedAlert(3, "gt-b"), now.Add(-5*time.Minute), LevelWarn),
		alertAt(rejectedAlert(4, "gt-c"), now.Add(-10*time.Minute), LevelCrit),
		alertAt(rejectedAlert(2, "gt-a"), now.Add(-20*time.Minute), LevelInfo),
		alertAt(rejectedAlert(1, "gt-old"), now.Add(-3*time.Hour), LevelWarn))

	want := []chipLine{
		{Cls: "echip info", Text: "1 info"},
		{Cls: "echip warn", Text: "1 warn"},
		{Cls: "echip crit", Text: "1 crit"},
		{Cls: "echip", Text: "newest 5m ago"},
	}
	if got := eventChips(t, dom); !slices.Equal(got, want) {
		t.Errorf("the header's chips read %+v, want the hour's three levels and the newest age %+v", got, want)
	}
	// The three streamed alerts are unread while the panel is shut, and the
	// header says so beside the chips.
	if got := eventsSummary(t, dom); got != "3 unread" {
		t.Errorf("the shut panel's summary reads %q with the chips, want the unread count %q", got, "3 unread")
	}
	// The chips sit in the heading, which stays while the panel is shut, and not
	// among the rows the fold hides.
	if c := dom.element(t, "eventschips"); c.Hidden {
		t.Error("the chips are hidden while the panel is shut, want them in the header")
	}
	if got := pageExpr(t, dom, `__byId["eventschips"].parentNode.tagName`); got != "H2" {
		t.Errorf("the chips sit under a %s, want the panel's heading", got)
	}
	if got := pageExpr(t, dom, `String(__byId["events"].textContent.indexOf("newest"))`); got != "-1" {
		t.Error("the stats are drawn in the panel's body, want them in the header alone")
	}

	// A reading that carries a newer alert in redraws the stats it is made of.
	stateAlerts(t, dom, alertAt(rejectedAlert(5, "gt-d"), now.Add(-2*time.Minute), LevelCrit))
	want = []chipLine{
		{Cls: "echip info", Text: "1 info"},
		{Cls: "echip warn", Text: "1 warn"},
		{Cls: "echip crit", Text: "2 crit"},
		{Cls: "echip", Text: "newest 2m ago"},
	}
	if got := eventChips(t, dom); !slices.Equal(got, want) {
		t.Errorf("after a reading with a newer alert the chips read %+v, want %+v", got, want)
	}
}

// A feed with nothing in it has nothing to say about the hour, so the header
// draws no chips at all (gt-y3fau).
func TestTheEventsHeaderShowsNoStatsForAnEmptyFeed(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	stateAlerts(t, dom) // a reading that carries no alerts, so the page paints the empty feed

	if chips := eventChips(t, dom); len(chips) != 0 {
		t.Errorf("an empty feed drew %d stat chips, want none: %+v", len(chips), chips)
	}

	// The control: the same header paints chips once a reading carries an alert
	// in the hour, so a header that never drew any fails the empty-feed check
	// rather than passing it.
	stateAlerts(t, dom, alertAt(rejectedAlert(1, "gt-a"), pageNow(t, dom).Add(-time.Minute), LevelWarn))
	if chips := eventChips(t, dom); len(chips) == 0 {
		t.Error("a feed with an alert in the hour drew no chips, so the empty-feed check above passes for a header that draws none at all")
	}
}

// The EVENTS feed draws the recent alerts in the order the hub serves them, and
// the NEWEST header reads the first row of that order. The pair here is served
// oldest first, which the hub never does: a page that sorted the list would draw
// it the other way round, and a header that took the newest of the rows would
// read the newer one instead of the first.
func TestAlertsNewestHeaderMatchesFirstRow(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	now := pageNow(t, dom)
	older := alertAt(rejectedAlert(1, "gt-old"), now.Add(-30*time.Minute), LevelWarn)
	newer := alertAt(rejectedAlert(2, "gt-new"), now.Add(-5*time.Minute), LevelWarn)
	stateAlerts(t, dom, older, newer)

	lines := feedLines(t, dom)
	if len(lines) != 2 {
		t.Fatalf("the feed holds %d lines for two served alerts, want 2", len(lines))
	}
	if lines[0].Cells[3] != older.Text || lines[1].Cells[3] != newer.Text {
		t.Errorf("the feed draws %q then %q, want the served order %q then %q",
			lines[0].Cells[3], lines[1].Cells[3], older.Text, newer.Text)
	}
	want := []chipLine{
		{Cls: "echip warn", Text: "2 warn"},
		{Cls: "echip", Text: "newest 30m ago"},
	}
	if got := eventChips(t, dom); !slices.Equal(got, want) {
		t.Errorf("the header's chips read %+v, want the first row's age %+v", got, want)
	}
}

// The EVENTS rows' time cell never wraps: the column it sat in was 62px, less
// than the page's monospace needs for "14:28:28", so the operator's screenshot
// showed one stamp over two lines ("14:28:2" above "8") (gt-y3fau). The harness
// has no layout engine, so the check is the cell the page draws the stamp in
// and the declarations the row and that cell carry.
func TestTheEventsTimeCellNeverWraps(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	stateAlerts(t, dom, alertAt(rejectedAlert(1, "gt-a"), pageNow(t, dom).Add(-time.Minute), LevelWarn))

	row, stamp := drawnStampCell(t, dom)
	if !slices.Contains(strings.Fields(row), "evrow") {
		t.Fatalf("the alert drew in a row classed %q, want an .evrow", row)
	}
	if !slices.Contains(strings.Fields(stamp), "t") {
		t.Fatalf("the row drew its stamp in a cell classed %q, want .t among its classes, the cell the no-wrap rule styles", stamp)
	}

	// The grid's first track holds the stamp, so it takes the stamp's width.
	grid := declOf(t, topRule(t, ".evrow"), "grid-template-columns")
	if first, _, _ := strings.Cut(grid, " "); first != "max-content" {
		t.Errorf("the .evrow rule sizes its stamp column %q, want the first track max-content so the column is as wide as the stamp", grid)
	}
	if got := stampCellDecls(t)["white-space"]; got != "nowrap" {
		t.Errorf("the rules that style the stamp cell set white-space %q, want nowrap so the stamp stays on one line", got)
	}
}

// stampCellDecls returns the declarations the top-level rules set on an EVENTS
// row's stamp cell, keyed by property, later rules last. A rule applies to the
// cell when its subject names .t and its ancestor, if it has one, names .evrow,
// so the check reads the rules the page applies to the drawn cell and not only
// the one spelled .evrow .t.
func stampCellDecls(t *testing.T) map[string]string {
	t.Helper()

	decls := map[string]string{}
	for _, rule := range strings.Split(withoutMediaBlocks(pageStyle(t)), "}") {
		sel, body, ok := strings.Cut(rule, "{")
		if !ok || !slices.ContainsFunc(strings.Split(sel, ","), appliesToStampCell) {
			continue
		}
		for _, decl := range strings.Split(body, ";") {
			if prop, val, ok := strings.Cut(decl, ":"); ok {
				decls[strings.TrimSpace(prop)] = strings.TrimSpace(val)
			}
		}
	}
	return decls
}

// appliesToStampCell reports whether one selector reaches the stamp cell: its
// last compound names .t, and either it has no ancestor or an ancestor names
// .evrow.
func appliesToStampCell(one string) bool {
	fields := strings.Fields(one)
	if len(fields) == 0 || !selectsClass(fields[len(fields)-1], "t") {
		return false
	}
	if len(fields) == 1 {
		return true
	}
	for _, anc := range fields[:len(fields)-1] {
		if selectsClass(anc, "evrow") {
			return true
		}
	}
	return false
}

// drawnStampCell is the first row the EVENTS feed drew and the class of the cell
// holding its stamp.
func drawnStampCell(t *testing.T, dom *pageDOM) (row, stamp string) {
	t.Helper()

	js := `(function () {
		var r = __byId["events"].children[0];
		return r ? r.className + "\u0000" + r.children[0].className : "";
	})()`
	row, stamp, ok := strings.Cut(pageExpr(t, dom, js), "\u0000")
	if !ok {
		t.Fatal("the EVENTS feed drew no row to read the stamp cell from")
	}
	return row, stamp
}
