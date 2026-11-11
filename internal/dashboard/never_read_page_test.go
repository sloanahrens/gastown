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
// The page's read ages ask one decider, neverRead: an empty stamp, one that does
// not parse, or one before the year 2000 is "never read" (gt-6qc3nw.14).

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

// assertNoBadAge fails on an age the page could not have drawn honestly: a day
// count above a year, the zero stamp's age, NaN or Infinity.
func assertNoBadAge(t *testing.T, what, text string) {
	t.Helper()
	for _, m := range absurdDays.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 365 {
			t.Errorf("%s draws an age of %s in %q", what, m[0], text)
		}
	}
	if strings.Contains(text, "739930") {
		t.Errorf("%s draws the zero stamp's age in %q", what, text)
	}
	if strings.Contains(text, "NaN") || strings.Contains(text, "Infinity") {
		t.Errorf("%s draws %q, which has no age to show", what, text)
	}
}

// agePane is one line on the page that draws a read's age: how it is drawn for a
// read that never happened, the words a fresh read's age follows, and how to
// draw it and read it back.
type agePane struct {
	name  string
	never string
	fresh string
	draw  func(t *testing.T, dom *pageDOM, stamp string)
	text  func(t *testing.T, dom *pageDOM) string
}

// costUsageState is the usage reading the data-quality line is drawn from, with
// the stamp of the scan under test.
func costUsageState(stamp string) map[string]any {
	return map[string]any{
		"spend":          spendReading(),
		"cost_providers": []any{},
		"deepreview":     map[string]any{"schedules": []any{}},
		"usage": map[string]any{
			"window":           "24h",
			"by_class_model":   []any{},
			"deep_review_runs": []any{},
			"top_beads":        []any{},
			"over20":           []any{},
			"as_of":            stamp,
		},
	}
}

func agePanes() []agePane {
	// A story keeps the caption drawn: a pane with no stories clears it.
	story := map[string]any{"rank": 1, "title": "a story", "url": "https://example.com/1", "points": 1, "comments": 0, "id": 1}
	return []agePane{
		{
			name:  "COST header",
			never: "never read",
			fresh: "read ",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				dom.call(t, "renderCost", map[string]any{
					"spend": liveSpend(),
					"reads": map[string]any{"spend": map[string]any{"at": stamp, "every_sec": 900}},
				})
			},
			text: func(t *testing.T, dom *pageDOM) string { return dom.element(t, "costnote").Text },
		},
		{
			name:  "Machine",
			never: "never read",
			fresh: "read ",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				dom.call(t, "renderMachine", map[string]any{
					"machine": map[string]any{},
					"reads":   map[string]any{"machine": map[string]any{"at": stamp, "every_sec": 60}},
				})
			},
			text: func(t *testing.T, dom *pageDOM) string { return dom.element(t, "machnote").Text },
		},
		{
			name:  "News",
			never: "never read",
			fresh: "read ",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				dom.call(t, "renderNews", map[string]any{
					"news": map[string]any{"at": stamp, "stories": []any{story}},
				})
			},
			text: func(t *testing.T, dom *pageDOM) string { return dom.element(t, "newsnote").Text },
		},
		{
			name:  "Polecats seats",
			never: "never read",
			fresh: "read ",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				dom.call(t, "renderPolecats", map[string]any{
					"summary_at": stamp,
					"polecats":   []any{},
					"reads":      map[string]any{"seats": map[string]any{"at": stamp, "every_sec": 900}},
				})
			},
			text: func(t *testing.T, dom *pageDOM) string { return dom.element(t, "polecatsnote").Text },
		},
		{
			name:  "cloud checks",
			never: "never read",
			fresh: "last ran ",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				dom.call(t, "renderCloud", map[string]any{
					"cloud": map[string]any{"checks": map[string]any{"at": stamp, "state": "stale", "every_sec": 300}},
				})
			},
			text: func(t *testing.T, dom *pageDOM) string { return dom.element(t, "cloud").Text },
		},
		{
			// The data-quality line keeps its own word: the pinned tests name
			// "never scanned" for it, and the helper decides only when.
			name:  "data quality",
			never: "never scanned",
			fresh: "last scanned ",
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				dom.call(t, "renderCost", costUsageState(stamp))
			},
			text: func(t *testing.T, dom *pageDOM) string {
				lines := panelFoldLines(t, dom, "cost.quality")
				if len(lines) == 0 {
					t.Fatal("the data-quality fold draws no lines")
				}
				return lines[0].Text
			},
		},
	}
}

// TestPageNeverReadAge draws every read-age line with a read that never happened
// (a zero, empty or unparsable stamp) and with one taken four minutes ago. The
// never read draws its own word and no age; the fresh one draws its age and none
// of the never words.
func TestPageNeverReadAge(t *testing.T) {
	t.Parallel()

	for _, pane := range agePanes() {
		for _, never := range []struct{ label, stamp string }{
			{"a zero stamp", neverStamp},
			{"an empty stamp", ""},
			{"an unparsable stamp", "not a time"},
		} {
			t.Run(pane.name+" with "+never.label, func(t *testing.T) {
				t.Parallel()

				dom := newPageDOM(t)
				pane.draw(t, dom, never.stamp)
				got := pane.text(t, dom)
				if !strings.Contains(got, pane.never) {
					t.Errorf("%s draws %q for a read that never happened, want %q", pane.name, got, pane.never)
				}
				assertNoBadAge(t, pane.name, got)
			})
		}

		t.Run(pane.name+" fresh read", func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			pane.draw(t, dom, freshStamp())
			got := pane.text(t, dom)
			if !regexp.MustCompile(regexp.QuoteMeta(pane.fresh) + `\S+ ago`).MatchString(got) {
				t.Errorf("%s draws %q for a read taken four minutes ago, want its age after %q", pane.name, got, pane.fresh)
			}
			for _, bad := range []string{"never read", "never scanned"} {
				if strings.Contains(got, bad) {
					t.Errorf("%s draws %q for a read that has happened", pane.name, got)
				}
			}
			assertNoBadAge(t, pane.name, got)
		})
	}
}

// TestPageNeverReadDecidesByStamp pins the decider's edges: an empty stamp, one
// that does not parse, and one a second before the year 2000 are never read, and
// the first instant of 2000 is an old read with an age.
func TestPageNeverReadDecidesByStamp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, stamp string
		never       bool
	}{
		{"empty", "", true},
		{"unparsable", "not a time", true},
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
		assertNoBadAge(t, "COST header", got)
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
		assertNoBadAge(t, "Polecats seats", got)
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
		assertNoBadAge(t, "Machine", got)
	})
}

// TestPageAgeLinesUseTheHelper is a source scan of the page. A line that names a
// read in its text and ages a stamp from the clock (a subtraction from Date.now(),
// or an age() call) must ask neverRead on that line or within the three lines
// above it, so a zero, empty or unparsable stamp never reaches the subtraction.
// Comment lines are skipped: they describe the code, they do not draw it.
func TestPageAgeLinesUseTheHelper(t *testing.T) {
	t.Parallel()

	agesAStamp := regexp.MustCompile(`Date\.now\(\)\s*-|-\s*Date\.now\(\)|\bage\(`)
	namesARead := regexp.MustCompile(`"[^"]*\bread\b[^"]*"|last ran|last scanned|checks:`)
	comment := regexp.MustCompile(`^\s*//`)

	lines := strings.Split(string(indexHTML), "\n")
	for i, line := range lines {
		if comment.MatchString(line) || !agesAStamp.MatchString(line) || !namesARead.MatchString(line) {
			continue
		}
		guarded := false
		for j := max(0, i-3); j <= i; j++ {
			if strings.Contains(lines[j], "neverRead(") {
				guarded = true
			}
		}
		if !guarded {
			t.Errorf("index.html:%d draws a read's age without neverRead: %s", i+1, strings.TrimSpace(line))
		}
	}
}
