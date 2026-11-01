package dashboard

import (
	"strings"
	"testing"
)

// The om panel's collapsed line is the line the operator watches while reading
// everything else: the review log's first-pass share, the reviews om rejected,
// the rework share and the loops out. The reviewer's own configuration — model,
// depth, threshold, timeout — is drawn in the panel's body, which a reader who
// wants it opens (gt-bz0pn). Where the threshold came from rides that body line
// as a hover, not as text (gt-7swd8). Every test here carries OMHeader in its
// name, which is the pattern gt-7swd8's gate runs; gt-bz0pn's gate matches the
// TestOM prefix, which the same names also satisfy.

// omPanelState is the om read the server sends with the review log beside it.
func omPanelState(rework any, threshold any, source string) map[string]any {
	om := map[string]any{
		"model": "haiku", "depth": "standard", "timeout_s": 900,
		"windows": []any{}, "days": []any{}, "scores": make([]any, 10),
		"rejects": map[string]any{}, "routes": map[string]any{}, "recent": []any{},
	}
	if threshold != nil {
		om["threshold"] = threshold
	}
	if source != "" {
		om["threshold_source"] = source
	}
	s := map[string]any{"om": om}
	if rework != nil {
		s["rework"] = rework
	}
	return s
}

// omReviewLog is a review log's latest window under two bars: 25 beads, 14
// approved on their first review, 50 reviews, 14 of them rejected, 3 loops out.
func omReviewLog() map[string]any {
	return map[string]any{"windows": []any{map[string]any{
		"name": "last 7 days", "from": "2026-10-25T00:00:00Z", "to": "2026-11-01T00:00:00Z",
		"bars": []any{
			map[string]any{"bar": 0.75, "reviews": 40, "rejected": 8, "beads": 20, "first_try": 13, "landed": 18, "reviews_per_landed": 2.0, "loops_out": 2, "mean_score": 0.8},
			map[string]any{"bar": 0, "reviews": 10, "rejected": 6, "beads": 5, "first_try": 1, "landed": 3, "reviews_per_landed": 1.5, "loops_out": 1, "mean_score": 0.7},
		},
	}}}
}

func drawOMPanel(t *testing.T, state map[string]any) *pageDOM {
	t.Helper()
	dom := newPageDOM(t)
	dom.call(t, "renderOM", state)
	return dom
}

func TestOMHeaderLineCarriesTheReviewFigures(t *testing.T) {
	t.Parallel()

	got := drawOMPanel(t, omPanelState(omReviewLog(), 0.75, "gastown .om.json")).element(t, "omnote")
	if want := "56% first pass · 14 rejected · 28% rework · 3 loops out"; got.Text != want {
		t.Errorf("the collapsed line reads %q, want %q", got.Text, want)
	}
	for _, config := range []string{"haiku", "standard", "threshold", "timeout", ".om.json"} {
		if strings.Contains(got.Text, config) {
			t.Errorf("the collapsed line still carries its configuration (%q): %q", config, got.Text)
		}
	}
}

// The line's hover says which stretch of the review log the figures count and
// what each means, so a figure that looks wrong can be checked without opening
// the panel.
func TestOMHeaderLineHoversWhatItCounts(t *testing.T) {
	t.Parallel()

	got := drawOMPanel(t, omPanelState(omReviewLog(), 0.75, "gastown .om.json")).element(t, "omnote")
	for _, want := range []string{"first pass", "rework", "rejected"} {
		if !strings.Contains(got.Title, want) {
			t.Errorf("the collapsed line's hover does not explain %q: %q", want, got.Title)
		}
	}
}

func TestOMHeaderPanelBodyCarriesTheConfig(t *testing.T) {
	t.Parallel()

	dom := drawOMPanel(t, omPanelState(omReviewLog(), 0.75, "gastown .om.json"))
	cfg := dom.cellWithText(t, "om", "haiku · standard · threshold 0.75 · 900s timeout")
	if cfg.Title != "gastown .om.json" {
		t.Errorf("the config line's title is %q, want the threshold's source", cfg.Title)
	}
	if got := dom.element(t, "omnote"); strings.Contains(got.Text, "threshold") {
		t.Errorf("the collapsed line still reads the config: %q", got.Text)
	}
}

func TestOMHeaderConfigKeepsTheReasonWhenTheThresholdIsUnknown(t *testing.T) {
	t.Parallel()

	dom := drawOMPanel(t, omPanelState(omReviewLog(), nil, "rig .om.json unreadable"))
	dom.cellWithText(t, "om", "haiku · standard · threshold unknown (rig .om.json unreadable) · 900s timeout")
}

// A review log that could not be read is a named state on the line, never a
// week with no rework.
func TestOMHeaderLineMarksAnUnreadableReviewLog(t *testing.T) {
	t.Parallel()

	got := drawOMPanel(t, omPanelState(map[string]any{"unavailable": true}, 0.75, "")).element(t, "omnote")
	if want := "om rework unreadable"; got.Text != want {
		t.Errorf("the collapsed line reads %q, want %q", got.Text, want)
	}
	if strings.Contains(got.Text, "%") {
		t.Errorf("an unreadable review log still reads as figures: %q", got.Text)
	}
	if want := "the review log could not be read, so the om figures are unknown"; got.Title != want {
		t.Errorf("the collapsed line's hover is %q, want %q", got.Title, want)
	}
}

// A review log the server has not sent yet contributes no figure: a missing
// reading is not a week of 0%.
func TestOMHeaderLineDrawsNoFigureBeforeTheReviewLogArrives(t *testing.T) {
	t.Parallel()

	got := drawOMPanel(t, omPanelState(nil, 0.75, "")).element(t, "omnote")
	if got.Text != "" {
		t.Errorf("the collapsed line reads %q before the review log has been read, want nothing", got.Text)
	}
}

// A bar the reader sent with no reviews must not divide by zero: the first-pass
// share still reads, and the share of an empty window is left out rather than
// drawn as a rate over nothing.
func TestOMHeaderLineDrawsNoShareForAWindowWithNoReviews(t *testing.T) {
	t.Parallel()

	log := map[string]any{"windows": []any{map[string]any{"bars": []any{
		map[string]any{"reviews": 0, "beads": 2, "first_try": 1},
	}}}}
	got := drawOMPanel(t, omPanelState(log, 0.75, "")).element(t, "omnote")
	if want := "50% first pass"; got.Text != want {
		t.Errorf("the collapsed line reads %q, want %q", got.Text, want)
	}
}

// A review log that arrived with no window in it names that on the line: a blank
// collapsed line would read as the log not having been read yet.
func TestOMHeaderLineSaysAReviewLogWithNoWindowHoldsNone(t *testing.T) {
	t.Parallel()

	got := drawOMPanel(t, omPanelState(map[string]any{"windows": []any{}}, 0.75, "")).element(t, "omnote")
	if want := "no review window"; got.Text != want {
		t.Errorf("the collapsed line reads %q, want %q", got.Text, want)
	}
	if got.Title == "" {
		t.Error("the collapsed line has no hover saying why it holds no figures")
	}
}
