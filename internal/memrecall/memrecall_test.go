package memrecall

import (
	"encoding/json"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// testNow is the clock every test scores against, so a recency window is
// arithmetic on a fixed date and not on the wall clock.
var testNow = time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

// TestSharedCases checks the contract in testdata/cases.json, which the Python
// tooling reads as the same rule. The file is the fixture; do not edit it to
// make a failure pass.
func TestSharedCases(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "cases.json"))
	if err != nil {
		t.Fatalf("reading the shared cases: %v", err)
	}
	var cases struct {
		Keywords []struct {
			Key    string   `json:"key"`
			Body   string   `json:"body"`
			Expect []string `json:"expect"`
		} `json:"keywords"`
		Overlap []struct {
			A      []string `json:"a"`
			B      []string `json:"b"`
			Expect float64  `json:"expect"`
		} `json:"overlap"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parsing the shared cases: %v", err)
	}
	if len(cases.Keywords) == 0 || len(cases.Overlap) == 0 {
		t.Fatal("the shared cases file holds no cases")
	}

	for _, tc := range cases.Keywords {
		if got, want := keywordList(Keywords(tc.Key, tc.Body)), sortedCopy(tc.Expect); !slices.Equal(got, want) {
			t.Errorf("Keywords(%q, %q) = %v, want %v", tc.Key, tc.Body, got, want)
		}
	}
	for _, tc := range cases.Overlap {
		a, b := setOf(tc.A), setOf(tc.B)
		if got := Overlap(a, b); math.Abs(got-tc.Expect) > 1e-9 {
			t.Errorf("Overlap(%v, %v) = %v, want %v", tc.A, tc.B, got, tc.Expect)
		}
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		key        string
		value      string
		wantType   string
		wantBody   string
		wantScope  string
		wantAnchor []string
		durable    bool
		always     bool
		verified   time.Time
	}{{
		name: "modern header block",
		key:  "memory.always-check-argv",
		value: "recorded: 2026-10-14 by gastown/crew/sloan (bead gt-xyz)\n" +
			"verified: 2026-09-01\n" +
			"type: project\n" +
			"durable: true\n" +
			"scope: town\n" +
			"always: true\n" +
			"anchors: file:internal/cmd/prime.go; symbol:shouldRenderMemories; bead:gt-abc\n" +
			"supersedes: none\n" +
			"\n" +
			"Body line one.\nBody line two.\n",
		wantType:  "project",
		wantBody:  "Body line one.\nBody line two.\n",
		wantScope: "town",
		wantAnchor: []string{
			"file:internal/cmd/prime.go",
			"symbol:shouldRenderMemories",
			"bead:gt-abc",
		},
		durable:  true,
		always:   true,
		verified: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}, {
		name:      "legacy value takes its type from the key",
		key:       "gt.feedback.absence-is-not-evidence",
		value:     "Before treating an absence as evidence, check.\n\n[recorded by: sloan @ 2026-09-30 at 12:03]",
		wantType:  "feedback",
		wantBody:  "Before treating an absence as evidence, check.\n\n[recorded by: sloan @ 2026-09-30 at 12:03]",
		wantScope: "rig",
		verified:  time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
	}, {
		// An unknown first segment is not a type: the spec names four, and
		// anything else is general.
		name:      "legacy value with an unknown type segment",
		key:       "gt.housekeeping.tidy-the-kv",
		value:     "Old value with no marker.",
		wantType:  "general",
		wantBody:  "Old value with no marker.",
		wantScope: "rig",
	}, {
		name:      "legacy memory.<slug> key with no marker",
		key:       "memory.refinery-worktree",
		value:     "Points at the refinery worktree.",
		wantType:  "general",
		wantBody:  "Points at the refinery worktree.",
		wantScope: "rig",
	}, {
		name:      "body running on without a blank line",
		key:       "memory.done-runs-once",
		value:     "type: feedback\nSomething else entirely.",
		wantType:  "feedback",
		wantBody:  "Something else entirely.",
		wantScope: "rig",
	}, {
		name:      "headers with no body",
		key:       "memory.header-only",
		value:     "verified: 2026-10-01\ndurable: false\nalways: false",
		wantType:  "general",
		wantBody:  "",
		wantScope: "rig",
		verified:  time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}, {
		name:      "malformed date leaves the memory unverified",
		key:       "memory.bad-date",
		value:     "verified: yesterday\n\nBody.",
		wantType:  "general",
		wantBody:  "Body.",
		wantScope: "rig",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Parse(tc.key, tc.value)
			if got.Key != tc.key {
				t.Errorf("Key = %q, want %q", got.Key, tc.key)
			}
			if got.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tc.wantType)
			}
			if got.Body != tc.wantBody {
				t.Errorf("Body = %q, want %q", got.Body, tc.wantBody)
			}
			if got.Scope != tc.wantScope {
				t.Errorf("Scope = %q, want %q", got.Scope, tc.wantScope)
			}
			if !slices.Equal(got.Anchors, tc.wantAnchor) {
				t.Errorf("Anchors = %v, want %v", got.Anchors, tc.wantAnchor)
			}
			if got.Durable != tc.durable {
				t.Errorf("Durable = %v, want %v", got.Durable, tc.durable)
			}
			if got.Always != tc.always {
				t.Errorf("Always = %v, want %v", got.Always, tc.always)
			}
			if !got.Verified.Equal(tc.verified) {
				t.Errorf("Verified = %v, want %v", got.Verified, tc.verified)
			}
		})
	}
}

// TestBodyKeywords pins the one difference from Keywords: the whole body is
// read, not only its first non-empty line.
func TestBodyKeywords(t *testing.T) {
	t.Parallel()
	body := "First line.\n\nSecond line names dolt.\n"
	if got := keywordList(BodyKeywords(body)); !slices.Contains(got, "dolt") {
		t.Errorf("BodyKeywords(%q) = %v, want it to hold dolt", body, got)
	}
	if got := keywordList(Keywords("memory.notes", body)); slices.Contains(got, "dolt") {
		t.Errorf("Keywords(%q) = %v, want only the first line's terms", body, got)
	}
}

func TestQuery(t *testing.T) {
	t.Parallel()
	got := Query(
		"memrecall: score rig memories",
		"Fills a budget. Dolt is not involved.",
		[]string{"memory", "polecat"},
		[]string{"internal/cmd/prime.go", "docs/writing-for-agents.md"},
	)
	for _, want := range []string{"memrecall", "score", "rig", "memories", "budget", "dolt", "involved", "memory", "polecat", "internal", "cmd", "prime", "docs", "writing", "agents"} {
		if _, ok := got[want]; !ok {
			t.Errorf("Query missed %q; got %v", want, keywordList(got))
		}
	}
	// A two-character path segment never becomes a term, and neither does a
	// stopword.
	for _, unwanted := range []string{"go", "the", "not", "is"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("Query kept %q; got %v", unwanted, keywordList(got))
		}
	}
}

func TestScore(t *testing.T) {
	t.Parallel()
	mem := func(key, body string) Memory {
		return Parse(key, "recorded: 2026-10-14 by gastown/crew/sloan\n\n"+body)
	}

	tests := []struct {
		name     string
		query    []string
		paths    []string
		memory   Memory
		wantRaw  float64
		wantScor float64
	}{{
		name:     "a key hit alone clears the floor",
		query:    []string{"presubmit"},
		memory:   mem("memory.gt-done-runs-presubmit-once", "It runs once."),
		wantRaw:  2,
		wantScor: 1.2,
	}, {
		name:    "a body hit alone does not",
		query:   []string{"window"},
		memory:  mem("memory.quiet-hours", "The window is quiet."),
		wantRaw: 1,
		// Below the floor; Score still reports the weighted value, and Select
		// is what refuses it.
		wantScor: 0.6,
	}, {
		name:    "a body word already counted in the key is not counted twice",
		query:   []string{"presubmit"},
		memory:  mem("memory.gt-done-runs-presubmit-once", "Presubmit runs once."),
		wantRaw: 2, wantScor: 1.2,
	}, {
		name:     "a file anchor the bead names adds two",
		query:    []string{"presubmit"},
		paths:    []string{"internal/cmd/prime.go"},
		memory:   Parse("memory.gt-done-runs-presubmit-once", "verified: 2026-01-01\nanchors: file:internal/cmd/prime.go\n\nIt runs once."),
		wantRaw:  4,
		wantScor: 2.4,
	}, {
		name:    "a symbol anchor naming the same path adds nothing",
		query:   []string{"presubmit"},
		paths:   []string{"internal/cmd/prime.go"},
		memory:  Parse("memory.gt-done-runs-presubmit-once", "anchors: symbol:internal/cmd/prime.go\n\nIt runs once."),
		wantRaw: 2, wantScor: 1.2,
	}, {
		name:    "feedback outweighs a reference of the same raw score",
		query:   []string{"presubmit"},
		memory:  Parse("memory.note-presubmit", "type: feedback\n\nBody."),
		wantRaw: 2, wantScor: 2.6,
	}, {
		name:    "an unknown type weighs as general",
		query:   []string{"presubmit"},
		memory:  Parse("memory.note-presubmit", "type: housekeeping\n\nBody."),
		wantRaw: 2, wantScor: 1.2,
	}, {
		name:     "verified exactly 30 days ago earns recency",
		query:    []string{"presubmit"},
		memory:   Parse("memory.note-presubmit", "type: feedback\nverified: 2026-09-14\n\nBody."),
		wantRaw:  2,
		wantScor: 3.1,
	}, {
		name:     "the day before the window does not",
		query:    []string{"presubmit"},
		memory:   Parse("memory.note-presubmit", "type: feedback\nverified: 2026-09-13\n\nBody."),
		wantRaw:  2,
		wantScor: 2.6,
	}, {
		name:     "an unknown date earns nothing",
		query:    []string{"presubmit"},
		memory:   Parse("memory.note-presubmit", "type: feedback\n\nBody."),
		wantRaw:  2,
		wantScor: 2.6,
	}, {
		name:     "no query terms, no score",
		query:    nil,
		memory:   mem("memory.gt-done-runs-presubmit-once", "It runs once."),
		wantRaw:  0,
		wantScor: 0,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, score := Score(setOf(tc.query), tc.paths, tc.memory, testNow)
			if raw != tc.wantRaw {
				t.Errorf("raw = %v, want %v", raw, tc.wantRaw)
			}
			if math.Abs(score-tc.wantScor) > 1e-9 {
				t.Errorf("score = %v, want %v", score, tc.wantScor)
			}
		})
	}
}

func TestOverlap(t *testing.T) {
	t.Parallel()
	// The smaller set is the denominator whichever argument it arrives as:
	// one shared term of two.
	a, b := setOf([]string{"dolt", "gc"}), setOf([]string{"dolt", "window", "quiet"})
	for _, got := range []float64{Overlap(a, b), Overlap(b, a)} {
		if math.Abs(got-0.5) > 1e-9 {
			t.Errorf("Overlap = %v, want 0.5", got)
		}
	}
	// A subset is a whole match: every term of the smaller set is shared.
	if got := Overlap(setOf([]string{"dolt"}), setOf([]string{"dolt", "gc"})); got != 1 {
		t.Errorf("Overlap of a subset = %v, want 1", got)
	}
	if got := Overlap(setOf(nil), setOf(nil)); got != 0 {
		t.Errorf("Overlap of two empty sets = %v, want 0", got)
	}
}

func TestSelectOrder(t *testing.T) {
	t.Parallel()
	q := setOf([]string{"dolt"})
	mems := []Memory{
		// Equal scores, so the newer verification decides.
		{Key: "memory.older-dolt-note", Body: "dolt", Type: "general", Scope: "rig",
			Verified: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{Key: "memory.newer-dolt-note", Body: "dolt", Type: "general", Scope: "rig",
			Verified: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		// Equal score and date, so the key decides.
		{Key: "memory.aaa-dolt-note", Body: "dolt", Type: "general", Scope: "rig"},
		{Key: "memory.bbb-dolt-note", Body: "dolt", Type: "general", Scope: "rig"},
		// Highest score of all: feedback's weight lifts it over the others.
		{Key: "memory.feedback-dolt-note", Body: "dolt", Type: "feedback", Scope: "rig"},
	}

	got := Select(q, nil, mems, testNow, 0)
	want := []string{
		"memory.feedback-dolt-note",
		"memory.newer-dolt-note",
		"memory.older-dolt-note",
		"memory.aaa-dolt-note",
		"memory.bbb-dolt-note",
	}
	if keys := selectedKeys(got); !slices.Equal(keys, want) {
		t.Errorf("Select order = %v, want %v", keys, want)
	}
}

func TestSelectAlways(t *testing.T) {
	t.Parallel()
	// Every pinned memory scores zero against the query, so nothing but the
	// always-and-durable rule can select them — and they still come before the
	// high-scoring candidate.
	mems := []Memory{
		{Key: "rig-a", Body: "nothing", Always: true, Durable: true, Scope: "rig"},
		{Key: "rig-b", Body: "nothing", Always: true, Durable: true, Scope: "rig"},
		{Key: "rig-c", Body: "nothing", Always: true, Durable: true, Scope: "rig"},
		{Key: "rig-d", Body: "nothing", Always: true, Durable: true, Scope: "rig"},
		{Key: "town-a", Body: "nothing", Always: true, Durable: true, Scope: "town"},
		{Key: "memory.dolt-candidate", Body: "dolt", Scope: "rig"},
		// Always without durable is not pinned; it needs a score like any other.
		{Key: "half-pinned", Body: "nothing", Always: true, Scope: "rig"},
	}

	got := Select(setOf([]string{"dolt"}), nil, mems, testNow, 0)
	want := []string{"rig-a", "rig-b", "rig-c", "town-a", "memory.dolt-candidate"}
	if keys := selectedKeys(got); !slices.Equal(keys, want) {
		t.Errorf("Select = %v, want %v (three pinned per scope, then the candidate)", keys, want)
	}
}

func TestSelectBudget(t *testing.T) {
	t.Parallel()
	large := Memory{Key: "memory.aaa-dolt-large", Body: "dolt " + strings.Repeat("padding ", 200), Scope: "rig"}
	small := Memory{Key: "memory.bbb-dolt-small", Body: "dolt", Scope: "rig"}
	// Scores tie, so the key decides the order: large, then small, then last;
	// the large one is the one that cannot fit.
	last := Memory{Key: "memory.zzz-dolt-last", Body: "dolt", Scope: "rig"}
	smallText := renderMemory(small)
	lastText := renderMemory(last)

	got := Select(setOf([]string{"dolt"}), nil, []Memory{large, small, last}, testNow, len(smallText)+len(lastText))
	if keys := selectedKeys(got); !slices.Equal(keys, []string{"memory.bbb-dolt-small", "memory.zzz-dolt-last"}) {
		t.Fatalf("Select = %v, want the two that fit", keys)
	}
	if got[1].Text != lastText {
		t.Errorf("the memory after the skipped one is not truncated: %q, want %q", got[1].Text, lastText)
	}
	if total := len(got[0].Text) + len(got[1].Text); total > len(smallText)+len(lastText) {
		t.Errorf("the section holds %d bytes, over the budget", total)
	}
}

func TestSelectBudgetDefault(t *testing.T) {
	t.Parallel()
	var mems []Memory
	for i := range 100 {
		mems = append(mems, Memory{
			Key:   "memory.dolt-note-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Body:  strings.Repeat("dolt ", 40),
			Scope: "rig",
		})
	}
	got := Select(setOf([]string{"dolt"}), nil, mems, testNow, 0)
	if len(got) == 0 {
		t.Fatal("Select returned nothing")
	}
	total := 0
	for _, s := range got {
		total += len(s.Text)
	}
	if total > Budget {
		t.Errorf("section is %d bytes, over the default budget %d", total, Budget)
	}
	if len(got) == len(mems) {
		t.Error("the default budget bounded nothing")
	}
}

func TestSelectDropsBelowFloor(t *testing.T) {
	t.Parallel()
	mems := []Memory{{Key: "memory.quiet-hours", Body: "The window is quiet.", Scope: "rig"}}
	if got := Select(setOf([]string{"window"}), nil, mems, testNow, 0); len(got) != 0 {
		t.Errorf("Select = %v, want nothing: a body-only hit is below the floor", selectedKeys(got))
	}
}

func TestSelectRendersWholeMemory(t *testing.T) {
	t.Parallel()
	mems := []Memory{
		{Key: "memory.dolt-verified", Body: "dolt\nsecond line", Scope: "rig",
			Verified: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{Key: "memory.dolt-unverified", Body: "dolt", Scope: "rig"},
	}
	got := Select(setOf([]string{"dolt"}), nil, mems, testNow, 0)
	if len(got) != 2 {
		t.Fatalf("Select = %v, want both", selectedKeys(got))
	}
	if want := "### memory.dolt-verified (verified 2026-10-01)\ndolt\nsecond line\n"; got[0].Text != want {
		t.Errorf("Text = %q, want %q", got[0].Text, want)
	}
	if want := "### memory.dolt-unverified (unverified)\ndolt\n"; got[1].Text != want {
		t.Errorf("Text = %q, want %q", got[1].Text, want)
	}
}

// keywordList lists a term set in a stable order for comparison and messages.
func keywordList(terms map[string]struct{}) []string {
	return sortedCopy(slices.Collect(maps.Keys(terms)))
}

// setOf builds a term set from a list, as the shared cases express one.
func setOf(words []string) map[string]struct{} {
	out := make(map[string]struct{}, len(words))
	for _, w := range words {
		out[w] = struct{}{}
	}
	return out
}

// sortedCopy sorts a copy of words, so a comparison does not depend on the
// order a case lists them in.
func sortedCopy(words []string) []string {
	out := slices.Clone(words)
	slices.Sort(out)
	return out
}

// selectedKeys lists the keys of a selection in order.
func selectedKeys(selected []Selected) []string {
	out := make([]string, 0, len(selected))
	for _, s := range selected {
		out = append(out, s.Key)
	}
	return out
}
