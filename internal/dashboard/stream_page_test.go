package dashboard

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The page keeps its own stream alive (gt-rpble): the operator's tab sat on
// "reconnecting" for hours after an install while /api/state and the server were
// fresh, because a browser that has closed a stream for good never retries it
// and never notices one that stays open and sends nothing. The tests below run
// index.html's own script against the stream stub and the fake clock in
// page_harness_test.go and drive each stream by hand, so the reconnect and the
// watchdog are checked through the page rather than by matching its source
// (gt-tq4ee).

// pageExpr runs one expression over a loaded page and reads back its value.
func pageExpr(t *testing.T, dom *pageDOM, expr string) string {
	t.Helper()

	v, err := dom.vm.RunString(expr)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return v.String()
}

// pageNumber runs one expression over a loaded page and reads it as a number.
func pageNumber(t *testing.T, dom *pageDOM, expr string) int {
	t.Helper()

	n, err := strconv.Atoi(pageExpr(t, dom, expr))
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return n
}

// pageStreams is how many streams the page has opened: a replacement is a new
// EventSource, so the count rises by one per reconnect.
func pageStreams(t *testing.T, dom *pageDOM) int {
	t.Helper()
	return pageNumber(t, dom, "__sources.length")
}

// liveStream is the index of the stream the page is on now, the one a browser
// would be delivering events to.
const liveStream = "__sources.length - 1"

// The stream stub stands in for the browser's EventSource, so its readyState
// values are the browser's. The page compares a stream against
// EventSource.CLOSED to tell one the browser has given up on from one it is
// still connecting, and a stub that renumbered the states would describe a page
// that never reconnects while the browser's page does (gt-rpble).
func TestTheStreamStubCarriesTheBrowsersStreamStates(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if got := pageExpr(t, dom, "[EventSource.CONNECTING, EventSource.OPEN, EventSource.CLOSED].join()"); got != "0,1,2" {
		t.Fatalf("the stream stub's states read %q, want the browser's 0,1,2", got)
	}
}

// failStream fails the live stream, the browser calling its onerror once the
// stream has reached readyState.
func failStream(t *testing.T, dom *pageDOM, readyState string) {
	t.Helper()

	pageExpr(t, dom, "__error(__sources["+liveStream+"], "+readyState+")")
}

// openStream opens the live stream, the browser calling its onopen when the
// server accepts it.
func openStream(t *testing.T, dom *pageDOM) {
	t.Helper()

	pageExpr(t, dom, "__open(__sources["+liveStream+"])")
}

// deliverStream sends one named event on the live stream, the reading the
// server would push.
func deliverStream(t *testing.T, dom *pageDOM, name, data string) {
	t.Helper()

	pageExpr(t, dom, "__fire(__sources["+liveStream+"], "+strconv.Quote(name)+", "+data+")")
}

// advanceTimers moves the page's clock by ms, running the timers that come due
// on the way.
func advanceTimers(t *testing.T, dom *pageDOM, ms int) {
	t.Helper()

	pageExpr(t, dom, "__advance("+strconv.Itoa(ms)+")")
}

// pageReloads is how many times the page has gone back to the server for a
// fresh build's page.
func pageReloads(t *testing.T, dom *pageDOM) int {
	t.Helper()
	return pageNumber(t, dom, "__reloads")
}

// A stream the browser has given up on is the page's to replace. WebKit does
// exactly this after a failed reconnect, and the delay doubles while connects
// keep failing so a server that is down is not hammered (gt-rpble).
func TestAClosedStreamIsReplacedOnABackoffCappedAtFifteenSeconds(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if got := pageStreams(t, dom); got != 1 {
		t.Fatalf("the page opened %d streams at load, want the one connect() makes", got)
	}

	// Each round fails the live stream and waits out its delay: no new stream
	// before then, one the moment the delay is up.
	for _, wait := range []int{2000, 4000, 8000, 15000, 15000} {
		before := pageStreams(t, dom)
		failStream(t, dom, "EventSource.CLOSED")

		advanceTimers(t, dom, wait-1)
		if got := pageStreams(t, dom); got != before {
			t.Fatalf("the page had %d streams %d ms into a %d ms wait, want %d", got, wait-1, wait, before)
		}
		advanceTimers(t, dom, 1)
		if got := pageStreams(t, dom); got != before+1 {
			t.Fatalf("the page had %d streams %d ms after a failure, want the replacement after %d ms", got, wait, wait)
		}
	}

	// The chip names what the page is doing while it waits.
	if d := dom.element(t, "conn"); d.Text != "reconnecting" || d.Class != "off" {
		t.Errorf("#conn reads %q in class %q after repeated failures, want the reconnecting label", d.Text, d.Class)
	}
}

// A stream the browser is still connecting comes back on its own, so the page
// leaves it alone: two reconnects — the browser's and the page's — would give an
// operator two streams to reason about (gt-rpble).
func TestAStreamTheBrowserIsStillConnectingIsLeftToIt(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	failStream(t, dom, "EventSource.CONNECTING")

	advanceTimers(t, dom, 60000)
	if got := pageStreams(t, dom); got != 1 {
		t.Errorf("the page opened %d streams while the browser was retrying the stream, want it left to the browser", got)
	}
}

// A stream that reopens after a drop may be a new build's page, so the page
// takes it — whether the browser retried the stream or the page made a new one
// (gt-rpble).
func TestAStreamThatReopensAfterADropTakesTheNewBuildsPage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fail string
	}{
		{"the browser retries the stream", "EventSource.CONNECTING"},
		{"the page replaces the stream", "EventSource.CLOSED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			failStream(t, dom, tc.fail)
			advanceTimers(t, dom, 2000) // the page's own reconnect, if it is making one
			openStream(t, dom)
			if got := pageReloads(t, dom); got != 1 {
				t.Errorf("the page reloaded %d times when a dropped stream reopened, want the new build's page", got)
			}
		})
	}
}

// A stream can be open and alive and still send nothing, which no browser
// notices. The watchdog replaces it after 90 s without a named event, and the
// state the page already holds stands until the replacement's snapshot arrives
// (gt-rpble).
func TestTheWatchdogReplacesAStreamQuietForNinetySeconds(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	openStream(t, dom)           // the stream is live and talking, and then stops talking
	advanceTimers(t, dom, 80000) // eight watchdog ticks, 80 s of silence
	if got := pageStreams(t, dom); got != 1 {
		t.Fatalf("the page had %d streams after 80 s of silence, want the stream left alone", got)
	}

	advanceTimers(t, dom, 10000) // the ninth tick
	if got := pageStreams(t, dom); got != 2 {
		t.Fatalf("the page had %d streams after 90 s of silence, want the stream replaced", got)
	}
	if got := pageNumber(t, dom, "__sources[0].readyState"); got != 2 {
		t.Errorf("the replaced stream's readyState is %d, want it closed", got)
	}
	// The watchdog drops the stream like the browser would have: the chip stops
	// claiming a stream it no longer has, and the stream that opens next takes
	// the new build's page (gt-3q3go).
	if d := dom.element(t, "conn"); d.Text != "reconnecting" || d.Class != "off" {
		t.Errorf("#conn reads %q in class %q after the swap, want the drop the watchdog made", d.Text, d.Class)
	}
	openStream(t, dom)
	if got := pageReloads(t, dom); got != 1 {
		t.Errorf("the page reloaded %d times when the watchdog's stream opened, want the new build's page", got)
	}
}

// A connect that works clears the backoff, and it clears it before the reload a
// dropped stream takes the page on: the reload ends the page in a browser, so a
// reset that came after it would never run (gt-3q3go).
func TestAStreamThatOpensAfterADropClearsTheBackoff(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	failStream(t, dom, "EventSource.CLOSED") // the delay doubles from here
	advanceTimers(t, dom, 2000)
	failStream(t, dom, "EventSource.CLOSED")
	advanceTimers(t, dom, 4000)
	openStream(t, dom) // the new build's page, and the reset the drop would have hidden

	// The next failure waits the base delay again, not the doubled one: 4 s here
	// would mean the reset never ran.
	failStream(t, dom, "EventSource.CLOSED")
	advanceTimers(t, dom, 1999)
	if got := pageStreams(t, dom); got != 3 {
		t.Fatalf("the page had %d streams 1999 ms into a 2 s wait, want the wait after the reset", got)
	}
	advanceTimers(t, dom, 1)
	if got := pageStreams(t, dom); got != 4 {
		t.Fatalf("the page had %d streams 2 s after the failure, want the replacement the reset timed", got)
	}
}

// A named event is the server saying it is alive, so it restarts the watchdog's
// clock: a stream that is talking is never replaced, whatever the last event
// was (gt-rpble).
func TestANamedEventRestartsTheWatchdog(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	advanceTimers(t, dom, 89000)
	deliverStream(t, dom, "backlog", "[]")

	advanceTimers(t, dom, 2000) // the ninth tick falls here, the one silence from load would have used
	if got := pageStreams(t, dom); got != 1 {
		t.Fatalf("the page had %d streams after an event 89 s in, want the replacement clock reset", got)
	}

	// The replacement comes 90 s after that event, not at the next tick.
	advanceTimers(t, dom, 88000)
	if got := pageStreams(t, dom); got != 1 {
		t.Fatalf("the page had %d streams 90 s after the event, want the stream left alone", got)
	}
	advanceTimers(t, dom, 1000)
	if got := pageStreams(t, dom); got != 2 {
		t.Fatalf("the page had %d streams 91 s after the event, want the stream replaced", got)
	}
}

// deliverFrames sends each frame in frames, as the server wrote them, down the
// live stream, in the order they were written.
func deliverFrames(t *testing.T, dom *pageDOM, frames string) {
	t.Helper()

	for _, f := range strings.Split(strings.TrimSuffix(frames, "\n\n"), "\n\n") {
		if f == "" {
			continue
		}
		head, data, ok := strings.Cut(f, "\ndata: ")
		if !ok {
			t.Fatalf("a frame with no data line: %q", f)
		}
		deliverStream(t, dom, strings.TrimPrefix(head, "event: "), data)
	}
}

// The page draws the same Landings and Work rows from the hub's list events and
// state frame as from one whole state frame: on connect, and after a change that
// moves one list while the other is left out of the frames (gt-6ecd0).
func TestStreamListEventsDrawTheSameRowsAsTheWholeState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	ready := []QueueBead{{ID: "gt-1", Rig: "gastown", Title: "first ready bead", Priority: 2, Shape: "ok"}}
	landed := []LandingRow{{At: now.Add(-time.Minute), Bead: "gt-9", Rig: "gastown", Title: "landed bead", Outcome: "landed"}}
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Trend: func() *Trend {
			return &Trend{Hours: []TrendHour{{Hour: now.Truncate(time.Hour), Landed: len(landed)}}, Recent: landed}
		},
		Queue: func() *Queue { return &Queue{At: now, Ready: ready, ReadyTotal: len(ready)} },
	})
	h.pollTrend()
	h.pollQueue()

	page, initial := h.Subscribe()
	defer h.Unsubscribe(page)
	var first strings.Builder
	for _, f := range initial {
		first.Write(f)
	}

	split := newPageDOM(t)
	deliverFrames(t, split, first.String())
	whole := newPageDOM(t)
	deliverStream(t, whole, "state", mustJSONString(t, h.State()))

	assertSameDrawing := func(when string) {
		t.Helper()
		for _, id := range []string{"trend", "work"} {
			assert.Equal(t, whole.element(t, id), split.element(t, id), "#%s draws differently from the whole state %s", id, when)
		}
	}
	assertSameDrawing("on connect")
	require.Contains(t, split.element(t, "work").Text, "gt-1", "the Work pane drew no queue row to compare")
	require.Contains(t, split.element(t, "trend").Text, "landed bead", "the Landings pane drew no landing to compare")

	// Only the queue changes, so only queue.ready goes out. The page must keep the
	// landings it already holds, and draw the new queue row beside them.
	ready = append(ready, QueueBead{ID: "gt-2", Rig: "gastown", Title: "second ready bead", Priority: 2, Shape: "ok"})
	h.pollQueue()
	deliverFrames(t, split, drainFrames(page))
	deliverStream(t, whole, "state", mustJSONString(t, h.State()))

	assertSameDrawing("after the queue changed")
	require.Contains(t, split.element(t, "work").Text, "gt-2", "the Work pane did not draw the changed queue")
	require.Contains(t, split.element(t, "trend").Text, "landed bead", "the Landings pane lost its rows when only the queue changed")
}
