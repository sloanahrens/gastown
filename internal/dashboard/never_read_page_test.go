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

// clockOperand is the clock as index.html writes it: Date.now(), new Date(),
// new Date().getTime() or a variable named now, with any whitespace inside the
// calls.
const clockOperand = `(?:Date\s*\.\s*now\s*\(\s*\)|new\s+Date\s*\(\s*\)(?:\s*\.\s*getTime\s*\(\s*\))?|\bnow\b)`

// clockSubtraction matches a subtraction with the clock on either side: the clock
// minus a stamp ("Date.now() - new Date(x)", "now - x", "new Date() - x") or a
// stamp minus the clock. A minus that begins "-=", "--" or "->" is not one.
var clockSubtraction = regexp.MustCompile(clockOperand + `\s*-(?:[^-=>]|$)|-\s*` + clockOperand)

// neverReadTernary is a neverRead call used as the condition of a ternary, as the
// page guards an age: "neverRead(x.at) ? never-text : age-text". A subtraction
// that follows it on the line sits in the else branch.
var neverReadTernary = regexp.MustCompile(`neverRead\s*\((?:[^()]|\([^()]*\))*\)\s*\?`)

// neverTernary is the two-line form: "const never = neverRead(x.at);" above and
// "never ? null : <age>" on the line. The assignment must be within the three
// lines above, or the name is some other never.
var (
	neverTernary  = regexp.MustCompile(`\bnever\s*\?`)
	neverAssigned = regexp.MustCompile(`\bnever\s*=\s*neverRead\s*\(`)
)

// ageAllowList names each subtraction from the clock that is not a read's age by
// the whole trimmed line that holds it, with the reason. A key is the full line, so
// an edit to the line fails the scan until the entry is re-read, and a key cannot
// match more than it names. A subtraction that sits in the else branch of a
// neverRead ternary on the same stamp needs no entry.
var ageAllowList = map[string]string{
	"const age = iso => iso ? fmtDur((Date.now() - new Date(iso).getTime()) / 1000) : \"–\";":                 "age formats a stamp; a caller that draws a read's age asks neverRead on the same argument within three lines (the call check below); the other callers pass a record's own stamp",
	"const old = (Date.now() - new Date(u.as_of).getTime()) / 1000 > USAGE_STALE_SEC;":                        "the else of neverRead(u.as_of) two lines up, so the stamp is a real read",
	"const oldest = sm.oldest_ready ? (Date.now() - new Date(sm.oldest_ready).getTime()) / 1000 : null;":      "age of the oldest ready bead: the bead's own stamp, not a read",
	"const tickSec = dp ? (Date.now() - new Date(dp.at).getTime()) / 1000 : null;":                            "age of the dispatcher's last tick: the record's own stamp, not a read",
	"const dispatchedSec = p.slung ? (now - new Date(p.slung).getTime()) / 1000 : null;":                      "a polecat's dispatch time: a session stamp, not a read",
	"const since = p.state_since ? (now - new Date(p.state_since).getTime()) / 1000 : dispatchedSec;":         "how long a polecat has been in its state: a session stamp, not a read",
	"const quiet = p.has_session && p.last_active ? (now - new Date(p.last_active).getTime()) / 1000 : null;": "how long a polecat has been quiet: a session stamp, not a read",
	"const inSecs = (new Date(r.next_try) - Date.now()) / 1000;":                                              "time until a retry: a future stamp, not an age",
	"tr.append(el(\"td\", \"when title\", r.outcome === \"running\" ? \"merging \" + fmtDur((Date.now() - when.getTime()) / 1000) : r.outcome === \"backoff\" ? \"checked \" + clock(r.at).slice(0, 5) : (when.toDateString() === today ? clock(r.at).slice(0, 5) : when.toLocaleDateString([], {month: \"short\", day: \"numeric\"}) + \" \" + clock(r.at).slice(0, 5))));": "a running merge's start: the merge record's own stamp",
	"st.append(el(\"b\", \"good\", \"running\"), el(\"span\", \"\", run.rig + \" · \" + stages + \" · \" + fmtDur((Date.now() - new Date(run.since).getTime()) / 1000)));":                                                                                                                                                                                                   "a running run's start: the run record's own stamp",
	"$(\"tiersweepnote\").textContent = rows.length + \" sweeps · last \" + fmtDur((Date.now() - new Date(newest.at).getTime()) / 1000) + \" ago\";":                                                                                                                                                                                                                         "the newest sweep's stamp: a sweep record, not a read",
	"return isNaN(t) ? null : (Date.now() - t) / 1000;":                                                         "secsSince: a run's start or stop stamp; an unreadable one is null",
	"return {counts: counts, ageSec: newest === null ? null : Math.max(0, Math.round((now - newest) / 1000))};": "age of the newest alert event: the feed's own stamp, not a read",
	"const since = now - EVENT_STATS_WINDOW_MS;":                                                                "the start of the event stats window: a count of milliseconds",
	"const now = Date.now(), late = now - lastTick > 2 * WATCHDOG_MS;":                                          "the page's watchdog clock, not a stamp from the server",
	"if (now - lastEvent < SILENT_MS) return;":                                                                  "the page's watchdog clock, not a stamp from the server",
}

// expressionEnd returns the offset where the expression that starts at from ends:
// the first ";" or "," at paren depth zero, or the end of text. A ternary's else
// branch runs to that point, so a guard covers only the subtractions inside it.
func expressionEnd(text string, from int) int {
	depth := 0
	for i := from; i < len(text); i++ {
		switch text[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return i
			}
			depth--
		case ';', ',':
			if depth == 0 {
				return i
			}
		}
	}
	return len(text)
}

// guardedAt reports whether the subtraction that begins at offset in text sits in
// the else branch of a neverRead guard: a "neverRead(...) ?" on the same line whose
// ternary expression spans the offset, or a "never ?" whose expression spans it
// when never was assigned from neverRead within the three lines above (above,
// oldest first). A guard covers only its own expression: a second subtraction
// after the ternary's end is not guarded by it.
func guardedAt(text string, offset int, above []string) bool {
	covers := func(m []int) bool { return m[1] <= offset && offset < expressionEnd(text, m[1]) }
	for _, m := range neverReadTernary.FindAllStringIndex(text, -1) {
		if covers(m) {
			return true
		}
	}
	for _, m := range neverTernary.FindAllStringIndex(text, -1) {
		if !covers(m) {
			continue
		}
		for _, prev := range above {
			if neverAssigned.MatchString(prev) {
				return true
			}
		}
	}
	return false
}

// unguardedClockLines returns one message for each subtraction of a stamp from the
// clock (or the clock from a stamp) in src that is neither guarded by a neverRead
// ternary before it on its line nor on a line that is a key of allow. A line that
// mentions neverRead elsewhere is not guarded by the mention. unused lists the keys
// of allow that match no line. Comment lines describe the code and are skipped.
func unguardedClockLines(src string, allow map[string]string) (bad, unused []string) {
	matched := map[string]bool{}
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "//") {
			continue
		}
		hits := clockSubtraction.FindAllStringIndex(text, -1)
		if len(hits) == 0 {
			continue
		}
		if _, ok := allow[text]; ok {
			matched[text] = true
			continue
		}
		above := lines[max(0, i-3):i]
		for _, h := range hits {
			if guardedAt(text, h[0], above) {
				continue
			}
			bad = append(bad, fmt.Sprintf("line %d subtracts a stamp from the clock at column %d without a neverRead guard before it or an allow-list entry for the whole line: %s", i+1, h[0]+1, text))
		}
	}
	for key := range allow {
		if !matched[key] {
			unused = append(unused, key)
		}
	}
	sort.Strings(unused)
	return bad, unused
}

// ageCall is one age(<arg>) call with a single argument, the page's age formatter.
var ageCall = regexp.MustCompile(`(?:^|[^\w.])age\(([^(),]*(?:\([^()]*\))?[^(),]*)\)`)

// readAgeCallsWithoutGuard returns one message per age(x) call on a line of src
// that names a read (a quoted text with the word read, "last ran", "last scanned"
// or "checks:") when no neverRead(x) on the same argument x sits on that line or
// within the three lines above it. Comment lines are skipped.
func readAgeCallsWithoutGuard(src string) (bad []string) {
	namesARead := regexp.MustCompile(`"[^"]*\bread\b[^"]*"|last ran|last scanned|checks:`)
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "//") || !namesARead.MatchString(text) {
			continue
		}
		window := strings.Join(lines[max(0, i-3):i+1], "\n")
		for _, m := range ageCall.FindAllStringSubmatch(text, -1) {
			arg := strings.TrimSpace(m[1])
			if arg == "" || strings.Contains(window, "neverRead("+arg+")") {
				continue
			}
			bad = append(bad, fmt.Sprintf("line %d draws a read's age with age(%s) and no neverRead(%s) on the line or the three above: %s", i+1, arg, arg, text))
		}
	}
	return bad
}

// TestPageAgeLinesUseTheHelper is a source scan of the page. Every subtraction from
// the clock in index.html must sit in the else branch of a neverRead guard on its
// line, or on a line that is an allow-list key with its reason, so a zero, empty or
// unparsable stamp never reaches a subtraction unasked. Every age(x) call on a line
// that names a read must have neverRead(x) on the same argument within the three
// lines above it. Comment lines are skipped.
func TestPageAgeLinesUseTheHelper(t *testing.T) {
	t.Parallel()

	bad, unused := unguardedClockLines(string(indexHTML), ageAllowList)
	for _, msg := range bad {
		t.Errorf("index.html: %s", msg)
	}
	for _, key := range unused {
		t.Errorf("the allow-list entry %q matches no line of index.html; re-read the line or remove the entry", key)
	}
	for _, msg := range readAgeCallsWithoutGuard(string(indexHTML)) {
		t.Errorf("index.html: %s", msg)
	}
}

// TestAgeScanFailsOnPlantedLines runs the scans over fixtures: each unguarded
// spelling of a subtraction from the clock must fail, a mention of neverRead that
// does not guard the subtraction must fail, a second unguarded subtraction beside a
// guarded one must fail, and the guarded forms, an allow-listed whole line and a
// comment must pass. It proves the scans can fail, so a pass on the page means the
// page has no such line.
func TestAgeScanFailsOnPlantedLines(t *testing.T) {
	t.Parallel()

	allow := map[string]string{"const a = (Date.now() - x) / 1000;": "a planted record stamp"}
	for _, tc := range []struct {
		name, src string
		allow     map[string]string
		fails     bool
	}{
		{"Date.now() minus new Date", "const a = (Date.now() - new Date(x)) / 1000;", nil, true},
		{"now minus x", "const a = (now - x) / 1000;", nil, true},
		{"new Date() minus x", "const a = (new Date()-x) / 1000;", nil, true},
		{"new Date().getTime() minus x", "const a = (new Date().getTime() - x) / 1000;", nil, true},
		{"spaced Date.now minus Date.parse", "const a = (Date . now ( ) -Date.parse(x)) / 1000;", nil, true},
		{"a stamp minus the clock", "const a = (x - Date.now()) / 1000;", nil, true},
		{"a mention of neverRead after the subtraction", "const a = (Date.now() - new Date(x)) / 1000; const b = neverRead(x);", nil, true},
		{"a neverRead that is not a ternary", "if (neverRead(x)) log(); const a = (Date.now() - new Date(x)) / 1000;", nil, true},
		{"a second subtraction beside a guarded one", "const a = neverRead(x) ? null : (Date.now() - new Date(x)) / 1000, b = (now - y) / 1000;", nil, true},
		{"never ? with no neverRead assignment above", "const never = x === null;\nconst a = never ? null : (now - x);", nil, true},
		{"an allow-list key that is only a prefix of the line", "const a = (Date.now() - x) / 1000; const b = 2;", allow, true},
		{"guarded on its line", "const a = neverRead(x) ? null : (Date.now() - new Date(x)) / 1000;", nil, false},
		{"guarded by never from the line above", "const never = neverRead(x.at);\nconst a = never ? null : (now - new Date(x.at).getTime()) / 1000;", nil, false},
		{"an allow-listed whole line", "const a = (Date.now() - x) / 1000;", allow, false},
		{"a comment", "// Date.now() - new Date(x) is the age a read draws", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bad, _ := unguardedClockLines(tc.src, tc.allow)
			if got := len(bad) > 0; got != tc.fails {
				t.Errorf("the scan of %q reports %q, want a failure = %v", tc.src, bad, tc.fails)
			}
		})
	}

	// The allow-list key that is only a prefix is also reported unused, so a
	// shortened key cannot pass by matching part of a line.
	if _, unused := unguardedClockLines("const a = (Date.now() - x) / 1000; const b = 2;", allow); len(unused) != 1 {
		t.Errorf("a prefix key reads as used: unused = %q, want the one key", unused)
	}
}

// TestReadAgeCallScanFailsOnPlantedLines drives the age(x) call check: a read's age
// drawn without neverRead(x) fails, with neverRead on another argument fails, with
// neverRead(x) within three lines passes, and an age of a record's own stamp on a
// line that names no read is not checked.
func TestReadAgeCallScanFailsOnPlantedLines(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, src string
		fails     bool
	}{
		{"a read's age with no guard", `note.textContent = "read " + age(f.at) + " ago";`, true},
		{"a read's age guarded on another argument", `note.textContent = neverRead(g.at) ? "never read" : "read " + age(f.at) + " ago";`, true},
		{"a read's age guarded on its argument", `note.textContent = neverRead(f.at) ? "never read" : "read " + age(f.at) + " ago";`, false},
		{"a read's age guarded two lines above", "const never = neverRead(f.at);\nconst x = 1;\nnote.textContent = never ? \"never read\" : \"read \" + age(f.at) + \" ago\";", false},
		{"a record's own stamp names no read", `head.append(el("span", "bage", "written " + age(b.written) + " ago"));`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bad := readAgeCallsWithoutGuard(tc.src)
			if got := len(bad) > 0; got != tc.fails {
				t.Errorf("the call scan of %q reports %q, want a failure = %v", tc.src, bad, tc.fails)
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
