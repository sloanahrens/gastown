package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Landings pane names the overseer's landing hold above its rows, and a
// pause alone is reason to show the section (gt-ckc93z).
func TestLandingsPaneNamesALandingPause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		paused     map[string]any
		wantHidden bool
		wantText   string
	}{
		{
			name:     "a pause names its reason and says polecats keep working",
			paused:   map[string]any{"reason": "overseer: drain the queue"},
			wantText: "landings paused: overseer: drain the queue (polecats keep working)",
		},
		{
			name:     "a pause that could not be read says so",
			paused:   map[string]any{"unknown": true},
			wantText: "landings pause unknown: the health report could not be read",
		},
		{
			name:       "no pause and nothing to draw hides the section",
			wantHidden: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trend := map[string]any{"hours": []any{}, "stages": []any{}, "recent": []any{}}
			if tc.paused != nil {
				trend["landings_paused"] = tc.paused
			}
			hidden, text := renderLandingsPane(t, trend)
			if hidden != tc.wantHidden {
				t.Errorf("section hidden = %v, want %v", hidden, tc.wantHidden)
			}
			// The body also holds the Polecats section's header, so the note is
			// found inside the text rather than matched whole.
			if tc.wantText != "" && !strings.Contains(text, tc.wantText) {
				t.Errorf("pane text = %q, want it to show %q", text, tc.wantText)
			}
		})
	}
}

// renderLandingsPane draws the Landings body for a trend in the page harness and
// reads back whether its section is hidden and the body's text.
func renderLandingsPane(t *testing.T, trend map[string]any) (hidden bool, text string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"trend": trend})
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	p := newPageDOM(t)
	p.call(t, "renderTrend", st)
	v, err := p.vm.RunString(`JSON.stringify({hidden: document.getElementById("trendsec").hidden, text: document.getElementById("trend").textContent})`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Hidden bool   `json:"hidden"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatal(err)
	}
	return out.Hidden, out.Text
}
