package storypoints

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/beads"
)

const shapeFixturesPath = "../../docs/plans/2026-11-11-story-points-shape-fixtures.json"

func TestEstimateTextLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		labels []string
		head   string
	}{
		{"with labels", []string{"sp", "rig:gastown"}, "# Add shape [open task P2] labels: sp, rig:gastown\n\n"},
		{"without labels", nil, "# Add shape [open task P2]\n\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := &beads.Issue{
				Title:              "Add shape",
				Status:             "open",
				Type:               "task",
				Priority:           2,
				Labels:             tc.labels,
				Description:        "## Goal\nDo it.\n",
				AcceptanceCriteria: "- [ ] one\n- [ ] two\n",
			}
			want := tc.head +
				"## Description\n\n## Goal\nDo it.\n\n" +
				"## Acceptance\n\n- [ ] one\n- [ ] two\n"
			if got := EstimateText(issue); got != want {
				t.Errorf("EstimateText() =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

func TestEstimateTextEmptyDescription(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		Title:    "T",
		Status:   "open",
		Type:     "task",
		Priority: 2,
	}
	text := EstimateText(issue)
	want := "# T [open task P2]\n\n## Description\n\n\n"
	if text != want {
		t.Fatalf("EstimateText() = %q, want %q", text, want)
	}
	f := Features(text, "gastown", SeatHaiku)
	if f.Boxes != 0 || f.Paths != 0 || f.Pkgs != 0 || f.NCons != 0 || f.Tests != 0 || f.NBt != 0 {
		t.Errorf("Features(empty description) = %+v, want zero counts", f)
	}
	if wantDLen := math.Log(1 + float64(utf8.RuneCountInString(text))); math.Abs(f.DLen-wantDLen) > 1e-9 {
		t.Errorf("DLen = %v, want %v", f.DLen, wantDLen)
	}
}

func TestEstimateTextWhitespaceAcceptance(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{
		Title:              "T",
		Status:             "open",
		Type:               "task",
		Priority:           2,
		Description:        "D",
		AcceptanceCriteria: "  \n\t\n",
	}
	want := "# T [open task P2]\n\n## Description\n\nD\n"
	if got := EstimateText(issue); got != want {
		t.Errorf("EstimateText() = %q, want %q: a whitespace-only acceptance field adds nothing", got, want)
	}
}

func TestFeaturesMatchFixtures(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(shapeFixturesPath)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixtures []struct {
		Bead      string             `json:"bead"`
		Rig       string             `json:"rig"`
		SeatClass string             `json:"seat_class"`
		Text      string             `json:"text"`
		Features  map[string]float64 `json:"features"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fixtures) != 6 {
		t.Fatalf("fixtures = %d beads, want 6", len(fixtures))
	}
	for _, fx := range fixtures {
		t.Run(fx.Bead, func(t *testing.T) {
			seat := SeatFlash
			if fx.SeatClass == "haiku" {
				seat = SeatHaiku
			}
			got := namedFeatures(Features(fx.Text, fx.Rig, seat))
			if len(got) != len(fx.Features) {
				t.Fatalf("fixture has %d features, Shape has %d", len(fx.Features), len(got))
			}
			for name, want := range fx.Features {
				if math.Abs(got[name]-want) > 1e-6 {
					t.Errorf("%s = %v, want %v", name, got[name], want)
				}
			}
		})
	}
}

func TestFeaturesEmptyText(t *testing.T) {
	t.Parallel()
	got := Features("", "devops", SeatFlash)
	want := Shape{RigDV: 1}
	if got != want {
		t.Errorf("Features(empty) = %+v, want %+v", got, want)
	}
}

func TestFeaturesNoAcceptanceHeading(t *testing.T) {
	t.Parallel()
	text := "# T [open task P2]\n\n## Description\n\n- [ ] not counted\n- [x] not counted either\n"
	if got := Features(text, "gastown", SeatHaiku).Boxes; got != 0 {
		t.Errorf("Boxes = %v, want 0 when there is no Acceptance heading", got)
	}
}

func TestFeaturesCountsBoxesAfterAcceptanceHeading(t *testing.T) {
	t.Parallel()
	text := "## Description\n\n- [x] before the heading\n\n## Acceptance\n\n- [x] one\n- [ ] two\n- [X] three\n"
	if got := Features(text, "gastown", SeatHaiku).Boxes; got != 3 {
		t.Errorf("Boxes = %v, want 3", got)
	}
}

func TestFeaturesLabels(t *testing.T) {
	t.Parallel()
	withLabels := "# T [open task P2] labels: a, b, c\n\n## Description\n\nD\n"
	withoutLabels := "# T [open task P2]\n\n## Description\n\nD\n"
	if got := Features(withLabels, "gastown", SeatHaiku).NLab; got != 3 {
		t.Errorf("NLab with labels = %v, want 3", got)
	}
	if got := Features(withoutLabels, "gastown", SeatHaiku).NLab; got != 0 {
		t.Errorf("NLab without labels = %v, want 0", got)
	}
}

func TestFeaturesBacktickPath(t *testing.T) {
	t.Parallel()
	text := "Edit `internal/storypoints/shape.go` and nothing else.\n"
	got := Features(text, "gastown", SeatHaiku)
	if got.Paths != 1 {
		t.Errorf("Paths = %v, want 1 for a path inside backticks", got.Paths)
	}
	if got.NBt != 2 {
		t.Errorf("NBt = %v, want 2", got.NBt)
	}
}

func TestFeaturesUnknownSeatUsesMean(t *testing.T) {
	t.Parallel()
	if got := Features("", "gastown", SeatUnknown).IsHaiku; got != unknownSeatHaiku {
		t.Errorf("IsHaiku for an unknown seat = %v, want the training mean %v", got, unknownSeatHaiku)
	}
	if got := Features("", "gastown", SeatHaiku).IsHaiku; got != 1 {
		t.Errorf("IsHaiku for the Haiku seat = %v, want 1", got)
	}
}

// namedFeatures keys each Shape field by its weights-file name.
func namedFeatures(s Shape) map[string]float64 {
	return map[string]float64{
		"boxes":    s.Boxes,
		"paths":    s.Paths,
		"pkgs":     s.Pkgs,
		"dlen":     s.DLen,
		"ncons":    s.NCons,
		"nlab":     s.NLab,
		"podeng":   s.PodEng,
		"tests":    s.Tests,
		"nbt":      s.NBt,
		"rig_gt":   s.RigGT,
		"rig_dv":   s.RigDV,
		"is_haiku": s.IsHaiku,
	}
}
