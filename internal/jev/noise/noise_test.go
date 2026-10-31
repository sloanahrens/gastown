package noise

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev/logtriage"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// at is the time h hours after t0.
func at(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

// fp is a pointer to v, for the optional fields of a Row.
func fp(v float64) *float64 { return &v }

func noulAns(p float64) logtriage.Answer  { return logtriage.Answer{Noul: &p} }
func scoreAns(s float64) logtriage.Answer { return logtriage.Answer{Score: &s} }
func choiceAns(c string) logtriage.Answer { return logtriage.Answer{Choice: c} }

// original is a battery log line for key at ts, asked under the current question set.
func original(key string, ts time.Time, answers map[string]logtriage.Answer) logtriage.Line {
	return logtriage.Line{TS: ts, Key: key, QuestionVersion: logtriage.Version, Answers: answers}
}

// repeatsOfNoul is one repeat per value, each giving the noul answer under qid.
func repeatsOfNoul(key string, ts time.Time, qid string, vals ...float64) []Repeat {
	var out []Repeat
	for _, v := range vals {
		out = append(out, Repeat{TS: ts, Key: key, QuestionVersion: logtriage.Version,
			Answers: map[string]logtriage.Answer{qid: noulAns(v)}})
	}
	return out
}

// repeatsOfChoice is one repeat per pick, each giving the choice answer under qid.
func repeatsOfChoice(key string, ts time.Time, qid string, picks ...string) []Repeat {
	var out []Repeat
	for _, p := range picks {
		out = append(out, Repeat{TS: ts, Key: key, QuestionVersion: logtriage.Version,
			Answers: map[string]logtriage.Answer{qid: choiceAns(p)}})
	}
	return out
}

// assertRow checks the fields want sets; a nil pointer in want is not checked.
func assertRow(t *testing.T, got, want Row) {
	t.Helper()
	assert.Equal(t, want.Question, got.Question, "question")
	assert.Equal(t, want.Kind, got.Kind, "kind")
	assert.Equal(t, want.Items, got.Items, "items")
	assertNear(t, "mean_sd", want.MeanSD, got.MeanSD)
	assertNear(t, "max_sd", want.MaxSD, got.MaxSD)
	assertNear(t, "cross_share", want.CrossShare, got.CrossShare)
	assertNear(t, "width90", want.Width90, got.Width90)
	assertNear(t, "changed", want.Changed, got.Changed)
}

func assertNear(t *testing.T, name string, want, got *float64) {
	t.Helper()
	if want == nil {
		return
	}
	if got == nil {
		t.Errorf("%s is nil, want %v", name, *want)
		return
	}
	assert.InDelta(t, *want, *got, 1e-9, name)
}

// TestNoiseStatistics is a table over fixtures of one original and its repeats:
// identical repeats give zero spread and no cross, repeats that straddle the cut
// cross it, an item with too few repeats or no original answer is skipped, and
// a choice counts a changed pick.
func TestNoiseStatistics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		originals   []logtriage.Line
		repeats     []Repeat
		opts        Options
		wantSkipped int
		want        []Row
	}{
		{
			name:      "identical repeats have zero spread and no cross",
			originals: []logtriage.Line{original("k", at(0), map[string]logtriage.Answer{"q": noulAns(0.7)})},
			repeats:   repeatsOfNoul("k", at(1), "q", 0.7, 0.7, 0.7),
			opts:      Options{Cut: 0.5},
			want:      []Row{{Question: "q", Kind: KindNoul, Items: 1, MeanSD: fp(0), MaxSD: fp(0), CrossShare: fp(0), Width90: fp(0)}},
		},
		{
			name:      "repeats 0.43 to 0.53 around a 0.5 cut cross it",
			originals: []logtriage.Line{original("k", at(0), map[string]logtriage.Answer{"q": noulAns(0.43)})},
			repeats:   repeatsOfNoul("k", at(1), "q", 0.53, 0.43, 0.53),
			opts:      Options{Cut: 0.5},
			want:      []Row{{Question: "q", Kind: KindNoul, Items: 1, MeanSD: fp(0.0577350269), MaxSD: fp(0.0577350269), CrossShare: fp(1), Width90: fp(0.1)}},
		},
		{
			name:        "an item with fewer than three repeats is skipped",
			originals:   []logtriage.Line{original("k", at(0), map[string]logtriage.Answer{"q": noulAns(0.2)})},
			repeats:     repeatsOfNoul("k", at(1), "q", 0.2, 0.2),
			opts:        Options{Cut: 0.5},
			wantSkipped: 1,
			want:        []Row{},
		},
		{
			name:        "an original with no answer is skipped",
			originals:   []logtriage.Line{original("k", at(0), nil)},
			repeats:     repeatsOfNoul("k", at(1), "q", 0.2, 0.2, 0.2),
			opts:        Options{Cut: 0.5},
			wantSkipped: 1,
			want:        []Row{},
		},
		{
			name: "a choice counts a changed pick",
			originals: []logtriage.Line{
				original("k1", at(0), map[string]logtriage.Answer{"c": choiceAns("a")}),
				original("k2", at(0), map[string]logtriage.Answer{"c": choiceAns("c")}),
			},
			repeats: append(
				repeatsOfChoice("k1", at(1), "c", "a", "b", "a"),
				repeatsOfChoice("k2", at(1), "c", "c", "c", "c")...,
			),
			opts: Options{Cut: 0.5},
			want: []Row{{Question: "c", Kind: KindChoice, Items: 2, Changed: fp(0.5)}},
		},
		{
			name:      "a score is read as its level index",
			originals: []logtriage.Line{original("k", at(0), map[string]logtriage.Answer{"s": scoreAns(1)})},
			repeats: []Repeat{
				{TS: at(1), Key: "k", QuestionVersion: logtriage.Version, Answers: map[string]logtriage.Answer{"s": scoreAns(1)}},
				{TS: at(1), Key: "k", QuestionVersion: logtriage.Version, Answers: map[string]logtriage.Answer{"s": scoreAns(2)}},
				{TS: at(1), Key: "k", QuestionVersion: logtriage.Version, Answers: map[string]logtriage.Answer{"s": scoreAns(1)}},
			},
			opts: Options{Cut: 0.5},
			want: []Row{{Question: "s", Kind: KindScore, Items: 1, MeanSD: fp(0.5), MaxSD: fp(0.5), CrossShare: fp(0), Width90: fp(1)}},
		},
		{
			name:      "a repeat pairs with the original of its own pass",
			originals: []logtriage.Line{original("k", at(0), map[string]logtriage.Answer{"q": noulAns(0.1)}), original("k", at(2), map[string]logtriage.Answer{"q": noulAns(0.9)})},
			repeats:   append(repeatsOfNoul("k", at(1), "q", 0.1, 0.1, 0.1), repeatsOfNoul("k", at(3), "q", 0.9, 0.9, 0.9)...),
			opts:      Options{Cut: 0.5},
			want:      []Row{{Question: "q", Kind: KindNoul, Items: 2, MeanSD: fp(0), MaxSD: fp(0), CrossShare: fp(0), Width90: fp(0)}},
		},
		{
			name: "an item before the window is neither counted nor skipped",
			originals: []logtriage.Line{
				original("old", at(0), map[string]logtriage.Answer{"q": noulAns(0.2)}),
				original("new", at(6), map[string]logtriage.Answer{"q": noulAns(0.2)}),
			},
			repeats: append(repeatsOfNoul("old", at(1), "q", 0.2, 0.2, 0.2), repeatsOfNoul("new", at(7), "q", 0.2, 0.2, 0.2)...),
			opts:    Options{Since: at(5), Cut: 0.5},
			want:    []Row{{Question: "q", Kind: KindNoul, Items: 1, MeanSD: fp(0), MaxSD: fp(0), CrossShare: fp(0), Width90: fp(0)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rep, err := Compute(tc.originals, tc.repeats, tc.opts)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSkipped, rep.Skipped, "skipped items")
			require.Len(t, rep.Rows, len(tc.want))
			for i := range tc.want {
				assertRow(t, rep.Rows[i], tc.want[i])
			}
		})
	}
}

// TestNoiseGroupsByKeyAndVersion: a repeat under another question version never
// pairs with an original, and two keys never share repeats. Each group's spread
// is zero, so a group that picked up the other's or another version's answers
// would show a spread instead.
func TestNoiseGroupsByKeyAndVersion(t *testing.T) {
	t.Parallel()
	originals := []logtriage.Line{
		original("a", at(0), map[string]logtriage.Answer{"q": noulAns(0.1)}),
		original("b", at(0), map[string]logtriage.Answer{"q": noulAns(0.9)}),
	}
	otherVersion := func(key string, vals ...float64) []Repeat {
		rs := repeatsOfNoul(key, at(1), "q", vals...)
		for i := range rs {
			rs[i].QuestionVersion = logtriage.Version + 1
		}
		return rs
	}
	repeats := append(repeatsOfNoul("a", at(1), "q", 0.1, 0.1, 0.1), otherVersion("a", 0.9, 0.9, 0.9)...)
	repeats = append(repeats, repeatsOfNoul("b", at(1), "q", 0.9, 0.9, 0.9)...)

	rep, err := Compute(originals, repeats, Options{Cut: 0.5})
	require.NoError(t, err)
	require.Len(t, rep.Rows, 1)
	assertRow(t, rep.Rows[0], Row{Question: "q", Kind: KindNoul, Items: 2, MeanSD: fp(0), MaxSD: fp(0), CrossShare: fp(0), Width90: fp(0)})
	assert.Equal(t, 0, rep.Skipped)
}

// TestNoiseWidthDropsTheTopFivePercent: the width is the widest span among the
// middle 90 percent of items, so one wide item out of twenty does not set it.
func TestNoiseWidthDropsTheTopFivePercent(t *testing.T) {
	t.Parallel()
	var originals []logtriage.Line
	var repeats []Repeat
	for i := range 20 {
		key := fmt.Sprintf("k%02d", i)
		originals = append(originals, original(key, at(0), map[string]logtriage.Answer{"q": noulAns(0.2)}))
		vals := []float64{0.3, 0.2, 0.2}
		if i == 0 {
			vals = []float64{0.7, 0.2, 0.2}
		}
		repeats = append(repeats, repeatsOfNoul(key, at(1), "q", vals...)...)
	}

	rep, err := Compute(originals, repeats, Options{Cut: 0.5})
	require.NoError(t, err)
	require.Len(t, rep.Rows, 1)
	assertRow(t, rep.Rows[0], Row{Question: "q", Kind: KindNoul, Items: 20, Width90: fp(0.1)})
}

// TestNoiseRejectsAQuestionAnsweredAsTwoKinds: one question id holding a noul
// in one item and a choice in another is a corrupt log, so the report refuses.
func TestNoiseRejectsAQuestionAnsweredAsTwoKinds(t *testing.T) {
	t.Parallel()
	originals := []logtriage.Line{
		original("k1", at(0), map[string]logtriage.Answer{"q": noulAns(0.2)}),
		original("k2", at(0), map[string]logtriage.Answer{"q": choiceAns("a")}),
	}
	repeats := append(repeatsOfNoul("k1", at(1), "q", 0.2, 0.2, 0.2), repeatsOfChoice("k2", at(1), "q", "a", "a", "a")...)

	_, err := Compute(originals, repeats, Options{Cut: 0.5})
	assert.ErrorContains(t, err, "question q was answered as")
}
