package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The wall trend reaches the page as part of the tiersweep snapshot: one entry
// per rig, oldest sweep first, each point naming the sha, the tier walls and
// the cycle's total under the field names the pane's script reads. This is the
// JSON contract between the reader and the page, so it is checked where the
// page would receive it (hub.go pollTierSweep).
func TestTierSweepTrendReachesThePageJSON(t *testing.T) {
	t.Parallel()

	total := 576.0
	ts := &TierSweep{Trends: []TierSweepTrendRig{{
		Rig: "gastown",
		Sweeps: []TierSweepTrendPoint{
			{At: time.Date(2026, 10, 3, 21, 9, 36, 0, time.UTC), SHA: "b7c1d2e3", Walls: map[string]float64{"shell": 70}, TotalSecs: &total},
			{At: time.Date(2026, 10, 3, 22, 9, 36, 0, time.UTC), SHA: "a9be03e4", Walls: map[string]float64{"shell": 65, "integration": 180}, TotalSecs: &total},
		},
	}}}
	h := NewHub(Config{TierSweep: func() *TierSweep { return ts }})
	h.pollTierSweep()

	b, err := json.Marshal(h.State())
	if err != nil {
		t.Fatalf("marshaling the state: %v", err)
	}
	var got struct {
		TierSweep struct {
			Trends []struct {
				Rig    string `json:"rig"`
				Sweeps []struct {
					At        time.Time          `json:"at"`
					SHA       string             `json:"sha"`
					Walls     map[string]float64 `json:"walls"`
					TotalSecs *float64           `json:"total_secs"`
				} `json:"sweeps"`
			} `json:"trends"`
		} `json:"tiersweep"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("reading the state JSON: %v", err)
	}
	trends := got.TierSweep.Trends
	if len(trends) != 1 || trends[0].Rig != "gastown" || len(trends[0].Sweeps) != 2 {
		t.Fatalf("trends = %+v, want gastown's two sweeps", trends)
	}
	p := trends[0].Sweeps[1]
	if p.SHA != "a9be03e4" || p.Walls["integration"] != 180 || p.TotalSecs == nil || *p.TotalSecs != 576 {
		t.Errorf("point = %+v, want the sha, tier walls and total", p)
	}
	if !p.At.After(trends[0].Sweeps[0].At) {
		t.Error("the page reads the points oldest first")
	}
}

// The Tier sweeps pane draws one wall trend per rig, using the Landings trend's
// chart: a rig whose sweeps hold walls gets a chart, and a rig that has not
// swept yet reads "no sweeps yet" rather than an error or an empty chart. The
// empty rig must not stop the series beside it from drawing.
func TestTierSweepPaneDrawsATrendPerRig(t *testing.T) {
	t.Parallel()

	total := 600.0
	at := "2026-10-03T22:09:36Z"
	dom := newPageDOM(t)
	dom.call(t, "renderTierSweep", map[string]any{
		"tiersweep": map[string]any{
			"sweeps": []any{map[string]any{
				"at": at, "rig": "gastown", "sha": "a9be03e4",
				"stages": []any{map[string]any{"tier": "shell", "verdict": "GREEN"}},
			}},
			"trends": []any{
				map[string]any{"rig": "gastown", "sweeps": []any{
					map[string]any{"at": at, "sha": "b7c1d2e3", "walls": map[string]any{"shell": 70.0, "integration": 240.0}, "total_secs": total},
					map[string]any{"at": at, "sha": "a9be03e4", "walls": map[string]any{"shell": 65.0, "integration": 180.0}, "total_secs": 570.0},
				}},
				map[string]any{"rig": "quiet", "sweeps": []any{}},
			},
		},
	})

	box := dom.element(t, "tiersweep")
	for _, want := range []string{"gastown", "quiet", "no sweeps yet", "2 sweeps"} {
		if !strings.Contains(box.Text, want) {
			t.Errorf("the pane does not read %q: %s", want, box.Text)
		}
	}
	if strings.Contains(box.Text, "unavailable") {
		t.Error("a rig with no sweeps must not read as an unavailable log")
	}
	// The chart is the one element with children and no text of its own: the
	// svg chartSVG builds, between the caption and the legend.
	var charts int
	for i := 0; i < box.Kids; i++ {
		if d := dom.descend(t, "tiersweep", i); d.Text == "" && d.Kids > 0 {
			charts++
		}
	}
	if charts != 1 {
		t.Errorf("the pane drew %d charts, want one for gastown and none for quiet", charts)
	}
}
