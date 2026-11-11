package dashboard

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A source the server has never read is stamped with a zero time, and a pane
// that subtracts that from now draws an age in centuries ("read 739930d ago").
// The page's read ages ask one decider, neverRead: an empty stamp or one before
// the year 2000 is "never read" (gt-6qc3nw.14).

// neverStamp is the zero time the server sends for a read that has not happened.
const neverStamp = "0001-01-01T00:00:00Z"

// freshStamp is a read taken four minutes ago. It is not the instant of the test:
// the page's clock is read once when the stub loads, so a stamp taken after
// that instant would draw a negative age.
func freshStamp() string {
	return time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339)
}

// absurdDays finds any age the page drew in days above a year.
var absurdDays = regexp.MustCompile(`\b(\d+)d\b`)

func assertNoAbsurdAge(t *testing.T, what, text string) {
	t.Helper()
	for _, m := range absurdDays.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 365 {
			t.Errorf("%s draws an age of %s in %q", what, m[0], text)
		}
	}
	if strings.Contains(text, "739930") {
		t.Errorf("%s draws the zero stamp's age in %q", what, text)
	}
}

// TestPageNeverReadAge draws the panes that print a read's age (the Cost header,
// Machine, News and the Polecats seats note) with a read that never happened,
// and again with one taken four minutes ago. The never read says "never read"
// and no age; the fresh one says its age.
func TestPageNeverReadAge(t *testing.T) {
	t.Parallel()

	panes := []struct {
		name string
		draw func(t *testing.T, dom *pageDOM, stamp string)
		note string
	}{
		{
			name: "COST header",
			note: "costnote",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				dom.call(t, "renderCost", map[string]any{
					"spend": liveSpend(),
					"reads": map[string]any{"spend": map[string]any{"at": stamp, "every_sec": 900}},
				})
			},
		},
		{
			name: "Machine",
			note: "machnote",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				dom.call(t, "renderMachine", map[string]any{
					"machine": map[string]any{},
					"reads":   map[string]any{"machine": map[string]any{"at": stamp, "every_sec": 60}},
				})
			},
		},
		{
			name: "News",
			note: "newsnote",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				// A story keeps the caption drawn: a pane with no stories clears it.
				story := map[string]any{"rank": 1, "title": "a story", "url": "https://example.com/1", "points": 1, "comments": 0, "id": 1}
				dom.call(t, "renderNews", map[string]any{
					"news": map[string]any{"at": stamp, "stories": []any{story}},
				})
			},
		},
		{
			name: "Polecats seats",
			note: "polecatsnote",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				dom.call(t, "renderPolecats", map[string]any{
					"summary_at": stamp,
					"polecats":   []any{},
					"reads":      map[string]any{"seats": map[string]any{"at": stamp, "every_sec": 900}},
				})
			},
		},
	}

	for _, pane := range panes {
		t.Run(pane.name+" never read", func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			pane.draw(t, dom, neverStamp)
			got := dom.element(t, pane.note).Text
			if !strings.Contains(got, "never read") {
				t.Errorf("a read that never happened draws %q, want \"never read\"", got)
			}
			assertNoAbsurdAge(t, pane.name, got)
		})

		t.Run(pane.name+" fresh read", func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			pane.draw(t, dom, freshStamp())
			got := dom.element(t, pane.note).Text
			if !strings.Contains(got, "read 4m ago") {
				t.Errorf("a read taken four minutes ago draws %q, want its age \"read 4m ago\"", got)
			}
			if strings.Contains(got, "never read") {
				t.Errorf("a read that has happened draws %q as never read", got)
			}
		})
	}
}

// TestPageNeverReadDecidesByStamp pins the decider's edges: an empty stamp and
// one a second before the year 2000 are never read, and the first instant of
// 2000 is an old read with an age.
func TestPageNeverReadDecidesByStamp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, stamp string
		never       bool
	}{
		{"empty", "", true},
		{"a second before 2000", "1999-12-31T23:59:59Z", true},
		{"the first instant of 2000", "2000-01-01T00:00:00Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderCost", map[string]any{
				"spend": liveSpend(),
				"reads": map[string]any{"spend": map[string]any{"at": tc.stamp, "every_sec": 900}},
			})
			got := dom.element(t, "costnote").Text
			if isNever := strings.Contains(got, "never read"); isNever != tc.never {
				t.Errorf("a read stamped %q draws %q, want never read = %v", tc.stamp, got, tc.never)
			}
		})
	}
}

// TestPageNeverReadKeepsFailureMark: a source that has never been read and has
// failed draws "never read" beside the failed-read text and its reason, and no
// output carries an age in days above a year.
func TestPageNeverReadKeepsFailureMark(t *testing.T) {
	t.Parallel()

	const reason = "the spend report could not be read"

	t.Run("COST header", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderCost", map[string]any{
			"spend":          liveSpend(),
			"reads":          map[string]any{"spend": map[string]any{"at": neverStamp, "every_sec": 900, "error": true, "stale": true}},
			"cost_providers": []any{map[string]any{"id": "deepseek", "label": "DeepSeek", "kind": "cash", "why": reason}},
		})
		got := dom.element(t, "costnote").Text
		if want := "never read · read failed: " + reason; !strings.Contains(got, want) {
			t.Errorf("the header draws %q, want %q", got, want)
		}
		assertNoAbsurdAge(t, "COST header", got)
	})

	t.Run("Polecats seats", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderPolecats", map[string]any{
			"summary_at": neverStamp,
			"summary":    map[string]any{"seats_why": reason},
			"polecats":   []any{},
			"reads":      map[string]any{"seats": map[string]any{"at": neverStamp, "every_sec": 900, "error": true, "stale": true}},
		})
		got := dom.element(t, "polecatsnote").Text
		if want := "seats never read · the seats read failed: " + reason; !strings.Contains(got, want) {
			t.Errorf("the polecats note draws %q, want %q", got, want)
		}
		assertNoAbsurdAge(t, "Polecats seats", got)
	})

	t.Run("Machine", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderMachine", map[string]any{
			"machine": map[string]any{},
			"reads":   map[string]any{"machine": map[string]any{"at": neverStamp, "every_sec": 60, "error": true, "stale": true}},
		})
		got := dom.element(t, "machnote").Text
		if want := "never read · read failed"; !strings.Contains(got, want) {
			t.Errorf("the machine caption draws %q, want %q", got, want)
		}
		assertNoAbsurdAge(t, "Machine", got)
	})
}
