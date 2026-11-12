package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests drive renderTrend over the page's own fake DOM with a velocity
// feed that carries the first and second differences, D1 and D2, and read back
// the rows it drew (gt-vd91ny.7). The hours carry only what the row reads, so
// an hour with no difference is an hour with no keys at all.

// velocityDifferencePane is the Landings reading with the given velocity hours
// and nothing else in the feed: the trend's own three hours stay, so the pane
// draws its bars and axis around the two difference rows.
func velocityDifferencePane(hours ...map[string]any) map[string]any {
	st := velocityPaneWithout()
	st["trend"].(map[string]any)["velocity"] = map[string]any{"hours": hours}
	return st
}

// velocityDifferenceHour is one feed hour: d1 and d2 are the differences (nil
// for an hour the feed holds no value for) and the states say what each one
// is. An empty state is left out, as a feed that never judged the hour does.
func velocityDifferenceHour(i int, d1 any, d1State string, d2 any, d2State string) map[string]any {
	h := map[string]any{"hour": velocityPaneHour(i).Format(time.RFC3339), "points": 0, "weighted": 0}
	if d1 != nil {
		h["d1"] = d1
	}
	if d1State != "" {
		h["d1_state"] = d1State
	}
	if d2 != nil {
		h["d2"] = d2
	}
	if d2State != "" {
		h["d2_state"] = d2State
	}
	return h
}

// velocityDifferenceBar is one bar a difference column drew.
type velocityDifferenceBar struct {
	Class  string `json:"cls"`
	Height string `json:"height"`
}

// velocityDifferenceColumn is one hour's column in a difference row: the
// tooltip it carries and the bars drawn in it, top down.
type velocityDifferenceColumn struct {
	Title string                  `json:"title"`
	Bars  []velocityDifferenceBar `json:"bars"`
}

// velocityDifferenceRow is one D1 or D2 row as the page drew it.
type velocityDifferenceRow struct {
	Label string                     `json:"label"`
	Title string                     `json:"title"`
	Cols  []velocityDifferenceColumn `json:"cols"`
}

// readDifferenceRows reads the D1 and D2 rows from the Landings body, in the
// order the page drew them.
func readDifferenceRows(t *testing.T, p *pageDOM) []velocityDifferenceRow {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var rows = [];
  __walk(document.getElementById("trend"), function (e) {
    if (e.className && (" " + e.className + " ").indexOf(" dbars ") >= 0) rows.push(e);
  });
  return rows.map(function (row) {
    var label = "", body = null;
    row._kids.forEach(function (k) {
      if (k.className === "dlabel") label = k.textContent;
      if (k.className === "dbody") body = k;
    });
    return {label: label, title: row.title, cols: body ? body._kids.map(function (c) {
      return {title: c.title, bars: (c._kids || []).map(function (b) {
        return {cls: b.className, height: b.style ? (b.style.height || "") : ""};
      })};
    }) : []};
  });
})())`)
	if err != nil {
		t.Fatalf("reading the difference rows: %v", err)
	}
	var got []velocityDifferenceRow
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decoding the difference rows: %v", err)
	}
	return got
}

// differenceRowNamed returns the row with the given label, failing when the
// pane does not draw it.
func differenceRowNamed(t *testing.T, rows []velocityDifferenceRow, label string) velocityDifferenceRow {
	t.Helper()

	for _, r := range rows {
		if r.Label == label {
			return r
		}
	}
	t.Fatalf("the pane draws no %s row: %+v", label, rows)
	return velocityDifferenceRow{}
}

// With a velocity feed the pane draws both difference rows, D1 before D2, each
// labelled and titled with what it shows.
func TestVelocityDifferenceRowsAreD1AndD2(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 2.0, "signal", 0.5, "signal"),
	))
	rows := readDifferenceRows(t, p)

	if len(rows) != 2 {
		t.Fatalf("the pane draws %d difference rows, want 2 (D1 and D2): %+v", len(rows), rows)
	}
	if rows[0].Label != "D1" || rows[1].Label != "D2" {
		t.Fatalf("the rows are labelled %q and %q, want D1 and D2", rows[0].Label, rows[1].Label)
	}
	if !strings.Contains(rows[0].Title, "D1") || !strings.Contains(rows[1].Title, "D2") {
		t.Errorf("the row titles are %q and %q, want each to name its row", rows[0].Title, rows[1].Title)
	}
}

// A positive difference draws an up bar and a negative one a down bar, each
// scaled to the row's largest signal, and each column's tooltip names the hour,
// the signed value and the state.
func TestVelocityDifferenceBarsAreSignedAboutTheZeroLine(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 2.0, "signal", 0.5, "signal"),
		velocityDifferenceHour(1, -1.0, "signal", -0.25, "signal"),
	))
	d1 := differenceRowNamed(t, readDifferenceRows(t, p), "D1")
	if len(d1.Cols) != 2 {
		t.Fatalf("the D1 row has %d columns, want one per feed hour (2)", len(d1.Cols))
	}

	up := d1.Cols[0]
	if len(up.Bars) != 1 || up.Bars[0].Class != "dbar dup" || up.Bars[0].Height != "50%" {
		t.Errorf("the 13:00 positive difference draws %+v, want one up bar at 50%% (the largest signal)", up.Bars)
	}
	if want := fmt.Sprintf("%s · D1 +2.00 points/h · signal", velocityPaneHour(0).Format("15:04")); up.Title != want {
		t.Errorf("the 13:00 tooltip is %q, want %q", up.Title, want)
	}

	down := d1.Cols[1]
	if len(down.Bars) != 1 || down.Bars[0].Class != "dbar ddn" || down.Bars[0].Height != "25%" {
		t.Errorf("the 14:00 negative difference draws %+v, want one down bar at 25%%", down.Bars)
	}
	if want := fmt.Sprintf("%s · D1 -1.00 points/h · signal", velocityPaneHour(1).Format("15:04")); down.Title != want {
		t.Errorf("the 14:00 tooltip is %q, want %q", down.Title, want)
	}
}

// A difference within noise is drawn muted, so a reader can see it is not a
// movement of the series.
func TestVelocityNoiseDifferenceIsMuted(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 2.0, "signal", nil, ""),
		velocityDifferenceHour(1, 1.0, "noise", nil, ""),
	))
	d1 := differenceRowNamed(t, readDifferenceRows(t, p), "D1")
	if len(d1.Cols) != 2 {
		t.Fatalf("the D1 row has %d columns, want 2", len(d1.Cols))
	}

	noise := d1.Cols[1].Bars
	if len(noise) != 1 || !strings.Contains(noise[0].Class, "dmute") {
		t.Errorf("the 14:00 noise difference draws %+v, want one muted bar", noise)
	}
	if !strings.HasSuffix(d1.Cols[1].Title, "noise") {
		t.Errorf("the 14:00 noise tooltip is %q, want it to end with noise", d1.Cols[1].Title)
	}
}

// A signal difference is drawn in full, never muted: the muting is the noise
// state's, not the bar's.
func TestVelocitySignalDifferenceIsNotMuted(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 2.0, "signal", nil, ""),
		velocityDifferenceHour(1, 1.0, "noise", nil, ""),
	))
	d1 := differenceRowNamed(t, readDifferenceRows(t, p), "D1")
	if len(d1.Cols) != 2 {
		t.Fatalf("the D1 row has %d columns, want 2", len(d1.Cols))
	}

	signal := d1.Cols[0].Bars
	if len(signal) != 1 || strings.Contains(signal[0].Class, "dmute") {
		t.Errorf("the 13:00 signal difference draws %+v, want one bar that is not muted", signal)
	}
}

// An insufficient difference draws no bar at all — an insufficient hour is not
// a quiet one — and its tooltip says there is not enough data. That holds even
// when the feed carries a value the state says is insufficient, and an hour
// whose state the feed does not carry reads the same way, never as a value.
func TestVelocityInsufficientDifferenceDrawsNothingAndSaysSo(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 0.7, "insufficient", nil, ""),
		velocityDifferenceHour(1, nil, "", nil, ""),
	))
	d1 := differenceRowNamed(t, readDifferenceRows(t, p), "D1")
	if len(d1.Cols) != 2 {
		t.Fatalf("the D1 row has %d columns, want 2", len(d1.Cols))
	}

	for i, want := range []string{"insufficient", "no state"} {
		if bars := d1.Cols[i].Bars; len(bars) != 0 {
			t.Errorf("the %s hour draws %+v, want no bar", want, bars)
		}
		if wantTip := fmt.Sprintf("%s · D1 not enough data", velocityPaneHour(i).Format("15:04")); d1.Cols[i].Title != wantTip {
			t.Errorf("the %s hour tooltip is %q, want %q", want, d1.Cols[i].Title, wantTip)
		}
	}
}

// A row with no signal value draws every defined bar at one small fixed
// height: with nothing to scale to, a noise bar must not stretch to fill the
// row and so read as a movement.
func TestVelocityDifferenceRowWithoutASignalUsesTheFixedHeight(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 0.5, "noise", nil, ""),
		velocityDifferenceHour(1, -0.25, "noise", nil, ""),
	))
	d1 := differenceRowNamed(t, readDifferenceRows(t, p), "D1")
	if len(d1.Cols) != 2 {
		t.Fatalf("the D1 row has %d columns, want 2", len(d1.Cols))
	}

	for i, col := range d1.Cols {
		if len(col.Bars) != 1 || col.Bars[0].Height != "10%" {
			t.Errorf("hour %d draws %+v with no signal in the row, want one bar at the fixed 10%%", i, col.Bars)
		}
	}
}

// A row whose signal values are all zero has a signal, so it is not a row with
// no signal: the fixed height does not apply. Any noise above zero is larger
// than every signal in that row and is capped at the row's half, not drawn at
// the fixed height.
func TestVelocityDifferenceRowWhoseSignalsAreZeroCapsItsNoise(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 0.0, "signal", nil, ""),
		velocityDifferenceHour(1, 0.5, "noise", nil, ""),
	))
	d1 := differenceRowNamed(t, readDifferenceRows(t, p), "D1")
	if len(d1.Cols) != 2 {
		t.Fatalf("the D1 row has %d columns, want 2", len(d1.Cols))
	}

	if bars := d1.Cols[0].Bars; len(bars) != 0 {
		t.Errorf("the zero signal hour draws %+v, want no bar", bars)
	}
	noise := d1.Cols[1].Bars
	if len(noise) != 1 || noise[0].Height != "50%" || !strings.Contains(noise[0].Class, "dmute") {
		t.Errorf("the noise hour draws %+v, want one muted bar capped at 50%%", noise)
	}
}

// A noise difference larger than every signal in its row is capped at the
// row's half, never allowed to stretch the row, and it stays muted.
func TestVelocityNoiseLargerThanEverySignalIsCapped(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 1.0, "signal", nil, ""),
		velocityDifferenceHour(1, 4.0, "noise", nil, ""),
	))
	d1 := differenceRowNamed(t, readDifferenceRows(t, p), "D1")
	if len(d1.Cols) != 2 {
		t.Fatalf("the D1 row has %d columns, want 2", len(d1.Cols))
	}

	noise := d1.Cols[1].Bars
	if len(noise) != 1 || noise[0].Height != "50%" || !strings.Contains(noise[0].Class, "dmute") {
		t.Errorf("the 14:00 noise draws %+v, want one muted bar capped at 50%%", noise)
	}
}

// D2 is drawn by the same rule as D1 and scales to its own signals, not D1's:
// a D2 signal is drawn at its share of the D2 row's largest signal, and a D2
// noise value is muted.
func TestVelocityD2BarsScaleToTheD2Signals(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 4.0, "signal", 0.5, "signal"),
		velocityDifferenceHour(1, -2.0, "signal", -0.25, "signal"),
		velocityDifferenceHour(2, 1.0, "noise", 0.1, "noise"),
	))
	d2 := differenceRowNamed(t, readDifferenceRows(t, p), "D2")
	if len(d2.Cols) != 3 {
		t.Fatalf("the D2 row has %d columns, want one per feed hour (3)", len(d2.Cols))
	}

	up := d2.Cols[0].Bars
	if len(up) != 1 || up[0].Class != "dbar dup" || up[0].Height != "50%" {
		t.Errorf("the 13:00 D2 signal draws %+v, want one up bar at 50%% (the D2 row's largest signal)", up)
	}
	down := d2.Cols[1].Bars
	if len(down) != 1 || down[0].Class != "dbar ddn" || down[0].Height != "25%" {
		t.Errorf("the 14:00 D2 signal draws %+v, want one down bar at 25%%", down)
	}
	noise := d2.Cols[2].Bars
	if len(noise) != 1 || noise[0].Height != "10%" || !strings.Contains(noise[0].Class, "dmute") {
		t.Errorf("the 15:00 D2 noise draws %+v, want one muted bar at 10%%", noise)
	}
	if want := fmt.Sprintf("%s · D2 +0.10 points/h² · noise", velocityPaneHour(2).Format("15:04")); d2.Cols[2].Title != want {
		t.Errorf("the 15:00 D2 tooltip is %q, want %q", d2.Cols[2].Title, want)
	}
}

// The rows carry a one-line legend that says what D1 and D2 are and that a
// muted bar is within noise.
func TestVelocityDifferenceRowsCarryTheLegend(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityDifferencePane(
		velocityDifferenceHour(0, 2.0, "signal", 0.5, "signal"),
	))
	v, err := p.vm.RunString(`(function () {
  var text = "";
  __walk(document.getElementById("trend"), function (e) {
    if (e.className && (" " + e.className + " ").indexOf(" legend ") >= 0) text = e.textContent;
  });
  return text;
})()`)
	if err != nil {
		t.Fatalf("reading the legend: %v", err)
	}
	got := v.String()
	for _, want := range []string{"D1", "D2", "within noise"} {
		if !strings.Contains(got, want) {
			t.Errorf("the legend %q does not name %q", got, want)
		}
	}
}
