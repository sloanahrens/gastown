package dashboard

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var updateVelocityPaneGolden = flag.Bool("update-velocity-pane-golden", false, "rewrite testdata/landings_pane_without_velocity.golden")

// velocityPaneHour is the start of the i-th hour the velocity pane tests draw;
// the trend's three hours start at 14:00 local, and the feed also carries 13:00
// so a page that matched the two series by index would fail.
func velocityPaneHour(i int) time.Time {
	return time.Date(2026, 10, 31, 14+i, 0, 0, 0, time.Local)
}

// velocityPaneWithout is the Landings reading with no velocity feed: the trend's
// three hours, which is the reading the pane drew before the feed existed.
func velocityPaneWithout() map[string]any {
	hour := func(i int) string { return velocityPaneHour(i).Format(time.RFC3339) }
	return map[string]any{"trend": map[string]any{
		"hours": []map[string]any{
			{"hour": hour(0), "landed": 3, "rejected": 0},
			{"hour": hour(1), "landed": 2, "rejected": 1},
			{"hour": hour(2), "landed": 1, "rejected": 0},
		},
	}}
}

// velocityPaneReading is velocityPaneWithout plus a velocity feed: 13:00 is
// outside the trend, 14:00 holds 4 points, 15:00 holds 8 points and an
// unweighted landing, and 16:00 holds only an unweighted landing.
func velocityPaneReading() map[string]any {
	st := velocityPaneWithout()
	hour := func(i int) string { return velocityPaneHour(i).Format(time.RFC3339) }
	st["trend"].(map[string]any)["velocity"] = map[string]any{
		"hours": []map[string]any{
			{"hour": hour(-1), "points": 10, "weighted": 2, "unweighted": 0},
			{"hour": hour(0), "points": 4, "weighted": 2, "unweighted": 0},
			{"hour": hour(1), "points": 8, "weighted": 3, "unweighted": 1},
			{"hour": hour(2), "points": 0, "weighted": 0, "unweighted": 2},
		},
		"figures": map[string]any{
			"points_24h": 22, "unweighted_24h": 3,
			"now_per_hour": 4.0, "avg_per_hour": 2.5, "median_per_landing": 4,
		},
	}
	return st
}

// paneTreeJS prints the trend element's drawn tree, one element or text per
// line, indented by depth: the tag, class, title, hidden flag and inline height
// of each element, so a test compares what the page drew.
const paneTreeJS = `(function () {
  var lines = [];
  (function dump(e, depth) {
    var pad = new Array(depth + 1).join("  ");
    if (e.nodeType === 3) { lines.push(pad + JSON.stringify(e.textContent)); return; }
    var attrs = [e.tagName.toLowerCase()];
    if (e.className) attrs.push('class="' + e.className + '"');
    if (e.title) attrs.push('title="' + e.title + '"');
    if (e.hidden) attrs.push("hidden");
    if (e.style && e.style.height) attrs.push('height="' + e.style.height + '"');
    lines.push(pad + attrs.join(" "));
    if (e._kids.length) e._kids.forEach(function (k) { dump(k, depth + 1); });
    else if (e._text) lines.push(pad + "  " + JSON.stringify(e._text));
  })(document.getElementById("trend"), 0);
  return lines.join("\n");
})()`

// clockText is a local clock reading, which depends on the host's zone; the
// golden keeps the shape of each label and not its digits.
var clockText = regexp.MustCompile(`\d{2}:\d{2}`)

// TestVelocityAbsentPaneDrawsWhatItDrew: with no velocity in the reading the
// pane draws the same tree it drew before the velocity figures and points row
// existed. The golden was taken from the page before the feed was drawn.
func TestVelocityAbsentPaneDrawsWhatItDrew(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityPaneWithout())
	v, err := p.vm.RunString(paneTreeJS)
	if err != nil {
		t.Fatalf("reading the drawn pane: %v", err)
	}
	got := clockText.ReplaceAllString(v.String(), "HH:MM") + "\n"

	golden := filepath.Join("testdata", "landings_pane_without_velocity.golden")
	if *updateVelocityPaneGolden {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the pane without a velocity differs from %s (rewrite with -update-velocity-pane-golden):\n%s\nwant:\n%s", golden, got, want)
	}
}

// velocityFigureView is one figure in the figures row as the page drew it.
type velocityFigureView struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Title string `json:"title"`
}

// readFigureRow reads the figures row, the stats the landings head's kv holds.
func readFigureRow(t *testing.T, p *pageDOM) []velocityFigureView {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var body = __find(document.getElementById("trend"), ".lheadbody");
  var kv = body && __find(body, ".kv");
  if (!kv) return null;
  return kv.children.__arr.map(function (d) {
    return {value: d.children.__arr[0].textContent, label: d.children.__arr[1].textContent, title: d.title};
  });
})())`)
	if err != nil {
		t.Fatalf("reading the figures row: %v", err)
	}
	if v.String() == "null" {
		t.Fatal("the pane draws no figures row")
	}
	var got []velocityFigureView
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decoding the figures row: %v", err)
	}
	return got
}

// figureNamed returns the figure with the given label, failing when the row
// does not carry it.
func figureNamed(t *testing.T, row []velocityFigureView, label string) velocityFigureView {
	t.Helper()

	for _, f := range row {
		if f.Label == label {
			return f
		}
	}
	t.Fatalf("the figures row has no %q figure: %+v", label, row)
	return velocityFigureView{}
}

// With a velocity reading the figures row gains points 24h, points/h now and
// median points, each with the text the feed carries: the rate reads to one
// decimal, the 24-hour average and the unweighted count sit in the tooltips.
func TestVelocityFiguresRowShowsTheThreeFigures(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityPaneReading())
	row := readFigureRow(t, p)

	if got := figureNamed(t, row, "points 24h"); got.Value != "22" {
		t.Errorf("points 24h reads %q, want 22", got.Value)
	} else if !strings.Contains(got.Title, "3 landings") {
		t.Errorf("points 24h tooltip %q does not name the 3 unweighted landings", got.Title)
	}
	if got := figureNamed(t, row, "points/h now"); got.Value != "4.0" {
		t.Errorf("points/h now reads %q, want 4.0", got.Value)
	} else if !strings.Contains(got.Title, "2.5") {
		t.Errorf("points/h now tooltip %q does not name the 24-hour average 2.5", got.Title)
	}
	if got := figureNamed(t, row, "median points"); got.Value != "4" {
		t.Errorf("median points reads %q, want 4", got.Value)
	}
}

// A figure the feed leaves undefined reads as a dash, never as zero: a rate with
// too little data to compute is not a rate of 0 points an hour.
func TestVelocityUndefinedFiguresDrawADashNotZero(t *testing.T) {
	t.Parallel()

	st := velocityPaneReading()
	st["trend"].(map[string]any)["velocity"].(map[string]any)["figures"] = map[string]any{
		"points_24h": 0, "unweighted_24h": 0,
	}
	p := newPageDOM(t)
	p.call(t, "renderTrend", st)
	row := readFigureRow(t, p)

	for _, label := range []string{"points/h now", "median points"} {
		if got := figureNamed(t, row, label); got.Value != "–" {
			t.Errorf("%s reads %q with no value, want a dash", label, got.Value)
		}
	}
}

// velocityColumn is one hour's column in the points row: its parts from the top
// down, each by class and inline height, and the tooltip the column carries.
type velocityColumn struct {
	Title string `json:"title"`
	Parts []struct {
		Class  string `json:"cls"`
		Height string `json:"height"`
	} `json:"parts"`
}

// readPointsRow reads the points row's columns, one per trend hour, or nil when
// the pane draws no points row.
func readPointsRow(t *testing.T, p *pageDOM) []velocityColumn {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var row = __find(document.getElementById("trend"), ".vbars");
  if (!row) return null;
  return row.children.__arr.map(function (col) {
    return {title: col.title, parts: col._kids.map(function (k) {
      return {cls: k.className, height: k.style ? k.style.height : ""};
    })};
  });
})())`)
	if err != nil {
		t.Fatalf("reading the points row: %v", err)
	}
	if v.String() == "null" {
		return nil
	}
	var got []velocityColumn
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decoding the points row: %v", err)
	}
	return got
}

// The points row draws one column per trend hour, matched by the hour's own
// time: the 13:00 velocity hour outside the trend is not drawn, and the bars
// scale to the largest hour's points, 8 here.
func TestVelocityPointsRowDrawsOneBarPerHourAtItsHeight(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityPaneReading())
	cols := readPointsRow(t, p)

	if len(cols) != 3 {
		t.Fatalf("the points row has %d columns, want one per trend hour (3)", len(cols))
	}
	if h := cols[0].Parts; len(h) != 1 || h[0].Class != "hpts" || h[0].Height != "50%" {
		t.Errorf("14:00 draws %+v, want one points bar at 50%% (4 of 8)", h)
	}
	if h := cols[1].Parts; len(h) != 2 || h[1].Class != "hpts" || h[1].Height != "100%" {
		t.Errorf("15:00 draws %+v, want a cap and a points bar at 100%%", h)
	}
}

// An hour with unweighted landings gets a hatched cap on its column, and its
// tooltip names the count. An hour with only unweighted landings draws no points
// bar at all, so it does not read as zero points; its tooltip says no points
// were measured.
func TestVelocityUnweightedLandingsDrawACapNotAZeroBar(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityPaneReading())
	cols := readPointsRow(t, p)
	if len(cols) != 3 {
		t.Fatalf("the points row has %d columns, want 3", len(cols))
	}

	mixed := cols[1]
	if len(mixed.Parts) == 0 || mixed.Parts[0].Class != "hcap" {
		t.Errorf("the 15:00 column has no hatched cap on top: %+v", mixed.Parts)
	}
	wantMixed := fmt.Sprintf("%s · 8 points · 1 landing without time", velocityPaneHour(1).Format("15:04"))
	if mixed.Title != wantMixed {
		t.Errorf("the 15:00 tooltip is %q, want %q", mixed.Title, wantMixed)
	}

	only := cols[2]
	for _, part := range only.Parts {
		if part.Class == "hpts" {
			t.Errorf("the 16:00 hour has only unweighted landings but draws a points bar of height %s", part.Height)
		}
	}
	if len(only.Parts) != 1 || only.Parts[0].Class != "hcap" {
		t.Errorf("the 16:00 column draws %+v, want only the hatched cap", only.Parts)
	}
	wantOnly := fmt.Sprintf("%s · no points measured · 2 landings without time", velocityPaneHour(2).Format("15:04"))
	if only.Title != wantOnly {
		t.Errorf("the 16:00 tooltip is %q, want %q", only.Title, wantOnly)
	}
}

// A trend hour the velocity feed holds no reading for is an empty column that
// says so, not a bar: the feed did not measure that hour, so it is not zero
// points. The bars still scale to the hours that have a reading.
func TestVelocityHourWithNoReadingDrawsAnEmptyColumn(t *testing.T) {
	t.Parallel()

	st := velocityPaneReading()
	st["trend"].(map[string]any)["velocity"].(map[string]any)["hours"] = []map[string]any{
		{"hour": velocityPaneHour(0).Format(time.RFC3339), "points": 4, "weighted": 2, "unweighted": 0},
		{"hour": velocityPaneHour(2).Format(time.RFC3339), "points": 0, "weighted": 0, "unweighted": 2},
	}
	p := newPageDOM(t)
	p.call(t, "renderTrend", st)
	cols := readPointsRow(t, p)
	if len(cols) != 3 {
		t.Fatalf("the points row has %d columns, want one per trend hour (3)", len(cols))
	}

	gap := cols[1]
	if len(gap.Parts) != 0 {
		t.Errorf("the 15:00 hour has no reading but draws %+v, want an empty column", gap.Parts)
	}
	if want := fmt.Sprintf("%s · no points reading", velocityPaneHour(1).Format("15:04")); gap.Title != want {
		t.Errorf("the 15:00 tooltip is %q, want %q", gap.Title, want)
	}
	if h := cols[0].Parts; len(h) != 1 || h[0].Height != "100%" {
		t.Errorf("14:00 draws %+v, want its points bar at 100%% (the largest reading)", h)
	}
}
