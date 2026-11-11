package dashboard

import (
	"fmt"
	"regexp"
	"sort"
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
	fresh string // a pattern the age a fresh read draws matches
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
			fresh: `read \S+ ago`,
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
			fresh: `read \S+ ago`,
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
			fresh: `read \S+ ago`,
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
			fresh: `read \S+ ago`,
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
			name:  "Forgejo snapshot",
			never: "never read",
			fresh: `snapshot \S+ old`,
			draw: func(t *testing.T, dom *pageDOM, stamp string) {
				// The snapshot's note is drawn with the feed: an empty feed returns before it.
				event := map[string]any{"at": "2026-11-11T02:00:00Z", "actor": "a", "summary": "a push", "repo": "gastown"}
				dom.call(t, "renderForgejo", map[string]any{
					"forgejo": map[string]any{"at": stamp, "every_seconds": 300, "events": []any{event}},
				})
			},
			text: func(t *testing.T, dom *pageDOM) string { return dom.element(t, "forgejonote").Text },
		},
		{
			name:  "cloud checks",
			never: "never read",
			fresh: `last ran \S+ ago`,
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
			fresh: `last scanned \S+ ago`,
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
			if !regexp.MustCompile(pane.fresh).MatchString(got) {
				t.Errorf("%s draws %q for a read taken four minutes ago, want its age as %q", pane.name, got, pane.fresh)
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

// clockOperand is the clock as index.html writes it: Date.now(), new Date() or a
// variable named now, with any whitespace inside the call.
const clockOperand = `(?:Date\s*\.\s*now\s*\(\s*\)|new\s+Date\s*\(\s*\)|\bnow\b)`

// clockSubtraction matches a subtraction with the clock on either side: the clock
// minus a stamp ("Date.now() - new Date(x)", "now - x", "new Date() - x") or a
// stamp minus the clock. A minus that begins "-=", "--" or "->" is not one.
var clockSubtraction = regexp.MustCompile(clockOperand + `\s*-(?:[^-=>]|$)|-\s*` + clockOperand)

// ageAllowList names each subtraction from the clock that is not a read's age,
// by a piece of its line, with the reason. A subtraction on a line that asks
// neverRead is guarded and needs no entry. The test fails on an entry that matches
// no line, so an entry for a line that moved or went is removed with it.
var ageAllowList = map[string]string{
	"const age = iso => iso ?":                               "age formats a stamp; every caller asks neverRead first (the call check below)",
	"const ageSec = never ? null :":                          "readOf: never is neverRead(x.at), decided on the line above",
	"const age = never ? null :":                             "the Forgejo snapshot's age: never is neverRead(f.at), decided on the line above",
	"const old = (Date.now() - new Date(u.as_of).getTime())": "the else of neverRead(u.as_of) two lines up, so the stamp is a real read",
	"const oldest = sm.oldest_ready ?":                       "age of the oldest ready bead: the bead's own stamp, not a read",
	"const tickSec = dp ?":                                   "age of the dispatcher's last tick: the record's own stamp, not a read",
	"const dispatchedSec = p.slung ?":                        "a polecat's dispatch time: a session stamp, not a read",
	"const since = p.state_since ?":                          "how long a polecat has been in its state: a session stamp, not a read",
	"const quiet = p.has_session && p.last_active ?":         "how long a polecat has been quiet: a session stamp, not a read",
	"const inSecs = (new Date(r.next_try) - Date.now())":     "time until a retry: a future stamp, not an age",
	"fmtDur((Date.now() - when.getTime())":                   "a running merge's start: the merge record's own stamp",
	"fmtDur((Date.now() - new Date(run.since).getTime())":    "a running run's start: the run record's own stamp",
	"fmtDur((Date.now() - new Date(newest.at).getTime())":    "the newest sweep's stamp: a sweep record, not a read",
	"return isNaN(t) ? null : (Date.now() - t) / 1000;":      "secsSince: a run's start or stop stamp; an unreadable one is null",
	"const oldest = stamps.length &&":                        "the oldest event bead's own stamp, not a read",
	"ageSec: newest === null":                                "age of the newest alert event: the feed's own stamp, not a read",
	"const since = now - EVENT_STATS_WINDOW_MS;":             "the start of the event stats window: a count of milliseconds",
	"const now = Date.now(), late = now - lastTick":          "the page's watchdog clock, not a stamp from the server",
	"if (now - lastEvent < SILENT_MS) return;":               "the page's watchdog clock, not a stamp from the server",
}

// unguardedClockLines returns one message for each line of src that subtracts a
// stamp from the clock, or the clock from a stamp, when the line neither asks
// neverRead nor contains a piece of allow. unused lists the pieces of allow that
// no line contains. Comment lines describe the code and are skipped.
func unguardedClockLines(src string, allow map[string]string) (bad, unused []string) {
	matched := map[string]bool{}
	for i, line := range strings.Split(src, "\n") {
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "//") || !clockSubtraction.MatchString(text) || strings.Contains(text, "neverRead(") {
			continue
		}
		allowed := false
		for piece := range allow {
			if strings.Contains(text, piece) {
				matched[piece] = true
				allowed = true
			}
		}
		if !allowed {
			bad = append(bad, fmt.Sprintf("line %d subtracts a stamp from the clock without neverRead or an allow-list reason: %s", i+1, text))
		}
	}
	for piece := range allow {
		if !matched[piece] {
			unused = append(unused, piece)
		}
	}
	sort.Strings(unused)
	return bad, unused
}

// TestPageAgeLinesUseTheHelper is a source scan of the page. Every subtraction from
// the clock in index.html must sit on a line that asks neverRead, or on the
// allow-list with its reason, so a zero, empty or unparsable stamp never reaches
// a subtraction unasked. Every age() call that names a read must also ask
// neverRead within the three lines above it. Comment lines are skipped.
func TestPageAgeLinesUseTheHelper(t *testing.T) {
	t.Parallel()

	bad, unused := unguardedClockLines(string(indexHTML), ageAllowList)
	for _, msg := range bad {
		t.Errorf("index.html: %s", msg)
	}
	for _, piece := range unused {
		t.Errorf("the allow-list entry %q matches no line of index.html; remove it", piece)
	}

	namesARead := regexp.MustCompile(`"[^"]*\bread\b[^"]*"|last ran|last scanned|checks:`)
	callsAge := regexp.MustCompile(`\bage\(`)
	comment := regexp.MustCompile(`^\s*//`)

	lines := strings.Split(string(indexHTML), "\n")
	for i, line := range lines {
		if comment.MatchString(line) || !callsAge.MatchString(line) || !namesARead.MatchString(line) {
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

// TestAgeScanFailsOnPlantedLines runs the subtraction scan over fixtures: each
// unguarded spelling of a subtraction from the clock must fail it, and the guarded
// lines and a comment must pass. It proves the scan can fail, so a pass on the page
// means the page has no such line.
func TestAgeScanFailsOnPlantedLines(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, src string
		fails     bool
	}{
		{"Date.now() minus new Date", "const a = (Date.now() - new Date(x)) / 1000;", true},
		{"now minus x", "const a = (now - x) / 1000;", true},
		{"new Date() minus x", "const a = (new Date()-x) / 1000;", true},
		{"spaced Date.now minus Date.parse", "const a = (Date . now ( ) -Date.parse(x)) / 1000;", true},
		{"a stamp minus the clock", "const a = (x - Date.now()) / 1000;", true},
		{"guarded on its line", "const a = neverRead(x) ? null : (Date.now() - new Date(x)) / 1000;", false},
		{"a comment", "// Date.now() - new Date(x) is the age a read draws", false},
		{"a guarded block", "const never = neverRead(x);\nconst a = neverRead(x) ? null : (now - x);\nconst b = 1;", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bad, _ := unguardedClockLines(tc.src, nil)
			if got := len(bad) > 0; got != tc.fails {
				t.Errorf("the scan of %q reports %q, want a failure = %v", tc.src, bad, tc.fails)
			}
		})
	}
}

// TestPageNeverReadForgejoSnapshotIsStale: a Forgejo snapshot nobody has read draws
// "never read" in the class a never-read source takes (amber, red when its read
// failed), and a fresh snapshot draws its age without that class.
func TestPageNeverReadForgejoSnapshotIsStale(t *testing.T) {
	t.Parallel()

	event := map[string]any{"at": "2026-11-11T02:00:00Z", "actor": "a", "summary": "a push", "repo": "gastown"}
	draw := func(t *testing.T, forgejo map[string]any) drawn {
		t.Helper()
		dom := newPageDOM(t)
		dom.call(t, "renderForgejo", map[string]any{"forgejo": forgejo})
		return dom.element(t, "forgejonote")
	}

	t.Run("never read", func(t *testing.T) {
		t.Parallel()

		got := draw(t, map[string]any{"at": neverStamp, "every_seconds": 300, "events": []any{event}})
		if !strings.Contains(got.Text, "never read") {
			t.Errorf("the snapshot note draws %q for a snapshot never read, want \"never read\"", got.Text)
		}
		if !strings.Contains(got.Class, "warnc") {
			t.Errorf("the snapshot note has class %q for a snapshot never read, want the stale class warnc", got.Class)
		}
		assertNoBadAge(t, "Forgejo snapshot", got.Text)
	})

	t.Run("fresh", func(t *testing.T) {
		t.Parallel()

		got := draw(t, map[string]any{"at": freshStamp(), "every_seconds": 300, "events": []any{event}})
		if strings.Contains(got.Class, "warnc") || strings.Contains(got.Class, "badc") {
			t.Errorf("the snapshot note has class %q for a fresh snapshot, want no stale class", got.Class)
		}
		assertNoBadAge(t, "Forgejo snapshot", got.Text)
	})
}
