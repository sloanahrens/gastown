package dedupe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// t0 is the pass's clock. Every bead a test makes is created before it unless
// the test says otherwise.
var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// fakeAsker returns scripted answers in order and records every state and
// question set.
type fakeAsker struct {
	script []fakeAnswer
	calls  []fakeCall
	// onAsk, when set, runs first and its error is the call's result.
	onAsk func() error
}

type fakeAnswer struct {
	ans *jev.Answers
	err error
}

type fakeCall struct {
	state string
	qs    map[string]jev.Question
}

func (f *fakeAsker) Ask(_ context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	f.calls = append(f.calls, fakeCall{state: state.(string), qs: qs})
	if f.onAsk != nil {
		if err := f.onAsk(); err != nil {
			return nil, err
		}
	}
	if len(f.script) == 0 {
		return nil, jev.ErrBadResponse
	}
	a := f.script[0]
	f.script = f.script[1:]
	return a.ans, a.err
}

// choices answers the duplicate Choice and every fixed question, so a test can
// vary just the choice.
func choices(choice string, confidence float64) *jev.Answers {
	return &jev.Answers{
		Model:  "jev-1.13.0",
		Choice: map[string]jev.ChoiceAnswer{QDuplicate: {Choice: choice, Confidence: confidence}},
		Noul: map[string]float64{
			QAnyDuplicate: 0.9,
			QFixWorthy:    0.8,
			QReportOnly:   0.1,
			QTestOnly:     0.2,
		},
		Score: map[string]jev.ScoreAnswer{QSeverity: {Score: 2, Confidence: 0.7}},
	}
}

// batteryOnly answers the questions a finding with no candidate draws: the
// fixed ones, without the duplicate Choice or any_duplicate.
func batteryOnly() *jev.Answers {
	a := choices("", 0)
	delete(a.Choice, QDuplicate)
	delete(a.Noul, QAnyDuplicate)
	return a
}

// fakeSearcher returns the hits for a query text.
type fakeSearcher struct {
	hits map[string][]Hit
	err  error
	n    int
	// onSearch, when set, runs first and its error is the call's result.
	onSearch func() error
}

func (f *fakeSearcher) Search(_ context.Context, text string) ([]Hit, error) {
	f.n++
	if f.onSearch != nil {
		if err := f.onSearch(); err != nil {
			return nil, err
		}
	}
	return f.hits[text], f.err
}

// fakeBeads is the rig's bead store: it lists and shows, and has no write. It
// records the name of every call, so a test can count what a pass did to it.
type fakeBeads struct {
	all   []*beads.Issue
	calls []string
}

func (f *fakeBeads) Since(since time.Time) ([]*beads.Issue, error) {
	f.calls = append(f.calls, "Since")
	var out []*beads.Issue
	for _, b := range f.all {
		if !beads.ParseIssueTime(b.CreatedAt).Before(since) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeBeads) Show(id string) (*beads.Issue, error) {
	f.calls = append(f.calls, "Show")
	for _, b := range f.all {
		if b.ID == id {
			return b, nil
		}
	}
	return nil, errors.New("not found")
}

// bead makes an issue created at the given minute after t0.
func bead(id, title, desc string, minute int, labels ...string) *beads.Issue {
	return &beads.Issue{
		ID:          id,
		Title:       title,
		Description: desc,
		Status:      "open",
		CreatedAt:   t0.Add(time.Duration(minute) * time.Minute).Format(time.RFC3339),
		Labels:      labels,
	}
}

// harness is one pass over fakes, with its files in a temp directory.
type harness struct {
	pass   *Pass
	asker  *fakeAsker
	search *fakeSearcher
	beads  *fakeBeads
	waits  []time.Duration
	spent  float64
	log    string
	cursor string
}

func newHarness(t *testing.T, all []*beads.Issue, hits map[string][]Hit, answers ...fakeAnswer) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		asker:  &fakeAsker{script: answers},
		search: &fakeSearcher{hits: hits},
		beads:  &fakeBeads{all: all},
		log:    filepath.Join(dir, "dedupe.jsonl"),
		cursor: filepath.Join(dir, "dedupe.cursor.json"),
	}
	h.pass = &Pass{
		Asker:      h.asker,
		Searcher:   h.search,
		Beads:      h.beads,
		LogPath:    h.log,
		CursorPath: h.cursor,
		Allows:     func() (bool, error) { return h.spent < 1, nil },
		Now:        func() time.Time { return t0.Add(time.Hour) },
		Wait: func(_ context.Context, d time.Duration) error {
			h.waits = append(h.waits, d)
			return nil
		},
	}
	// A cursor from before every bead makes the first pass ask about them all.
	if err := battery.WriteCursor(h.cursor, battery.Cursor{Time: t0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return h
}

// lines reads the log back as generic maps, so the test sees every key.
func (h *harness) lines(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(h.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func mustRun(t *testing.T, p *Pass) Summary {
	t.Helper()
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sum
}

func TestJevDedupePassLogsOneLinePerFinding(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-TITLE-TEXT"
	all := []*beads.Issue{
		bead("gt-old", "Old defect in the parser", "the parser drops the last byte", 0),
		bead("gt-cmt", "Parser comment-only match", "", 1),
		bead("gt-nofield", "Parser no-field match", "", 2),
		bead("gt-fnd", "Found: parser drops the last byte "+secret, "", 10),
		bead("gt-late", "Parser drops the last byte, later copy", "", 20),
		bead("gt-plain", "Refactor the cache", "", 15),
	}
	hits := map[string][]Hit{
		all[3].Title: {
			{Kind: "bead", ID: "gt-fnd", Field: "title"},      // the finding itself
			{Kind: "bead", ID: "gt-late", Field: "title"},     // created after the finding
			{Kind: "bead", ID: "gt-cmt", Field: "comment:4"},  // matched a comment only
			{Kind: "bead", ID: "gt-nofield", Field: ""},       // no matched chunk
			{Kind: "bead", ID: "gt-old", Field: "title"},      // the duplicate
			{Kind: "bead", ID: "gt-old", Field: "title"},      // repeated hit
			{Kind: "doc", ID: "gt-doc", Field: "description"}, // not a bead
		},
	}
	h := newHarness(t, all, hits, fakeAnswer{ans: choices("gt-old", 0.9)})

	sum := mustRun(t, h.pass)
	if sum.Asked != 1 {
		t.Fatalf("Asked = %d, want 1 (only the Found bead; the plain and later beads are not findings)", sum.Asked)
	}
	if n := len(h.asker.calls); n != 1 {
		t.Fatalf("Jev asked %d times, want 1", n)
	}

	lines := h.lines(t)
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1", len(lines))
	}
	got := lines[0]
	want := map[string]any{
		"finding":            "gt-fnd",
		"candidate":          "gt-old",
		"confidence":         0.9,
		"candidates_offered": []any{"gt-old"},
		"question_version":   float64(Version),
		"model":              "jev-1.13.0",
	}
	for k, v := range want {
		if !reflect.DeepEqual(got[k], v) {
			t.Errorf("line[%q] = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["ts"].(string); !ok {
		t.Errorf("line has no ts string: %v", got)
	}
	if got["outcome"] != string(OutcomeAnswered) {
		t.Errorf("outcome = %v, want %v", got["outcome"], OutcomeAnswered)
	}
	// Every question asked is answered in the line, with no floor applied.
	wantAnswers := map[string]any{
		"duplicate":     map[string]any{"choice": "gt-old", "confidence": 0.9},
		"any_duplicate": map[string]any{"noul": 0.9},
		"fix_worthy":    map[string]any{"noul": 0.8},
		"report_only":   map[string]any{"noul": 0.1},
		"test_only":     map[string]any{"noul": 0.2},
		"severity":      map[string]any{"score": 2.0, "confidence": 0.7},
	}
	if !reflect.DeepEqual(got["answers"], wantAnswers) {
		t.Errorf("answers = %v, want %v", got["answers"], wantAnswers)
	}
	const wantKeys = "answers candidate candidates_offered confidence finding model outcome question_version ts"
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, " ") != wantKeys {
		t.Errorf("line keys = %v, want %s", keys, wantKeys)
	}

	raw, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{secret, "parser drops", "Old defect"} {
		if strings.Contains(string(raw), text) {
			t.Errorf("log holds bead text %q: %s", text, raw)
		}
	}
}

// A pass only reads the rig's beads: every call the store takes is a Since or
// a Show, so no bead is edited, labeled, commented on or closed.
func TestJevDedupePassWritesNoBeads(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
		bead("gt-two", "Found: another defect", "", 11),
	}
	hits := map[string][]Hit{
		"Found: the same defect": {{Kind: "bead", ID: "gt-old", Field: "title"}},
		"Found: another defect":  {{Kind: "bead", ID: "gt-old", Field: "description"}},
	}
	answer := fakeAnswer{ans: choices("gt-old", 0.9)}
	h := newHarness(t, all, hits, answer, answer)
	mustRun(t, h.pass)

	if len(h.beads.calls) == 0 {
		t.Fatal("the pass read no beads at all")
	}
	writes := 0
	for _, c := range h.beads.calls {
		if c != "Since" && c != "Show" {
			writes++
		}
	}
	if writes != 0 {
		t.Errorf("the pass made %d non-read calls to the bead store: %v", writes, h.beads.calls)
	}
}

func TestJevDedupeCandidateFailuresLogNone(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
	}
	hits := map[string][]Hit{"Found: the same defect": {{Kind: "bead", ID: "gt-old", Field: "title"}}}

	tests := []struct {
		name        string
		hits        map[string][]Hit
		searchErr   error
		answer      fakeAnswer
		wantAsked   int
		wantCand    string
		wantConf    float64
		wantOutcome Outcome
		wantOffers  []any
	}{
		{
			name:        "Jev times out",
			wantOutcome: OutcomeAskFailed,
			hits:        hits,
			answer:      fakeAnswer{err: jev.ErrTimeout},
			wantAsked:   1,
			wantCand:    "none",
			wantOffers:  []any{"gt-old"},
		},
		{
			name:        "Jev sends an unusable answer",
			wantOutcome: OutcomeAskFailed,
			hits:        hits,
			answer:      fakeAnswer{err: jev.ErrBadResponse},
			wantAsked:   1,
			wantCand:    "none",
			wantOffers:  []any{"gt-old"},
		},
		{
			name:        "Jev sends a 500",
			wantOutcome: OutcomeAskFailed,
			hits:        hits,
			answer:      fakeAnswer{err: &jev.StatusError{Status: 500}},
			wantAsked:   1,
			wantCand:    "none",
			wantOffers:  []any{"gt-old"},
		},
		{
			name:        "Jev returns no answers and no error",
			wantOutcome: OutcomeAskFailed,
			hits:        hits,
			answer:      fakeAnswer{},
			wantAsked:   1,
			wantCand:    "none",
			wantOffers:  []any{"gt-old"},
		},
		{
			name:        "Jev says none",
			wantOutcome: OutcomeAnswered,
			hits:        hits,
			answer:      fakeAnswer{ans: choices("none", 0.8)},
			wantAsked:   1,
			wantCand:    "none",
			wantConf:    0.8,
			wantOffers:  []any{"gt-old"},
		},
		{
			name:        "confidence below the floor",
			wantOutcome: OutcomeLowConfidence,
			hits:        hits,
			answer:      fakeAnswer{ans: choices("gt-old", 0.49)},
			wantAsked:   1,
			wantCand:    "none",
			wantConf:    0.49,
			wantOffers:  []any{"gt-old"},
		},
		{
			name:        "confidence exactly at the floor is kept",
			wantOutcome: OutcomeAnswered,
			hits:        hits,
			answer:      fakeAnswer{ans: choices("gt-old", 0.5)},
			wantAsked:   1,
			wantCand:    "gt-old",
			wantConf:    0.5,
			wantOffers:  []any{"gt-old"},
		},
		{
			name:        "an answer that names no offered option",
			wantOutcome: OutcomeUnknownOption,
			hits:        hits,
			answer:      fakeAnswer{ans: choices("gt-other", 0.95)},
			wantAsked:   1,
			wantCand:    "none",
			wantConf:    0.95,
			wantOffers:  []any{"gt-old"},
		},
		{
			name:        "search fails, so the choice is not asked",
			wantOutcome: OutcomeSearchFailed,
			hits:        hits,
			searchErr:   errors.New("hm down"),
			answer:      fakeAnswer{ans: batteryOnly()},
			wantAsked:   1,
			wantCand:    "none",
			wantOffers:  []any{},
		},
		{
			name:        "no candidate survives the filter, so the choice is not asked",
			wantOutcome: OutcomeNoCandidates,
			hits:        map[string][]Hit{"Found: the same defect": {{Kind: "bead", ID: "gt-fnd", Field: "title"}}},
			answer:      fakeAnswer{ans: batteryOnly()},
			wantAsked:   1,
			wantCand:    "none",
			wantOffers:  []any{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, all, tt.hits, tt.answer)
			h.search.err = tt.searchErr
			mustRun(t, h.pass)

			if n := len(h.asker.calls); n != tt.wantAsked {
				t.Errorf("Jev asked %d times, want %d", n, tt.wantAsked)
			}
			lines := h.lines(t)
			if len(lines) != 1 {
				t.Fatalf("log has %d lines, want 1", len(lines))
			}
			if got := lines[0]["candidate"]; got != tt.wantCand {
				t.Errorf("candidate = %v, want %v", got, tt.wantCand)
			}
			if got := lines[0]["outcome"]; got != string(tt.wantOutcome) {
				t.Errorf("outcome = %v, want %v", got, tt.wantOutcome)
			}
			if got := lines[0]["confidence"]; got != tt.wantConf {
				t.Errorf("confidence = %v, want %v", got, tt.wantConf)
			}
			if got := lines[0]["candidates_offered"]; !reflect.DeepEqual(got, tt.wantOffers) {
				t.Errorf("candidates_offered = %v, want %v", got, tt.wantOffers)
			}
		})
	}
}

func TestJevDedupeRateLimitRetriesOnce(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
	}
	hits := map[string][]Hit{"Found: the same defect": {{Kind: "bead", ID: "gt-old", Field: "title"}}}
	limited := func(after time.Duration) error {
		return &jev.RateLimitError{RetryAfter: after}
	}

	tests := []struct {
		name      string
		answers   []fakeAnswer
		wantCalls int
		wantWaits []time.Duration
		wantCand  string
	}{
		{
			name: "one 429 then an answer uses the answer",
			answers: []fakeAnswer{
				{err: limited(2 * time.Second)},
				{ans: choices("gt-old", 0.9)},
			},
			wantCalls: 2,
			wantWaits: []time.Duration{2 * time.Second},
			wantCand:  "gt-old",
		},
		{
			name: "a second 429 logs none",
			answers: []fakeAnswer{
				{err: limited(time.Second)},
				{err: limited(time.Second)},
			},
			wantCalls: 2,
			wantWaits: []time.Duration{time.Second},
			wantCand:  "none",
		},
		{
			name: "a long Retry-After waits at most the cap",
			answers: []fakeAnswer{
				{err: limited(10 * time.Minute)},
				{err: limited(0)},
			},
			wantCalls: 2,
			wantWaits: []time.Duration{maxRetryWait},
			wantCand:  "none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, all, hits, tt.answers...)
			mustRun(t, h.pass)

			if n := len(h.asker.calls); n != tt.wantCalls {
				t.Errorf("Jev asked %d times, want %d", n, tt.wantCalls)
			}
			if !reflect.DeepEqual(h.waits, tt.wantWaits) {
				t.Errorf("waits = %v, want %v", h.waits, tt.wantWaits)
			}
			if got := h.lines(t)[0]["candidate"]; got != tt.wantCand {
				t.Errorf("candidate = %v, want %v", got, tt.wantCand)
			}
		})
	}
}

func TestJevDedupeSecondPassAddsNoLines(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
	}
	hits := map[string][]Hit{"Found: the same defect": {{Kind: "bead", ID: "gt-old", Field: "title"}}}
	h := newHarness(t, all, hits, fakeAnswer{ans: choices("gt-old", 0.9)})

	if sum := mustRun(t, h.pass); sum.Asked != 1 {
		t.Fatalf("first pass Asked = %d, want 1", sum.Asked)
	}
	sum := mustRun(t, h.pass)
	if sum.Asked != 0 {
		t.Errorf("second pass Asked = %d, want 0", sum.Asked)
	}
	if n := len(h.asker.calls); n != 1 {
		t.Errorf("Jev asked %d times across two passes, want 1", n)
	}
	if n := len(h.lines(t)); n != 1 {
		t.Errorf("log has %d lines after two passes, want 1", n)
	}
}

func TestJevDedupeFirstPassOnlySetsCursor(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
	}
	hits := map[string][]Hit{"Found: the same defect": {{Kind: "bead", ID: "gt-old", Field: "title"}}}
	h := newHarness(t, all, hits, fakeAnswer{ans: choices("gt-old", 0.9)})
	if err := os.Remove(h.cursor); err != nil {
		t.Fatal(err)
	}

	sum := mustRun(t, h.pass)
	if !sum.Started || sum.Asked != 0 {
		t.Errorf("first pass = %+v, want Started and no asks", sum)
	}
	if n := len(h.asker.calls); n != 0 {
		t.Errorf("first pass asked Jev %d times, want 0", n)
	}
	if _, err := os.Stat(h.log); !os.IsNotExist(err) {
		t.Errorf("first pass wrote a log, want none: %v", err)
	}
	c, ok, err := battery.ReadCursor(h.cursor)
	if err != nil || !ok || !c.Time.Equal(t0.Add(time.Hour)) {
		t.Errorf("cursor = %+v (found %v, err %v), want the pass's own time", c, ok, err)
	}
}

func TestJevDedupeCapStopsThePass(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-a", "Found: first defect", "", 10),
		bead("gt-b", "Found: second defect", "", 11),
	}
	hits := map[string][]Hit{
		"Found: first defect":  {{Kind: "bead", ID: "gt-old", Field: "title"}},
		"Found: second defect": {{Kind: "bead", ID: "gt-old", Field: "title"}},
	}
	answer := fakeAnswer{ans: choices("gt-old", 0.9)}
	h := newHarness(t, all, hits, answer, answer)

	h.spent = 1 // the day's cap is already spent
	sum := mustRun(t, h.pass)
	if !sum.CapReached || sum.Asked != 0 {
		t.Errorf("capped pass = %+v, want CapReached and no asks", sum)
	}
	if n := len(h.asker.calls); n != 0 {
		t.Errorf("capped pass asked Jev %d times, want 0", n)
	}

	// The findings the capped pass did not reach wait for the next pass.
	h.spent = 0
	sum = mustRun(t, h.pass)
	if sum.Asked != 2 || sum.CapReached {
		t.Errorf("next pass = %+v, want both findings asked", sum)
	}
}

func TestJevDedupeCapHoldsBetweenFindings(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-a", "Found: first defect", "", 10),
		bead("gt-b", "Found: second defect", "", 11),
	}
	hits := map[string][]Hit{
		"Found: first defect":  {{Kind: "bead", ID: "gt-old", Field: "title"}},
		"Found: second defect": {{Kind: "bead", ID: "gt-old", Field: "title"}},
	}
	answer := fakeAnswer{ans: choices("gt-old", 0.9)}
	h := newHarness(t, all, hits, answer)

	// The first finding's call spends the cap, so the second waits.
	calls := 0
	h.pass.Allows = func() (bool, error) {
		calls++
		return calls == 1, nil
	}
	sum := mustRun(t, h.pass)
	if sum.Asked != 1 || !sum.CapReached {
		t.Errorf("pass = %+v, want one finding asked and the cap reached", sum)
	}
	if n := len(h.lines(t)); n != 1 {
		t.Errorf("log has %d lines, want 1", n)
	}
}

// A log line that cannot be written names the dedupe log in the error, so the
// daemon's log says which battery failed.
func TestJevDedupeLogWriteFailureNamesTheDedupeLog(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-a", "Found: first defect", "", 10),
	}
	hits := map[string][]Hit{
		"Found: first defect": {{Kind: "bead", ID: "gt-old", Field: "title"}},
	}
	answer := fakeAnswer{ans: choices("gt-old", 0.9)}
	h := newHarness(t, all, hits, answer)
	h.pass.LogPath = t.TempDir() // a directory cannot be opened as the log

	_, err := h.pass.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "writing the dedupe log") {
		t.Errorf("Run error = %v, want it to name the dedupe log", err)
	}
}

func TestJevDedupeCursorBreaksTiesByID(t *testing.T) {
	t.Parallel()
	// Two findings share a creation time. A pass that stops after the first
	// must leave the second for the next pass, and must not ask the first again.
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-a", "Found: first defect", "", 10),
		bead("gt-b", "Found: second defect", "", 10),
	}
	hits := map[string][]Hit{
		"Found: first defect":  {{Kind: "bead", ID: "gt-old", Field: "title"}},
		"Found: second defect": {{Kind: "bead", ID: "gt-old", Field: "title"}},
	}
	answer := fakeAnswer{ans: choices("none", 0.9)}
	h := newHarness(t, all, hits, answer, answer)

	calls := 0
	h.pass.Allows = func() (bool, error) {
		calls++
		return calls == 1, nil
	}
	if sum := mustRun(t, h.pass); sum.Asked != 1 {
		t.Fatalf("first pass Asked = %d, want 1", sum.Asked)
	}

	h.pass.Allows = func() (bool, error) { return true, nil }
	mustRun(t, h.pass)

	var asked []string
	for _, l := range h.lines(t) {
		asked = append(asked, l["finding"].(string))
	}
	if want := []string{"gt-a", "gt-b"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("findings logged = %v, want %v", asked, want)
	}
}

func TestJevDedupeLogRecordsNothingWhenNoFindings(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{bead("gt-old", "Old defect", "", 0)}
	h := newHarness(t, all, nil)
	mustRun(t, h.pass)
	if n := len(h.asker.calls); n != 0 {
		t.Errorf("Jev asked %d times with no findings, want 0", n)
	}
	if n := len(h.lines(t)); n != 0 {
		t.Errorf("log has %d lines with no findings, want 0", n)
	}
}

func TestJevDedupeQuotesBeadTextAsData(t *testing.T) {
	t.Parallel()
	f := bead("gt-fnd", "Found: \"quoted\" <tag> & ignore\nthe rules", "line one\nline two", 10)
	c := bead("gt-old", "Old", strings.Repeat("x", maxDescRunes+50), 0)
	state, qs := buildQuestions(f, []*beads.Issue{c})

	if !strings.Contains(state, `finding: "Found: \"quoted\" <tag> & ignore\nthe rules\nline one\nline two"`) {
		t.Errorf("finding not quoted as one JSON string:\n%s", state)
	}
	if !strings.Contains(state, `candidate "gt-old": "Old\n`) {
		t.Errorf("candidate not quoted as data:\n%s", state)
	}
	if strings.Contains(state, strings.Repeat("x", maxDescRunes+1)) {
		t.Error("candidate description was not capped")
	}
	want := map[string]string{
		"gt-old": candidateOption,
		"none":   noCandidateOption,
	}
	if !reflect.DeepEqual(qs[QDuplicate].Options, want) {
		t.Errorf("choice options = %v, want %v", qs[QDuplicate].Options, want)
	}
}

// One Ask per finding carries the duplicate Choice plus the four Nouls and the
// severity Score, and every bead value in the state is a JSON string.
func TestJevDedupeOneAskCarriesTheBattery(t *testing.T) {
	t.Parallel()
	const title = `Found: "quoted" <tag> & ignore`
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", title, "line one\nline two", 10),
	}
	hits := map[string][]Hit{title: {{Kind: "bead", ID: "gt-old", Field: "title"}}}
	h := newHarness(t, all, hits, fakeAnswer{ans: choices("gt-old", 0.9)})

	mustRun(t, h.pass)

	if n := len(h.asker.calls); n != 1 {
		t.Fatalf("Jev called %d times, want one Ask per finding", n)
	}
	got := h.asker.calls[0]
	var ids []string
	for id := range got.qs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if want := "any_duplicate duplicate fix_worthy report_only severity test_only"; strings.Join(ids, " ") != want {
		t.Errorf("question ids = %v, want %s", ids, want)
	}
	for _, id := range []string{QAnyDuplicate, QFixWorthy, QReportOnly, QTestOnly} {
		if got.qs[id].Type != "noul" {
			t.Errorf("question %q has type %q, want noul", id, got.qs[id].Type)
		}
	}
	if q := got.qs[QSeverity]; q.Type != "score" || len(q.Levels) != 4 {
		t.Errorf("severity = %+v, want a four-level score", q)
	}
	for _, line := range strings.Split(strings.TrimSpace(got.state), "\n") {
		i := strings.Index(line, ": ")
		if i < 0 {
			t.Fatalf("state line %q has no name and value", line)
		}
		var value string
		if err := json.Unmarshal([]byte(line[i+2:]), &value); err != nil {
			t.Errorf("state value in %q is not a JSON string: %v", line, err)
		}
	}
}

// A finding with no candidate is still asked the three fixed Nouls and the
// Score, and its line carries them. any_duplicate has no candidate to judge,
// so it is not asked.
func TestJevDedupeNoCandidatesLineCarriesTheBattery(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: a lone defect", "", 10),
	}
	h := newHarness(t, all, map[string][]Hit{}, fakeAnswer{ans: batteryOnly()})

	mustRun(t, h.pass)

	if n := len(h.asker.calls); n != 1 {
		t.Fatalf("Jev called %d times, want the battery asked without a choice", n)
	}
	if _, ok := h.asker.calls[0].qs[QDuplicate]; ok {
		t.Error("the duplicate Choice was asked with no candidate offered")
	}
	if _, ok := h.asker.calls[0].qs[QAnyDuplicate]; ok {
		t.Error("any_duplicate was asked with no candidate offered")
	}
	lines := h.lines(t)
	if len(lines) != 1 || lines[0]["outcome"] != string(OutcomeNoCandidates) {
		t.Fatalf("log = %v, want one no_candidates line", lines)
	}
	want := map[string]any{
		"fix_worthy":  map[string]any{"noul": 0.8},
		"report_only": map[string]any{"noul": 0.1},
		"test_only":   map[string]any{"noul": 0.2},
		"severity":    map[string]any{"score": 2.0, "confidence": 0.7},
	}
	if !reflect.DeepEqual(lines[0]["answers"], want) {
		t.Errorf("answers = %v, want %v", lines[0]["answers"], want)
	}
	if lines[0]["question_version"] != float64(Version) || lines[0]["model"] != "jev-1.13.0" {
		t.Errorf("line = %v, want the version and model", lines[0])
	}
}

func TestJevDedupeReadRecent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dedupe.jsonl")
	if lines, err := ReadRecent(path, 5); err != nil || lines != nil {
		t.Fatalf("missing log = %v, %v; want empty and no error", lines, err)
	}
	for _, id := range []string{"gt-1", "gt-2", "gt-3"} {
		if err := battery.Append(path, Line{TS: t0, Finding: id, Candidate: "none", CandidatesOffered: []string{}}); err != nil {
			t.Fatal(err)
		}
	}
	lines, err := ReadRecent(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0].Finding != "gt-2" || lines[1].Finding != "gt-3" {
		t.Errorf("ReadRecent(2) = %+v, want the last two lines, oldest first", lines)
	}
}

// A pass stopped by its own context (a daemon shutdown, the run budget) says
// nothing about the finding it was asking: it logs no line and leaves the
// cursor, so the next pass asks the finding again.
func TestJevDedupeCancelledContextLeavesTheFindingForTheNextPass(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
	}
	hits := map[string][]Hit{"Found: the same defect": {{Kind: "bead", ID: "gt-old", Field: "title"}}}
	good := fakeAnswer{ans: choices("gt-old", 0.9)}

	tests := []struct {
		name string
		// cancelAt wires the cancel into the step the context ends in.
		cancelAt func(h *harness, cancel context.CancelFunc)
	}{
		{
			name: "during the hm search",
			cancelAt: func(h *harness, cancel context.CancelFunc) {
				h.search.onSearch = func() error { cancel(); return context.Canceled }
			},
		},
		{
			name: "during the Jev call",
			cancelAt: func(h *harness, cancel context.CancelFunc) {
				h.asker.onAsk = func() error { cancel(); return context.Canceled }
			},
		},
		{
			name: "while waiting out a 429",
			cancelAt: func(h *harness, cancel context.CancelFunc) {
				h.asker.script = []fakeAnswer{{err: &jev.RateLimitError{RetryAfter: time.Second}}}
				h.pass.Wait = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, all, hits, good)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tt.cancelAt(h, cancel)

			_, err := h.pass.Run(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run error = %v, want context.Canceled", err)
			}
			if n := len(h.lines(t)); n != 0 {
				t.Fatalf("log has %d lines after a cancelled pass, want 0", n)
			}

			// A later pass with a live context asks the same finding.
			h.search.onSearch, h.asker.onAsk = nil, nil
			h.asker.script = []fakeAnswer{good}
			h.pass.Wait = func(context.Context, time.Duration) error { return nil }
			if sum := mustRun(t, h.pass); sum.Asked != 1 {
				t.Errorf("next pass Asked = %d, want 1", sum.Asked)
			}
			lines := h.lines(t)
			if len(lines) != 1 || lines[0]["candidate"] != "gt-old" || lines[0]["outcome"] != string(OutcomeAnswered) {
				t.Errorf("log after the next pass = %v, want one answered line for gt-old", lines)
			}
		})
	}
}

// A failure that is not the context's end still logs none and moves on, so one
// bad finding cannot hold the queue.
func TestJevDedupeFailureWithLiveContextAdvancesTheCursor(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
	}
	hits := map[string][]Hit{"Found: the same defect": {{Kind: "bead", ID: "gt-old", Field: "title"}}}
	h := newHarness(t, all, hits, fakeAnswer{err: &jev.StatusError{Status: 500}})

	mustRun(t, h.pass)
	mustRun(t, h.pass)

	lines := h.lines(t)
	if len(lines) != 1 || lines[0]["outcome"] != string(OutcomeAskFailed) || lines[0]["candidate"] != "none" {
		t.Errorf("log = %v, want one ask_failed none line", lines)
	}
}

// A manual pass and the daemon's pass share one cursor, so a pass that finds
// the other one running stops without touching the log or the cursor.
func TestJevDedupeRunRefusesWhileAnotherPassHoldsTheLock(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
	}
	hits := map[string][]Hit{"Found: the same defect": {{Kind: "bead", ID: "gt-old", Field: "title"}}}
	h := newHarness(t, all, hits, fakeAnswer{ans: choices("gt-old", 0.9)})

	release, err := lockRun(h.cursor)
	if err != nil {
		t.Fatalf("lockRun: %v", err)
	}
	_, err = h.pass.Run(context.Background())
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Run error = %v, want ErrBusy", err)
	}
	if n := len(h.asker.calls) + len(h.lines(t)); n != 0 {
		t.Errorf("a refused pass asked or logged %d times, want 0", n)
	}

	release()
	if sum := mustRun(t, h.pass); sum.Asked != 1 {
		t.Errorf("Asked = %d after the lock was released, want 1", sum.Asked)
	}
}

// A candidate whose bead cannot be read says nothing about whether the
// finding is a duplicate. The pass must not log the run as "no candidates",
// and the cursor still advances so the finding is not asked again.
func TestCandidatesShowFailureIsNotLoggedAsNoCandidates(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
	}
	// Every hit names a bead the store cannot show.
	hits := map[string][]Hit{"Found: the same defect": {
		{Kind: "bead", ID: "gt-gone", Field: "title"},
		{Kind: "bead", ID: "gt-alsogone", Field: "description"},
	}}
	h := newHarness(t, all, hits, fakeAnswer{ans: batteryOnly()})

	if sum := mustRun(t, h.pass); sum.Asked != 1 {
		t.Fatalf("Asked = %d, want 1", sum.Asked)
	}
	lines := h.lines(t)
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1", len(lines))
	}
	if got := lines[0]["outcome"]; got != string(OutcomeReadFailed) {
		t.Errorf("outcome = %v, want %v: an unreadable candidate is not the same as none", got, OutcomeReadFailed)
	}
	if got := lines[0]["candidate"]; got != "none" {
		t.Errorf("candidate = %v, want none", got)
	}
	if got := lines[0]["candidates_offered"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("candidates_offered = %v, want none offered", got)
	}

	// The finding was answered about, so the next pass leaves it alone.
	mustRun(t, h.pass)
	if n := len(h.asker.calls); n != 1 {
		t.Errorf("Jev asked %d times across two passes, want 1", n)
	}
}

// A short write leaves a partial line with no newline. The next append must
// not merge into it: that would lose both entries when the merged line fails
// to parse.
func TestAppendLineAfterAPartialLineKeepsTheNextEntry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dedupe.jsonl")
	partial := `{"ts":"2026-10-19T12:00:00Z","finding":"gt-cut`
	if err := os.WriteFile(path, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}

	next := Line{TS: t0, Finding: "gt-next", Candidate: "none", CandidatesOffered: []string{}}
	if err := battery.Append(path, next); err != nil {
		t.Fatalf("Append: %v", err)
	}
	lines, err := ReadRecent(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Finding != "gt-next" {
		t.Fatalf("ReadRecent = %+v, want the new entry intact", lines)
	}
	if lines[0].TS != next.TS.UTC() {
		t.Errorf("new entry ts = %v, want %v", lines[0].TS, next.TS)
	}
}

// A paid answer whose log line cannot be written must still advance the
// cursor: otherwise the same finding is asked, and billed, on every pass.
func TestRunDoesNotReAskWhenTheLogIsUnwritable(t *testing.T) {
	t.Parallel()
	all := []*beads.Issue{
		bead("gt-old", "Old defect", "", 0),
		bead("gt-fnd", "Found: the same defect", "", 10),
	}
	hits := map[string][]Hit{"Found: the same defect": {{Kind: "bead", ID: "gt-old", Field: "title"}}}
	h := newHarness(t, all, hits, fakeAnswer{ans: choices("gt-old", 0.9)})

	// A directory where the log should be makes every append fail.
	if err := os.Mkdir(h.log, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pass.Run(context.Background()); err == nil {
		t.Fatal("Run reported no error with an unwritable log")
	}
	if n := len(h.asker.calls); n != 1 {
		t.Fatalf("first pass asked Jev %d times, want 1", n)
	}

	// The next pass must not pay for the same finding again.
	_, _ = h.pass.Run(context.Background())
	if n := len(h.asker.calls); n != 1 {
		t.Errorf("Jev asked %d times across two passes, want 1: an unlogged answer was re-billed", n)
	}
}
