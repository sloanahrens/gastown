package dashboard

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"
)

// These tests drive renderTrend over the page's own fake DOM with a velocity
// feed that carries one series per seat class, and press the class buttons the
// way a reader's click does (gt-vd91ny.8). Each class series is the feed's own
// shape: the hours carry the points, the landings with time and the rejected
// points, and the differences are set where a test reads them.

// velocityClassHour is one feed hour of a class series: the points landed, the
// landings that carried a time, and the points rejected after landing.
func velocityClassHour(i, points, weighted, rejectedPoints int) map[string]any {
	return map[string]any{
		"hour":                velocityPaneHour(i).Format(time.RFC3339),
		"points":              points,
		"weighted":            weighted,
		"unweighted":          0,
		"rejected_points":     rejectedPoints,
		"rejected_unweighted": 0,
	}
}

// velocityClassPane is the Landings reading with a velocity feed whose whole
// series is all and whose per-class series are classes. The trend's three hours
// stay, so the pane draws its bars and axis around the velocity rows.
func velocityClassPane(all []map[string]any, classes map[string][]map[string]any) map[string]any {
	st := velocityPaneWithout()
	st["trend"].(map[string]any)["velocity"] = map[string]any{"hours": all, "classes": classes}
	return st
}

// velocityClassReading is the reading most tests share: the whole town lands 4
// points at 14:00 and 8 at 15:00, the haiku class lands 2 and 1, and the flash
// class has no landings at all.
func velocityClassReading() map[string]any {
	all := []map[string]any{velocityClassHour(0, 4, 2, 0), velocityClassHour(1, 8, 3, 0)}
	haiku := []map[string]any{velocityClassHour(0, 2, 1, 0), velocityClassHour(1, 1, 1, 0)}
	flash := []map[string]any{velocityClassHour(0, 0, 0, 0), velocityClassHour(1, 0, 0, 0)}
	return velocityClassPane(all, map[string][]map[string]any{"all": all, "haiku": haiku, "flash": flash})
}

// seatClassButton is one class button the pane drew: its name and whether it
// is the one pressed.
type seatClassButton struct {
	Name    string `json:"name"`
	Pressed bool   `json:"pressed"`
}

// readSeatClassButtons reads the class buttons in the order the pane drew them.
func readSeatClassButtons(t *testing.T, p *pageDOM) []seatClassButton {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var out = [];
  __walk(document.getElementById("trend"), function (e) {
    if (e.className && (" " + e.className + " ").indexOf(" vsel ") >= 0) {
      out.push({name: e.textContent, pressed: (" " + e.className + " ").indexOf(" on ") >= 0});
    }
  });
  return out;
})())`)
	if err != nil {
		t.Fatalf("reading the seat class buttons: %v", err)
	}
	var got []seatClassButton
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decoding the seat class buttons: %v", err)
	}
	return got
}

// pressSeatClass delivers a click to the seat class button with the given name,
// and fails when the pane draws no such button or draws it twice.
func pressSeatClass(t *testing.T, p *pageDOM, name string) {
	t.Helper()

	v, err := p.vm.RunString(`(function (name) {
  var hits = 0;
  __walk(document.getElementById("trend"), function (e) {
    if (e.className && (" " + e.className + " ").indexOf(" vsel ") >= 0 && e.textContent === name) {
      hits++;
      __fireOn(e, "click");
    }
  });
  return hits;
})(` + strconv.Quote(name) + `)`)
	if err != nil {
		t.Fatalf("pressing the %s button: %v", name, err)
	}
	if v.ToInteger() != 1 {
		t.Fatalf("the pane draws %d %s buttons, want 1", v.ToInteger(), name)
	}
}

// readEmptyTexts reads the text of every empty-state line the pane draws.
func readEmptyTexts(t *testing.T, p *pageDOM) []string {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((function () {
  var out = [];
  __walk(document.getElementById("trend"), function (e) {
    if (e.className === "empty") out.push(e.textContent);
  });
  return out;
})())`)
	if err != nil {
		t.Fatalf("reading the empty-state lines: %v", err)
	}
	var got []string
	if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
		t.Fatalf("decoding the empty-state lines: %v", err)
	}
	return got
}

// pointsHeights reads the points row as one part list per column, top down, so
// a test can compare the bars the pane drew with the series it was given.
func pointsHeights(t *testing.T, p *pageDOM) [][]string {
	t.Helper()

	cols := readPointsRow(t, p)
	if cols == nil {
		t.Fatal("the pane draws no points row")
	}
	out := make([][]string, len(cols))
	for i, c := range cols {
		for _, part := range c.Parts {
			out[i] = append(out[i], part.Class+" "+part.Height)
		}
	}
	return out
}

// The pane draws one button per seat class the feed carries, the whole town's
// first and then the classes in name order, with the whole town pressed.
func TestVelocityClassButtonsAreTheClassesThePaneHolds(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassReading())
	got := readSeatClassButtons(t, p)

	want := []seatClassButton{{"all", true}, {"flash", false}, {"haiku", false}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the class buttons are %+v, want %+v", got, want)
	}
}

// Choosing haiku redraws the points row from haiku's series, so its bars scale
// to haiku's own largest hour: 14:00 is the largest and draws at 100%.
func TestVelocityChoosingAClassRedrawsThePointsRowFromThatClass(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassReading())
	pressSeatClass(t, p, "haiku")

	got := pointsHeights(t, p)
	want := [][]string{{"hpts 100%"}, {"hpts 50%"}, nil}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after choosing haiku the points row draws %v, want %v", got, want)
	}
}

// Choosing haiku redraws the two difference rows from haiku's series too: at
// 14:00 the D1 and D2 bars are haiku's own negative differences, not the whole
// town's positive ones.
func TestVelocityChoosingAClassRedrawsTheDifferenceRowsFromThatClass(t *testing.T) {
	t.Parallel()

	all := []map[string]any{velocityClassHour(0, 4, 2, 0), velocityClassHour(1, 8, 3, 0)}
	all[0]["d1"], all[0]["d1_state"] = 2.0, "signal"
	all[0]["d2"], all[0]["d2_state"] = 0.5, "signal"
	haiku := []map[string]any{velocityClassHour(0, 2, 1, 0), velocityClassHour(1, 1, 1, 0)}
	haiku[0]["d1"], haiku[0]["d1_state"] = -1.0, "signal"
	haiku[0]["d2"], haiku[0]["d2_state"] = -0.25, "signal"
	haiku[1]["d1"], haiku[1]["d1_state"] = -0.5, "signal"
	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassPane(all, map[string][]map[string]any{"all": all, "haiku": haiku}))

	rows := readDifferenceRows(t, p)
	if up := differenceRowNamed(t, rows, "D1").Cols[0].Bars; len(up) != 1 || up[0].Class != "dbar dup" {
		t.Fatalf("before the choice the 14:00 D1 draws %+v, want the whole town's up bar", up)
	}
	if up := differenceRowNamed(t, rows, "D2").Cols[0].Bars; len(up) != 1 || up[0].Class != "dbar dup" {
		t.Fatalf("before the choice the 14:00 D2 draws %+v, want the whole town's up bar", up)
	}
	pressSeatClass(t, p, "haiku")

	rows = readDifferenceRows(t, p)
	if down := differenceRowNamed(t, rows, "D1").Cols[0].Bars; len(down) != 1 || down[0].Class != "dbar ddn" {
		t.Errorf("after choosing haiku the 14:00 D1 draws %+v, want haiku's down bar", down)
	}
	if down := differenceRowNamed(t, rows, "D2").Cols[0].Bars; len(down) != 1 || down[0].Class != "dbar ddn" {
		t.Errorf("after choosing haiku the 14:00 D2 draws %+v, want haiku's down bar", down)
	}
}

// Choosing all again restores the whole town's points row, so the choice is not
// a one-way trip.
func TestVelocityChoosingAllRestoresTheWholeTownsPointsRow(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassReading())
	before := pointsHeights(t, p)
	pressSeatClass(t, p, "haiku")
	pressSeatClass(t, p, "all")

	if got := pointsHeights(t, p); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Errorf("after choosing all the points row draws %v, want %v as before the choice", got, before)
	}
	if got := readSeatClassButtons(t, p); !got[0].Pressed || got[0].Name != "all" {
		t.Errorf("after choosing all the buttons are %+v, want all pressed", got)
	}
}

// A choice is kept in localStorage, so a reader who comes back to the page finds
// the class they picked still picked and its rows still drawn.
func TestVelocityClassChoiceIsStoredAndSurvivesAReload(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassReading())
	pressSeatClass(t, p, "haiku")
	if got := p.storage(t)["gt-landings-velocity-class"]; got != "haiku" {
		t.Fatalf("the stored choice is %q, want haiku", got)
	}

	reloaded := newPageDOMStoring(t, p.storage(t))
	reloaded.call(t, "renderTrend", velocityClassReading())
	if got := readSeatClassButtons(t, reloaded); got[2].Name != "haiku" || !got[2].Pressed {
		t.Errorf("after a reload the buttons are %+v, want haiku pressed", got)
	}
	if got := pointsHeights(t, reloaded); fmt.Sprint(got) != fmt.Sprint([][]string{{"hpts 100%"}, {"hpts 50%"}, nil}) {
		t.Errorf("after a reload the points row draws %v, want haiku's series", got)
	}
}

// A stored class the feed does not carry falls back to the whole town, rather
// than drawing no rows or an empty pane.
func TestVelocityStoredClassMissingFromTheFeedFallsBackToAll(t *testing.T) {
	t.Parallel()

	p := newPageDOMStoring(t, map[string]string{"gt-landings-velocity-class": "sonnet"})
	p.call(t, "renderTrend", velocityClassReading())

	if got := readSeatClassButtons(t, p); !got[0].Pressed || got[0].Name != "all" {
		t.Errorf("with sonnet stored and absent from the feed the buttons are %+v, want all pressed", got)
	}
	if got := pointsHeights(t, p); fmt.Sprint(got) != fmt.Sprint([][]string{{"hpts 50%"}, {"hpts 100%"}, nil}) {
		t.Errorf("with sonnet stored and absent from the feed the points row draws %v, want the whole town's", got)
	}
}

// A browser that refuses storage still renders the pane with all selected: the
// page reads its choice with a try and falls back to the whole town.
func TestVelocityPaneRendersWithAllSelectedWhenStorageIsRefused(t *testing.T) {
	t.Parallel()

	p := newPageDOMNoStorage(t)
	p.call(t, "renderTrend", velocityClassReading())

	if got := readSeatClassButtons(t, p); !got[0].Pressed || got[0].Name != "all" {
		t.Errorf("with storage refused the buttons are %+v, want all pressed", got)
	}
	if got := pointsHeights(t, p); fmt.Sprint(got) != fmt.Sprint([][]string{{"hpts 50%"}, {"hpts 100%"}, nil}) {
		t.Errorf("with storage refused the points row draws %v, want the whole town's", got)
	}
}

// With storage refused a choice still applies for the session: the write fails
// quietly and the rows redraw from the class picked.
func TestVelocityChoiceAppliesForTheSessionWhenStorageIsRefused(t *testing.T) {
	t.Parallel()

	p := newPageDOMNoStorage(t)
	p.call(t, "renderTrend", velocityClassReading())
	pressSeatClass(t, p, "haiku")

	if got := pointsHeights(t, p); fmt.Sprint(got) != fmt.Sprint([][]string{{"hpts 100%"}, {"hpts 50%"}, nil}) {
		t.Errorf("with storage refused and haiku chosen the points row draws %v, want haiku's series", got)
	}
}

// A class with no landing that carried a time draws its text in place of the
// rows: no points bars and no difference rows, never empty rows.
func TestVelocityClassWithNoLandingWithTimeShowsTheTextNotEmptyRows(t *testing.T) {
	t.Parallel()

	all := []map[string]any{velocityClassHour(0, 4, 2, 0), velocityClassHour(1, 8, 3, 0)}
	flash := []map[string]any{velocityClassHour(0, 0, 0, 0), velocityClassHour(1, 0, 0, 0)}
	flash[1]["unweighted"] = 2
	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassPane(all, map[string][]map[string]any{"all": all, "flash": flash}))
	pressSeatClass(t, p, "flash")

	if got := readEmptyTexts(t, p); !slices.Contains(got, "no landings with time") {
		t.Errorf("the empty-state lines are %q, want %q", got, "no landings with time")
	}
	if cols := readPointsRow(t, p); cols != nil {
		t.Errorf("a class with no landing with time draws a points row of %d columns, want none", len(cols))
	}
	if rows := readDifferenceRows(t, p); len(rows) != 0 {
		t.Errorf("a class with no landing with time draws %d difference rows, want none", len(rows))
	}
}

// A rejected segment stacks directly above the landed bar, at its own share of
// the same scale. The scale is the largest hour's points plus its rejected
// points: 14:00 holds 4 landed and 6 rejected, a total of 10, so the rejected
// part draws at 60% and the landed bar at 40%, and 15:00 draws at 80%.
func TestVelocityRejectedPointsStackAboveTheLandedBar(t *testing.T) {
	t.Parallel()

	all := []map[string]any{velocityClassHour(0, 4, 2, 6), velocityClassHour(1, 8, 3, 0)}
	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassPane(all, map[string][]map[string]any{"all": all}))
	cols := readPointsRow(t, p)
	if len(cols) != 3 {
		t.Fatalf("the points row has %d columns, want 3", len(cols))
	}

	stack := cols[0].Parts
	if len(stack) != 2 || stack[0].Class != "hrej" || stack[0].Height != "60%" || stack[1].Class != "hpts" || stack[1].Height != "40%" {
		t.Errorf("14:00 draws %+v, want a rejected segment at 60%% above a landed bar at 40%%", stack)
	}
	if got := cols[1].Parts; len(got) != 1 || got[0].Class != "hpts" || got[0].Height != "80%" {
		t.Errorf("15:00 draws %+v, want one points bar at 80%%", got)
	}
}

// The tooltip of a column with rejected points names the rejected points and
// the rejections without time, beside the landed points.
func TestVelocityRejectedTooltipNamesRejectedPointsAndRejections(t *testing.T) {
	t.Parallel()

	first := velocityClassHour(0, 4, 2, 2)
	first["rejected_unweighted"] = 1
	all := []map[string]any{first, velocityClassHour(1, 8, 3, 0)}
	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassPane(all, map[string][]map[string]any{"all": all}))
	cols := readPointsRow(t, p)
	if len(cols) != 3 {
		t.Fatalf("the points row has %d columns, want 3", len(cols))
	}

	want := fmt.Sprintf("%s · 4 points · 2 points rejected · 1 rejection without time", velocityPaneHour(0).Format("15:04"))
	if cols[0].Title != want {
		t.Errorf("the 14:00 tooltip is %q, want %q", cols[0].Title, want)
	}
}

// readPickRows counts the seat class rows the pane draws. A row is drawn only
// when the feed holds a class to pick, so a feed with none draws no row at all.
func readPickRows(t *testing.T, p *pageDOM) int {
	t.Helper()

	v, err := p.vm.RunString(`(function () {
  var n = 0;
  __walk(document.getElementById("trend"), function (e) {
    if (e.className === "vpick") n++;
  });
  return n;
})()`)
	if err != nil {
		t.Fatalf("counting the seat class rows: %v", err)
	}
	return int(v.ToInteger())
}

// A feed whose classes do not carry the whole town's series offers no "all"
// button: the pane presses the first class it holds and draws that class's rows.
func TestVelocityClassButtonsOmitAllWhenTheFeedHasNoAllSeries(t *testing.T) {
	t.Parallel()

	haiku := []map[string]any{velocityClassHour(0, 2, 1, 0), velocityClassHour(1, 1, 1, 0)}
	sonnet := []map[string]any{velocityClassHour(0, 4, 2, 0), velocityClassHour(1, 8, 3, 0)}
	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassPane(sonnet, map[string][]map[string]any{"haiku": haiku, "sonnet": sonnet}))

	want := []seatClassButton{{"haiku", true}, {"sonnet", false}}
	if got := readSeatClassButtons(t, p); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the class buttons are %+v, want %+v", got, want)
	}
	if got := pointsHeights(t, p); fmt.Sprint(got) != fmt.Sprint([][]string{{"hpts 100%"}, {"hpts 50%"}, nil}) {
		t.Errorf("the points row draws %v, want haiku's series", got)
	}
}

// A feed with no seat classes draws no pick row: there is no button to show, so
// an empty row would only add its margin under the landings bars.
func TestVelocityFeedWithoutClassesDrawsNoPickRow(t *testing.T) {
	t.Parallel()

	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityPaneReading())

	if got := readPickRows(t, p); got != 0 {
		t.Errorf("a feed with no seat classes draws %d pick rows, want none", got)
	}
}

// A feed with a single class draws no pick row either: one button chooses
// nothing, so the row is left out as it is for a feed with no classes.
func TestVelocityFeedWithOneClassDrawsNoPickRow(t *testing.T) {
	t.Parallel()

	all := []map[string]any{velocityClassHour(0, 4, 2, 0), velocityClassHour(1, 8, 3, 0)}
	p := newPageDOM(t)
	p.call(t, "renderTrend", velocityClassPane(all, map[string][]map[string]any{"all": all}))

	if got := readPickRows(t, p); got != 0 {
		t.Errorf("a feed with one seat class draws %d pick rows, want none", got)
	}
}
