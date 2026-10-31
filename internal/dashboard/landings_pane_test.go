package dashboard

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// The Landings pane draws every finished landing it is given, inside a scroll
// container whose header row stays fixed (gt-1kttu). These render the pane in the
// page harness and count what it drew.
func TestLandingsPaneDrawsEveryLandingInAScrollBox(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 25, 16, 0, 0, 0, time.Local)
	var recent []map[string]any
	for i := 0; i < 30; i++ {
		recent = append(recent, map[string]any{
			"at":      now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339),
			"bead":    fmt.Sprintf("gt-%d", i),
			"rig":     "gastown",
			"outcome": "landed",
		})
	}
	state := map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": now.Truncate(time.Hour).Format(time.RFC3339), "landed": 30, "rejected": 0}},
		"stages": []any{},
		"recent": recent,
	}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}

	p := newPageDOM(t)
	p.call(t, "renderTrend", st)
	counted, err := p.vm.RunString(`(function () {
  var out = {rows: 0, headers: 0, scroll: 0};
  function walk(n, inScroll) {
    if (!n || !n.children) return;
    for (var i = 0; i < n.children.length; i++) {
      var c = n.children[i];
      var cls = c.attr_class || c.className || "";
      var here = inScroll || cls === "lrows";
      if (cls === "lrows") out.scroll++;
      if (here && c.tagName === "TR" && c.children && c.children.length && c.children[0].tagName === "TH") out.headers++;
      if (here && c.tagName === "TR" && c.children && c.children.length && c.children[0].tagName !== "TH") out.rows++;
      walk(c, here);
    }
  }
  walk(document.getElementById("trend"), false);
  return JSON.stringify(out);
})()`)
	if err != nil {
		t.Fatalf("reading the drawn landings pane: %v", err)
	}
	var got struct {
		Rows    int `json:"rows"`
		Headers int `json:"headers"`
		Scroll  int `json:"scroll"`
	}
	if err := json.Unmarshal([]byte(counted.String()), &got); err != nil {
		t.Fatalf("decoding the count: %v", err)
	}
	if got.Scroll != 1 {
		t.Errorf("scroll containers = %d, want the one the landings table sits in", got.Scroll)
	}
	if got.Rows != len(recent) {
		t.Errorf("landing rows drawn = %d, want all %d", got.Rows, len(recent))
	}
	if got.Headers != 1 {
		t.Errorf("header rows drawn in the scroll box = %d, want 1", got.Headers)
	}
}

// Each poll rebuilds the landings rows in a scroll box that opens at the top; the
// redraw must keep the reader's place (gt-tjlu6).
func TestLandingsPaneKeepsItsScrollAcrossARedraw(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 25, 16, 0, 0, 0, time.Local)
	var recent []map[string]any
	for i := 0; i < 30; i++ {
		recent = append(recent, map[string]any{
			"at":      now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339),
			"bead":    fmt.Sprintf("gt-%d", i),
			"rig":     "gastown",
			"outcome": "landed",
		})
	}
	state := map[string]any{"trend": map[string]any{
		"hours":  []map[string]any{{"hour": now.Truncate(time.Hour).Format(time.RFC3339), "landed": 30, "rejected": 0}},
		"stages": []any{},
		"recent": recent,
	}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}

	p := newPageDOM(t)
	p.call(t, "renderTrend", st)
	if _, err := p.vm.RunString(`document.getElementById("trend").querySelector(".lrows").scrollTop = 240`); err != nil {
		t.Fatalf("scrolling the landings rows down: %v", err)
	}
	p.call(t, "renderTrend", st)

	v, err := p.vm.RunString(`document.getElementById("trend").querySelector(".lrows").scrollTop`)
	if err != nil {
		t.Fatalf("reading the landings rows' scroll position: %v", err)
	}
	if got := v.ToInteger(); got != 240 {
		t.Errorf("landings scroll position after a redraw = %d, want 240: the poll put the rows back at the top", got)
	}
}
