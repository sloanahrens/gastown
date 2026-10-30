// Package omlessons clusters om's review findings into rejection classes, so
// the weekly lessons pass counts the same classes every week (gt-tsetu).
//
// A class is a row of the table below: an id, a name, the directive rule that
// covers it, and the keywords a finding is matched by. A finding joins the
// first row whose title or claim keyword it carries. One that matches no row
// is unclassified, and the report lists its title, so a class the table is
// missing is named before it is added.
package omlessons

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

const (
	// DefaultDays is the length of a window when the operator names none: a
	// week, the cadence of the lessons pass.
	DefaultDays = 7

	// Unclassified is the id the report counts a finding under when no class
	// matches it.
	Unclassified = "unclassified"
)

// Class is one rejection class: a row of the table that turns om's finding
// text into a count.
type Class struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Rule is the number the class's rule carries in the directive's "What om
	// rejects" list, or 0 when no numbered rule covers it.
	Rule int `json:"rule,omitempty"`
	// Title and Claim are the lowercase substrings a finding is matched by,
	// against its title and its claim: one hit in either is a match.
	Title []string `json:"-"`
	Claim []string `json:"-"`
}

// classes is the table, most specific first: a finding joins the first row it
// matches, so a broad keyword must follow the narrow row that should claim it.
// The rows are seeded from the directive's "What om rejects" rules and the
// phrasings it quotes; a row is added when the report's unclassified list
// shows the same title week after week.
var classes = []Class{
	{
		ID: "fail-open", Name: "an error reads as success", Rule: 1,
		Title: []string{"fail-open", "fail open", "reads as clean", "as clean", "reads as success", "as success", "as approval", "does not apply", "treated as absent", "as if it succeeded", "suppresses escalation"},
		Claim: []string{"fail-open", "fail open", "reads the error", "reads a failed", "treats the error", "cannot tell a failed", "suppresses the escalation"},
	},
	{
		ID: "silent-drop", Name: "a bad input is dropped without a word", Rule: 1,
		Title: []string{"silently", "swallows", "discards", "drops the input", "ignores the error", "no error is returned"},
		Claim: []string{"silently", "swallowed", "discarded", "no error is returned", "drops the"},
	},
	{
		ID: "missing-refusal-test", Name: "a refusal branch has no test", Rule: 3,
		Title: []string{"refusal branch", "failing branch", "no assertion", "lacks a test", "lacks tests", "untested", "error path has no"},
		Claim: []string{"no test drives", "only the success path", "is not tested", "no assertion", "the refusal branch"},
	},
	{
		ID: "no-red-when-removed", Name: "no test goes red when the code is removed", Rule: 2,
		Title: []string{"passes with the", "passed with the", "passes without", "would still pass", "goes red", "fails without the", "test always passes"},
		Claim: []string{"would still pass", "still passes", "went red", "goes red", "fails when removed", "would pass with"},
	},
	{
		ID: "wall-clock-wait", Name: "a wait or sleep on the wall clock", Rule: 0,
		Title: []string{"sleep", "wall clock", "wall-clock", "polls until", "busy-wait", "a wait of"},
		Claim: []string{"sleep", "wall clock", "wall-clock", "the real clock", "waits for the clock", "real time"},
	},
	{
		ID: "non-hermetic-test", Name: "package state or a test on the host", Rule: 7,
		Title: []string{"non-hermetic", "not hermetic", "hermetic", "host's", "hosts", "environment variable", "package-level", "global state", "live town", "tempdir"},
		Claim: []string{"hermetic", "os.getenv", "the host's", "package-level", "global state", "developer's environment"},
	},
	{
		ID: "doc-contradicts-code", Name: "a doc says what the code does not do", Rule: 0,
		Title: []string{"contradicts", "stale doc", "documentation says", "the doc says"},
		Claim: []string{"contradicts", "documentation says", "the doc says", "no longer true"},
	},
	{
		ID: "comment-claim", Name: "a comment carries a line number or an untrue claim", Rule: 0,
		Title: []string{"comment", "line number", "misleading", "misstates"},
		Claim: []string{"comment", "the code does not", "misstates", "line number"},
	},
	{
		ID: "scope-widened", Name: "the change reaches past its scope", Rule: 9,
		Title: []string{"outside the scope", "out of scope", "scope", "unrelated", "unexplained"},
		Claim: []string{"unrelated", "outside the stated scope", "widens the", "not part of the bead"},
	},
	{
		ID: "secret-in-output", Name: "a secret reaches output", Rule: 12,
		Title: []string{"secret", "password", "credential", "token", "captured output", "echoes"},
		Claim: []string{"secret", "password", "credential", "token", "captured"},
	},
	{
		ID: "script-holds-logic", Name: "a new script holds the logic", Rule: 8,
		Title: []string{"shell script", "script", ".sh", "python"},
		Claim: []string{"script", "shell"},
	},
	{
		ID: "unverified-claim", Name: "a verification claim shows no output", Rule: 6,
		Title: []string{"no shown output", "without output", "no output", "unfalsifiable", "verification claim", "says it passed"},
		Claim: []string{"no output", "not shown", "without showing", "did not show", "unfalsifiable"},
	},
}

// Bounds is one window: half-open, from Start's first moment to End's.
type Bounds struct {
	Start, End time.Time
}

// Span is the two windows one report covers.
type Span struct {
	Current, Prior Bounds
	// Days is each window's length, in calendar days.
	Days int
}

// Windows returns the two windows a report covers: Days days ending with
// today when since is zero, or starting on since's day when it is not. Each
// window is a whole number of calendar days in now's location.
func Windows(now, since time.Time, days int) (Span, error) {
	if days < 1 {
		return Span{}, fmt.Errorf("--days %d: want at least one day", days)
	}
	var cur Bounds
	if since.IsZero() {
		cur.End = dayStart(now).AddDate(0, 0, 1)
		cur.Start = cur.End.AddDate(0, 0, -days)
	} else {
		cur.Start = dayStart(since)
		cur.End = cur.Start.AddDate(0, 0, days)
	}
	return Span{
		Current: cur,
		Prior:   Bounds{Start: cur.Start.AddDate(0, 0, -days), End: cur.Start},
		Days:    days,
	}, nil
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func (b Bounds) holds(at time.Time) bool {
	return !at.Before(b.Start) && at.Before(b.End)
}

// Report is one run's clustering: each class's figures in the two windows,
// the findings no class matched, and how many verdicts carried no finding text
// to classify.
type Report struct {
	Days         int                 `json:"days"`
	Window       Window              `json:"window"`
	Prior        Window              `json:"prior"`
	Classes      []ClassRow          `json:"classes"`
	Unclassified []UnclassifiedTitle `json:"unclassified"`
}

// Window is one window's days and the verdicts the clustering could not read
// findings from.
type Window struct {
	// First and Last are the window's first and last calendar days, both
	// counted.
	First string `json:"first"`
	Last  string `json:"last"`
	// Verdicts counts every verdict record in the window, whether it carried
	// finding text or not.
	Verdicts int `json:"verdicts"`
	// NoFindingText counts the records written before the log kept finding
	// text, whose findings are in no class and so in no count.
	NoFindingText int `json:"no_finding_text"`
}

// ClassRow is one class's figures in the current window and the one before it.
type ClassRow struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Rule    int         `json:"rule,omitempty"`
	Current ClassCounts `json:"current"`
	Prior   ClassCounts `json:"prior"`
}

// ClassCounts is how one window's findings fell into a class.
type ClassCounts struct {
	// Findings counts every finding of the class, whatever its severity.
	Findings int `json:"findings"`
	// Beads counts the beads it found them on.
	Beads int `json:"beads"`
	// Rounds counts the attempts after the first on beads where the class
	// carried a blocking finding: a class whose findings were all minor
	// burned no round of its own.
	Rounds int `json:"rounds"`
}

// UnclassifiedTitle is one finding title no class matched, with its count in
// each window.
type UnclassifiedTitle struct {
	// Title is empty for a finding the log kept with no title.
	Title   string `json:"title"`
	Current int    `json:"current"`
	Prior   int    `json:"prior"`
}

// Cluster counts records, the verdicts read from the review log, by class in
// the span's two windows. A record outside both windows counts nothing.
func Cluster(records []land.ReviewRecord, sp Span) Report {
	cur, pri := newWindowTally(), newWindowTally()
	for _, rec := range records {
		var t *windowTally
		switch {
		case sp.Current.holds(rec.At):
			t = cur
		case sp.Prior.holds(rec.At):
			t = pri
		default:
			continue
		}
		t.add(rec)
	}
	rep := Report{
		Days:         sp.Days,
		Window:       cur.window(sp.Current),
		Prior:        pri.window(sp.Prior),
		Classes:      make([]ClassRow, 0, len(classes)),
		Unclassified: unclassifiedTitles(cur, pri),
	}
	for _, c := range classes {
		rep.Classes = append(rep.Classes, ClassRow{
			ID: c.ID, Name: c.Name, Rule: c.Rule,
			Current: cur.counts(c.ID), Prior: pri.counts(c.ID),
		})
	}
	return rep
}

type classTally struct {
	findings      int
	beads         map[string]struct{}
	blockingBeads map[string]struct{}
}

type windowTally struct {
	verdicts      int
	noFindingText int
	classes       map[string]*classTally
	unclassified  map[string]int
	// attempts is each bead's highest attempt in the window, so the rounds a
	// class burned are the attempts past the first on the beads it blocked.
	attempts map[string]int
}

func newWindowTally() *windowTally {
	return &windowTally{
		classes:      map[string]*classTally{},
		unclassified: map[string]int{},
		attempts:     map[string]int{},
	}
}

func (w *windowTally) add(rec land.ReviewRecord) {
	w.verdicts++
	if rec.Attempt > w.attempts[rec.BeadID] {
		w.attempts[rec.BeadID] = rec.Attempt
	}
	// A record written before the log kept finding text names no finding, so
	// it can be counted for its verdict and for nothing else.
	if len(rec.FindingText) == 0 {
		w.noFindingText++
		return
	}
	for _, f := range rec.FindingText {
		id := classify(f)
		if id == Unclassified {
			w.unclassified[strings.TrimSpace(f.Title)]++
			continue
		}
		t := w.class(id)
		t.findings++
		t.beads[rec.BeadID] = struct{}{}
		if blocking(f.Severity) {
			t.blockingBeads[rec.BeadID] = struct{}{}
		}
	}
}

func (w *windowTally) class(id string) *classTally {
	t := w.classes[id]
	if t == nil {
		t = &classTally{beads: map[string]struct{}{}, blockingBeads: map[string]struct{}{}}
		w.classes[id] = t
	}
	return t
}

func (w *windowTally) counts(id string) ClassCounts {
	t := w.classes[id]
	if t == nil {
		return ClassCounts{}
	}
	rounds := 0
	for bead := range t.blockingBeads {
		if burned := w.attempts[bead] - 1; burned > 0 {
			rounds += burned
		}
	}
	return ClassCounts{Findings: t.findings, Beads: len(t.beads), Rounds: rounds}
}

func (w *windowTally) window(b Bounds) Window {
	return Window{
		First:         b.Start.Format(land.OmStatsDayLayout),
		Last:          b.End.AddDate(0, 0, -1).Format(land.OmStatsDayLayout),
		Verdicts:      w.verdicts,
		NoFindingText: w.noFindingText,
	}
}

// classify returns the id of the first class f matches, or Unclassified.
func classify(f land.ReviewFindingText) string {
	title, claim := strings.ToLower(f.Title), strings.ToLower(f.Claim)
	for _, c := range classes {
		if matchesAny(c.Title, title) || matchesAny(c.Claim, claim) {
			return c.ID
		}
	}
	return Unclassified
}

func matchesAny(keywords []string, text string) bool {
	for _, keyword := range keywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

// blocking reports whether severity is one om refuses a landing for, padded or
// shifted in case as om writes it.
func blocking(severity string) bool {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "major":
		return true
	}
	return false
}

// unclassifiedTitles lists every title no class matched, most frequent first.
func unclassifiedTitles(cur, pri *windowTally) []UnclassifiedTitle {
	titles := make([]string, 0, len(cur.unclassified)+len(pri.unclassified))
	seen := map[string]bool{}
	for _, t := range []map[string]int{cur.unclassified, pri.unclassified} {
		for title := range t {
			if !seen[title] {
				seen[title] = true
				titles = append(titles, title)
			}
		}
	}
	out := make([]UnclassifiedTitle, 0, len(titles))
	for _, title := range titles {
		out = append(out, UnclassifiedTitle{
			Title:   title,
			Current: cur.unclassified[title],
			Prior:   pri.unclassified[title],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current > out[j].Current
		}
		if out[i].Prior != out[j].Prior {
			return out[i].Prior > out[j].Prior
		}
		return out[i].Title < out[j].Title
	})
	return out
}
