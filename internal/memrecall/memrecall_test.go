package memrecall

import (
	"encoding/json"
	"fmt"
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
		name:      "legacy value takes a user type from the key",
		key:       "gt.user.prefs",
		value:     "Prefers table-driven tests.",
		wantType:  "user",
		wantBody:  "Prefers table-driven tests.",
		wantScope: "rig",
	}, {
		// An unknown first segment is not a type: the format defines five, and
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

// TestKeyType pins the types a key can name, including user, which the prime
// index listed long before recall scoring recognized it (gt-fg4rh).
func TestKeyType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		want string
	}{
		{"gt.user.prefs", "user"},
		{"gt.feedback.absence-is-not-evidence", "feedback"},
		{"gt.project.merge-freeze", "project"},
		{"gt.reference.grafana-dashboard", "reference"},
		{"gt.general.some-insight", "general"},
		{"gt.housekeeping.tidy-the-kv", "general"},
		{"gt.refinery-worktree", "general"},
		{"memory.user.prefs", "user"},
		{"memory.feedback.always-use-race-flag", "feedback"},
		{"memory.refinery-worktree", "general"},
		{"gt.", "general"},
		{"memory.", "general"},
		{"elsewhere.key", "general"},
	}
	for _, tc := range tests {
		if got := KeyType(tc.key); got != tc.want {
			t.Errorf("KeyType(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// TestKeySlug pins that a typed key's slug excludes the type segment in either
// namespace, so the type word does not also score as a key word (gt-fg4rh).
func TestKeySlug(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		want string
	}{
		{"gt.user.prefs", "prefs"},
		{"gt.feedback.absence-is-not-evidence", "absence-is-not-evidence"},
		{"gt.housekeeping.tidy-the-kv", "housekeeping.tidy-the-kv"},
		{"gt.refinery-worktree", "refinery-worktree"},
		{"memory.user.prefs", "prefs"},
		{"memory.feedback.always-use-race-flag", "always-use-race-flag"},
		{"memory.refinery-worktree", "refinery-worktree"},
		{"gt.", ""},
		{"memory.", ""},
		{"elsewhere.key", "elsewhere.key"},
	}
	for _, tc := range tests {
		if got := KeySlug(tc.key); got != tc.want {
			t.Errorf("KeySlug(%q) = %q, want %q", tc.key, got, tc.want)
		}
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

// TestCorpusIDF pins the weight every match is worth: ln(1 + N/df).
func TestCorpusIDF(t *testing.T) {
	t.Parallel()
	c := NewCorpus([]Memory{
		note("dolt-notes", "dolt"),
		note("dolt-more", "dolt"),
		note("quiet-window", "dolt"),
	})

	if c.N != 3 {
		t.Errorf("N = %d, want 3", c.N)
	}
	if got, want := c.IDF("dolt"), math.Log(2); math.Abs(got-want) > 1e-9 {
		t.Errorf("IDF of a word every memory holds = %v, want ln 2 = %v", got, want)
	}
	if got, want := c.IDF("window"), math.Log(4); math.Abs(got-want) > 1e-9 {
		t.Errorf("IDF of a word one memory of three holds = %v, want ln 4 = %v", got, want)
	}
	if got := c.IDF("absent"); got != 0 {
		t.Errorf("IDF of a word no memory holds = %v, want 0", got)
	}
	if got := NewCorpus(nil).IDF("dolt"); got != 0 {
		t.Errorf("IDF in an empty pool = %v, want 0", got)
	}
}

// TestScore pins the idf-weighted raw score: what a matched keyword is worth,
// and where in the memory it was found. The pool is part of every case, because
// a keyword is worth its rarity in that pool.
func TestScore(t *testing.T) {
	t.Parallel()

	parse := func(key, body string) Memory {
		return Parse(key, "recorded: 2026-10-14 by gastown/crew/sloan\n\n"+body)
	}

	tests := []struct {
		name string
		// query and paths are the bead side; memories is the whole pool and
		// target names the memory under test.
		query    []string
		paths    []string
		memories []Memory
		target   int
		wantRaw  float64
		wantScor float64
	}{{
		name:     "a key hit weighs its idf twice",
		query:    []string{"presubmit"},
		memories: inPool(4, parse("memory.presubmit-note", "It runs once.")),
		wantRaw:  2 * math.Log(6),
		wantScor: 2 * math.Log(6) * 0.6,
	}, {
		name:     "a body hit weighs its idf once",
		query:    []string{"window"},
		memories: inPool(4, parse("memory.quiet-hours", "The window is quiet.")),
		wantRaw:  math.Log(6),
		wantScor: math.Log(6) * 0.6,
	}, {
		// The pool's common word: every memory holds it, so it weighs ln 2
		// whatever the pool's size.
		name:  "a word in every memory weighs ln 2",
		query: []string{"dolt"},
		memories: []Memory{
			parse("memory.note-one", "dolt"),
			parse("memory.note-two", "dolt"),
			parse("memory.note-three", "dolt"),
			parse("memory.note-four", "dolt"),
		},
		wantRaw:  math.Log(2),
		wantScor: math.Log(2) * 0.6,
	}, {
		name:  "the key side of a word weighs twice its body side",
		query: []string{"window"},
		memories: []Memory{
			parse("memory.window-note", "Nothing here."),
			parse("memory.quiet-note", "The window is quiet."),
			parse("memory.third-note", "Nothing here."),
			parse("memory.fourth-note", "Nothing here."),
		},
		wantRaw:  2 * math.Log(3),
		wantScor: 2 * math.Log(3) * 0.6,
	}, {
		name:  "the body side of the same word weighs it once",
		query: []string{"window"},
		memories: []Memory{
			parse("memory.window-note", "Nothing here."),
			parse("memory.quiet-note", "The window is quiet."),
			parse("memory.third-note", "Nothing here."),
			parse("memory.fourth-note", "Nothing here."),
		},
		target:   1,
		wantRaw:  math.Log(3),
		wantScor: math.Log(3) * 0.6,
	}, {
		name:     "a body word the key already holds is not counted twice",
		query:    []string{"presubmit"},
		memories: inPool(4, parse("memory.presubmit-note", "Presubmit runs once.")),
		wantRaw:  2 * math.Log(6),
		wantScor: 2 * math.Log(6) * 0.6,
	}, {
		name:     "a file anchor the bead names weighs 2*ln(1+N)",
		query:    []string{"presubmit"},
		paths:    []string{"internal/cmd/prime.go"},
		memories: inPool(4, Parse("memory.anchor-note", "anchors: file:internal/cmd/prime.go\n\nIt runs once.")),
		wantRaw:  2 * math.Log(6),
		wantScor: 2 * math.Log(6) * 0.6,
	}, {
		name:     "a symbol anchor naming the same path weighs nothing",
		query:    []string{"presubmit"},
		paths:    []string{"internal/cmd/prime.go"},
		memories: inPool(4, Parse("memory.anchor-note", "anchors: symbol:internal/cmd/prime.go\n\nIt runs once.")),
		wantRaw:  0,
		wantScor: 0,
	}, {
		name:  "feedback outweighs a reference of the same raw score",
		query: []string{"presubmit"},
		memories: []Memory{
			Parse("memory.feedback-presubmit", "type: feedback\n\nBody."),
			Parse("memory.reference-presubmit", "type: reference\n\nBody."),
		},
		wantRaw:  2 * math.Log(2),
		wantScor: 2 * math.Log(2) * 1.3,
	}, {
		name:  "an unknown type weighs as general",
		query: []string{"presubmit"},
		memories: []Memory{
			Parse("memory.feedback-presubmit", "type: housekeeping\n\nBody."),
			Parse("memory.reference-presubmit", "type: reference\n\nBody."),
		},
		wantRaw:  2 * math.Log(2),
		wantScor: 2 * math.Log(2) * 0.6,
	}, {
		name:     "verified exactly 30 days ago earns recency",
		query:    []string{"presubmit"},
		memories: inPool(4, Parse("memory.presubmit-note", "type: feedback\nverified: 2026-09-14\n\nBody.")),
		wantRaw:  2 * math.Log(6),
		wantScor: 2*math.Log(6)*1.3 + 0.5,
	}, {
		name:     "the day before the window does not",
		query:    []string{"presubmit"},
		memories: inPool(4, Parse("memory.presubmit-note", "type: feedback\nverified: 2026-09-13\n\nBody.")),
		wantRaw:  2 * math.Log(6),
		wantScor: 2 * math.Log(6) * 1.3,
	}, {
		name:     "an unknown date earns nothing",
		query:    []string{"presubmit"},
		memories: inPool(4, Parse("memory.presubmit-note", "type: feedback\n\nBody.")),
		wantRaw:  2 * math.Log(6),
		wantScor: 2 * math.Log(6) * 1.3,
	}, {
		name:     "no query terms, no score",
		query:    nil,
		memories: inPool(4, parse("memory.presubmit-note", "It runs once.")),
		wantRaw:  0,
		wantScor: 0,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, score := Score(setOf(tc.query), tc.paths, tc.memories[tc.target], NewCorpus(tc.memories), testNow)
			if math.Abs(raw-tc.wantRaw) > 1e-9 {
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

// TestSelectRareWordOnly is the acceptance case: a pool of long memories that
// all hold the town's common words, where one also holds a word of its own.
// Only that memory is about the bead.
func TestSelectRareWordOnly(t *testing.T) {
	t.Parallel()
	got := Select(
		setOf([]string{"gastown", "polecat", "bead", "daemon", "kittens"}),
		nil, longCorpus(), testNow, 0)
	if want := []string{"memory.kittens-grooming"}; !slices.Equal(selectedKeys(got), want) {
		t.Errorf("Select = %v, want only the memory holding the bead's rare word", selectedKeys(got))
	}
}

// TestSelectCommonWordsOnly is the other half: a bead that shares only the
// town's common words selects nothing, which is what omits the section.
func TestSelectCommonWordsOnly(t *testing.T) {
	t.Parallel()
	corpus := longCorpus()
	q := setOf([]string{"gastown", "polecat", "bead", "daemon"})

	if got := Select(q, nil, corpus, testNow, 0); len(got) != 0 {
		t.Errorf("Select = %v, want nothing: a pool-common word is no evidence a memory is about this bead", selectedKeys(got))
	}

	rows := Rank(q, nil, corpus, testNow, 0)
	if len(rows) != len(corpus) {
		t.Fatalf("Rank returned %d rows for a %d-memory pool", len(rows), len(corpus))
	}
	for _, row := range rows {
		if row.Matched() {
			t.Errorf("%s matched a bead sharing only pool-common words", row.Memory.Key)
		}
		if row.Raw >= CandidateFloor {
			t.Errorf("%s scored raw %v, at or over the floor %v", row.Memory.Key, row.Raw, CandidateFloor)
		}
	}
}

// TestDistinctiveShareBoundary pins the share test at its edge: in a pool of
// ten, a word two memories hold (df/N = 0.2) is distinctive and a word three
// hold is not.
func TestDistinctiveShareBoundary(t *testing.T) {
	t.Parallel()
	// A distinctive word of its own keeps the memory a candidate, so the
	// boundary word's classification is what the assertion reads.
	subject := note("widget-gizmo-note", "Nothing else.")

	distinctive := func(t *testing.T, holders int) []string {
		t.Helper()
		mems := append([]Memory{subject}, note("widget-first-note", "Nothing else."))
		for range holders - 2 {
			mems = append(mems, note("widget-other-note", "Nothing else."))
		}
		mems = append(mems, filler(10-len(mems))...)
		if len(mems) != 10 {
			t.Fatalf("the pool is %d memories, want 10", len(mems))
		}
		rows := Rank(setOf([]string{"widget", "gizmo"}), nil, mems, testNow, 0)
		for _, row := range rows {
			if row.Memory.Key == subject.Key {
				return row.Distinctive
			}
		}
		t.Fatalf("%s is not in the ranking", subject.Key)
		return nil
	}

	if hits := distinctive(t, 2); !slices.Contains(hits, "key:widget") {
		t.Errorf("df/N exactly 0.2: distinctive hits = %v, want the boundary word among them", hits)
	}
	if hits := distinctive(t, 3); slices.Contains(hits, "key:widget") {
		t.Errorf("df/N 0.3: distinctive hits = %v, want the boundary word left out", hits)
	}
}

// TestSelectSmallPool covers the rule for a pool too small to read a share
// from: a key hit is distinctive there, and a body hit is not.
func TestSelectSmallPool(t *testing.T) {
	t.Parallel()
	// Eight memories that all hold the bead's three words, four of them in the
	// key and four in the body. The share is 1.0 for all three words, so only
	// the small-pool rule can qualify anything — and three key hits reach the
	// floor.
	mems := []Memory{
		note("alpha-bravo-charlie-one", "Nothing else."),
		note("alpha-bravo-charlie-two", "Nothing else."),
		note("alpha-bravo-charlie-three", "Nothing else."),
		note("alpha-bravo-charlie-four", "Nothing else."),
		note("quiet-one", "Alpha, bravo and charlie, said every memory."),
		note("quiet-two", "Alpha, bravo and charlie, said every memory."),
		note("quiet-three", "Alpha, bravo and charlie, said every memory."),
		note("quiet-four", "Alpha, bravo and charlie, said every memory."),
	}
	q := setOf([]string{"alpha", "bravo", "charlie"})

	got := Select(q, nil, mems, testNow, 0)
	if len(got) != MaxScored {
		t.Errorf("Select = %v, want the %d best of a pool where a key hit is distinctive", selectedKeys(got), MaxScored)
	}
	for _, row := range Rank(q, nil, mems, testNow, 0) {
		if row.Memory.Key == "memory.quiet-one" && row.Matched() {
			t.Errorf("a body-only match in a small pool matched, so the body side of the rule is not in force")
		}
	}

	// Enough matched body words clear the floor and still select nothing: at
	// this pool size what makes a memory distinctive is a key hit or an anchor,
	// and a body word is neither.
	bodyOnly := note("quiet-note", "Alpha, bravo, charlie, delta, echo and foxtrot, all in the body.")
	bodyQuery := setOf([]string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"})
	rows := Rank(bodyQuery, nil, inPool(8, bodyOnly), testNow, 0)
	if rows[0].Memory.Key != bodyOnly.Key || rows[0].Raw < CandidateFloor {
		t.Fatalf("the body-only memory scored raw %v at the head of the ranking, want it over the floor %v: this case tests nothing",
			rows[0].Raw, CandidateFloor)
	}
	if rows[0].Dropped != DroppedNoDistinctive {
		t.Errorf("a body-only match in a small pool was dropped for %q, want %q", rows[0].Dropped, DroppedNoDistinctive)
	}
}

// TestSelectOrder pins the ranking: the score first, then newer verification,
// then key, so a corpus renders the same section on every run.
func TestSelectOrder(t *testing.T) {
	t.Parallel()
	t.Run("equal scores fall back to the key", func(t *testing.T) {
		t.Parallel()
		mems := pool(10,
			note("bravo-note", "Matched in the key."),
			note("alpha-note", "Matched in the key."),
			note("charlie-note", "Matched in the key."),
		)
		got := Select(setOf([]string{"alpha", "bravo", "charlie"}), nil, mems, testNow, 0)
		want := []string{"memory.alpha-note", "memory.bravo-note", "memory.charlie-note"}
		if keys := selectedKeys(got); !slices.Equal(keys, want) {
			t.Errorf("Select order = %v, want %v", keys, want)
		}
	})

	t.Run("a newer verification wins a tie in words", func(t *testing.T) {
		t.Parallel()
		fresh := note("zzz-alpha-note", "Matched in the key.")
		fresh.Type, fresh.Verified = "general", testNow.Add(-24*time.Hour)
		stale := note("aaa-bravo-note", "Matched in the key.")
		got := Select(setOf([]string{"alpha", "bravo"}), nil, pool(10, fresh, stale), testNow, 0)
		if want := []string{"memory.zzz-alpha-note", "memory.aaa-bravo-note"}; !slices.Equal(selectedKeys(got), want) {
			t.Errorf("Select = %v, want the verified memory first despite its later key", selectedKeys(got))
		}
	})

	t.Run("a heavier type wins the same raw score", func(t *testing.T) {
		t.Parallel()
		guidance := note("zzz-alpha-note", "Matched in the key.")
		guidance.Type = "feedback"
		pointer := note("aaa-bravo-note", "Matched in the key.")
		rows := Rank(setOf([]string{"alpha", "bravo"}), nil, pool(10, guidance, pointer), testNow, 0)
		if want := []string{"memory.zzz-alpha-note", "memory.aaa-bravo-note"}; !slices.Equal(rankedKeys(rows[:2]), want) {
			t.Errorf("Rank = %v, want feedback ahead of a general of the same raw score", rankedKeys(rows[:2]))
		}
	})
}

func TestSelectAlways(t *testing.T) {
	t.Parallel()
	// Every pinned memory scores zero against the query, so nothing but the
	// always-and-durable rule can select them — and they still come before the
	// high-scoring candidate.
	mems := append([]Memory{
		{Key: "rig-a", Body: "nothing", Always: true, Durable: true, Scope: "rig"},
		{Key: "rig-b", Body: "nothing", Always: true, Durable: true, Scope: "rig"},
		{Key: "rig-c", Body: "nothing", Always: true, Durable: true, Scope: "rig"},
		{Key: "rig-d", Body: "nothing", Always: true, Durable: true, Scope: "rig"},
		{Key: "town-a", Body: "nothing", Always: true, Durable: true, Scope: "town"},
		// Always without durable is not pinned; it needs a score like any other.
		{Key: "half-pinned", Body: "nothing", Always: true, Scope: "rig"},
	}, note("dolt-spindle-window-note", "Matched in the key."))

	q := setOf([]string{"dolt", "spindle", "window"})
	got := Select(q, nil, mems, testNow, 0)
	want := []string{"rig-a", "rig-b", "rig-c", "town-a", "memory.dolt-spindle-window-note"}
	if keys := selectedKeys(got); !slices.Equal(keys, want) {
		t.Errorf("Select = %v, want %v (three pinned per scope, then the candidate)", keys, want)
	}
	for _, row := range Rank(q, nil, mems, testNow, 0) {
		if row.Memory.Key != "rig-d" {
			continue
		}
		if row.Dropped != DroppedOverPinnedCap || row.Matched() {
			t.Errorf("the fourth pinned memory is %q and matched=%v, want it turned away as %q and no match",
				row.Dropped, row.Matched(), DroppedOverPinnedCap)
		}
	}
}

func TestSelectBudget(t *testing.T) {
	t.Parallel()
	large := note("aaa-alpha-large", "alpha "+strings.Repeat("padding ", 200))
	small := note("bbb-bravo-small", "bravo")
	// Scores tie, so the key decides the order: large, then small, then last;
	// the large one is the one that cannot fit.
	last := note("ccc-charlie-last", "charlie")
	mems := pool(10, large, small, last)
	smallText, lastText := renderMemory(small), renderMemory(last)

	got := Select(setOf([]string{"alpha", "bravo", "charlie"}), nil, mems, testNow, len(smallText)+len(lastText))
	if keys := selectedKeys(got); !slices.Equal(keys, []string{"memory.bbb-bravo-small", "memory.ccc-charlie-last"}) {
		t.Fatalf("Select = %v, want the two that fit", keys)
	}
	if got[1].Text != lastText {
		t.Errorf("the memory after the skipped one is not truncated: %q, want %q", got[1].Text, lastText)
	}
	if total := len(got[0].Text) + len(got[1].Text); total > len(smallText)+len(lastText) {
		t.Errorf("the section holds %d bytes, over the budget", total)
	}

	for _, row := range Rank(setOf([]string{"alpha", "bravo", "charlie"}), nil, mems, testNow, len(smallText)+len(lastText)) {
		if row.Memory.Key == large.Key && (row.Dropped != DroppedNoBudget || !row.Matched()) {
			t.Errorf("the memory that did not fit is %q and matched=%v, want it reported as %q and still a match",
				row.Dropped, row.Matched(), DroppedNoBudget)
		}
	}
}

// TestSelectDefaultBudget pins the default: a non-positive budget means Budget,
// which bounds the section below the cap of MaxScored.
func TestSelectDefaultBudget(t *testing.T) {
	t.Parallel()
	mems := pool(10,
		note("alpha-note", "alpha "+strings.Repeat("padding ", 100)),
		note("bravo-note", "bravo "+strings.Repeat("padding ", 100)),
		note("charlie-note", "charlie "+strings.Repeat("padding ", 100)),
	)
	got := Select(setOf([]string{"alpha", "bravo", "charlie"}), nil, mems, testNow, 0)
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
	if len(got) == MaxScored {
		t.Error("three memories of this size fit the default budget, so the budget bounded nothing")
	}
}

// TestSelectCap pins the cap: however many memories clear the bar, the section
// carries MaxScored of them.
func TestSelectCap(t *testing.T) {
	t.Parallel()
	q := setOf([]string{"alpha", "bravo", "charlie", "delta"})
	mems := pool(10,
		note("alpha-note", "Matched in the key."),
		note("bravo-note", "Matched in the key."),
		note("charlie-note", "Matched in the key."),
		note("delta-note", "Matched in the key."),
	)

	want := []string{"memory.alpha-note", "memory.bravo-note", "memory.charlie-note"}
	if keys := selectedKeys(Select(q, nil, mems, testNow, 0)); !slices.Equal(keys, want) {
		t.Errorf("Select = %v, want the %d best", keys, MaxScored)
	}

	for _, row := range Rank(q, nil, mems, testNow, 0) {
		if row.Memory.Key != "memory.delta-note" {
			continue
		}
		if row.Dropped != DroppedOverCap || !row.Matched() {
			t.Errorf("the fourth match is %q and matched=%v, want it cut as %q and still a match",
				row.Dropped, row.Matched(), DroppedOverCap)
		}
	}
}

// TestSelectKeepShareCut pins the other cut: from a top score, a memory more
// than twice as far below it is no longer worth rendering.
func TestSelectKeepShareCut(t *testing.T) {
	t.Parallel()
	// Three, two and one key hits of the query's own words: raw scores in the
	// ratio 3:2:1, so 60% of the top keeps the first two.
	top := note("alpha-bravo-charlie-note", "Matched in the key.")
	mid := note("delta-echo-note", "Matched in the key.")
	low := note("foxtrot-note", "Matched in the key.")
	q := setOf([]string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"})
	mems := pool(10, top, mid, low)

	if keys := selectedKeys(Select(q, nil, mems, testNow, 0)); !slices.Equal(keys, []string{top.Key, mid.Key}) {
		t.Errorf("Select = %v, want the two within %v of the top score", keys, KeepShare)
	}

	for _, row := range Rank(q, nil, mems, testNow, 0) {
		if row.Memory.Key != low.Key {
			continue
		}
		if row.Dropped != DroppedBelowTop || !row.Matched() {
			t.Errorf("the lowest scorer is %q and matched=%v, want it cut as %q and still a match",
				row.Dropped, row.Matched(), DroppedBelowTop)
		}
	}
}

// TestRankReportsTheMatchReasons pins the two ways a memory fails to be a
// match: no distinctive hit, and a distinctive hit under the floor.
func TestRankReportsTheMatchReasons(t *testing.T) {
	t.Parallel()
	// Ten memories. Every one holds "dolt" in its body, so dolt weighs ln 2 and
	// a body hit under the floor; "widget" is held by two of them, which is
	// exactly the distinctive share, but one key hit does not reach the floor.
	common := note("common-note", "Dolt again.")
	boundary := note("widget-gizmo-note", "Nothing else.")
	mems := []Memory{
		common,
		boundary,
		note("widget-other-note", "Nothing else."),
	}
	// Eight of the ten memories hold dolt, so it is this pool's own common word.
	for i := range 7 {
		mems = append(mems, note(fmt.Sprintf("dolt-note-%02d", i), "Dolt again."))
	}

	reasons := map[string]DropReason{}
	for _, row := range Rank(setOf([]string{"widget", "dolt"}), nil, mems, testNow, 0) {
		reasons[row.Memory.Key] = row.Dropped
	}
	if got := reasons[common.Key]; got != DroppedNoDistinctive {
		t.Errorf("a pool-common word alone reported %q, want %q", got, DroppedNoDistinctive)
	}
	if got := reasons[boundary.Key]; got != DroppedBelowFloor {
		t.Errorf("a distinctive hit under the floor reported %q, want %q", got, DroppedBelowFloor)
	}
}

// TestSelectRendersWholeMemory pins the text a memory contributes: its heading
// with the date it was last verified, then its body whole.
func TestSelectRendersWholeMemory(t *testing.T) {
	t.Parallel()
	verified := note("dolt-alpha-note", "dolt\nsecond line")
	verified.Verified = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	got := Select(setOf([]string{"alpha", "bravo"}), nil, pool(10, verified, note("bravo-note", "dolt")), testNow, 0)
	if len(got) != 2 {
		t.Fatalf("Select = %v, want both", selectedKeys(got))
	}
	if want := "### memory.dolt-alpha-note (verified 2026-10-01)\ndolt\nsecond line\n"; got[0].Text != want {
		t.Errorf("Text = %q, want %q", got[0].Text, want)
	}
	if want := "### memory.bravo-note (unverified)\ndolt\n"; got[1].Text != want {
		t.Errorf("Text = %q, want %q", got[1].Text, want)
	}
}

// note builds a memory from a key slug and a body, the shape a legacy value
// parses to.
func note(slug, body string) Memory {
	return Memory{Key: "memory." + slug, Body: body, Type: "general", Scope: "rig"}
}

// filler returns n memories that share no term with a test's query, so the idf
// weights are measured against a pool rather than against the two memories a
// test cares about. A query word in a filler key or body would move df, which
// is why the filler is about nothing else.
func filler(n int) []Memory {
	out := make([]Memory, 0, n)
	for i := range n {
		out = append(out, note(fmt.Sprintf("filler-note-%02d", i), "Unrelated chatter about gardening and bread."))
	}
	return out
}

// pool lists mems behind n filler memories.
func pool(n int, mems ...Memory) []Memory {
	return append(filler(n), mems...)
}

// inPool puts one memory in front of n filler memories.
func inPool(n int, mem Memory) []Memory {
	return append([]Memory{mem}, filler(n)...)
}

// longCorpus is the acceptance fixture: twelve long memories that all hold the
// town's common words, and one of them a word of its own.
func longCorpus() []Memory {
	const common = "In gastown a polecat takes one bead at a time, and the daemon watches the rest of the town."
	mems := []Memory{note("kittens-grooming", common+" The kittens are brushed here.")}
	for i := range 11 {
		mems = append(mems, note(fmt.Sprintf("long-note-%02d", i), common))
	}
	return mems
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

// rankedKeys lists the keys of rows in the order they are ranked.
func rankedKeys(rows []Ranked) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Memory.Key)
	}
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
