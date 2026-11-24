package dashboard

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// seatAgeCell reads the Age cell of the named seat's row: its own text and the
// text of each child element by class, so a second line the row draws inside the
// cell is read apart from the since-sling time (gt-qjl0ym).
func seatAgeCell(t *testing.T, dom *pageDOM, name string) (string, map[string]string) {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		let out = null;
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (String(c.className).split(" ").indexOf("workrow") >= 0) {
					const spans = c.children.__arr.filter(x => x.tagName === "SPAN");
					if (!out && spans[0] && spans[0].textContent === ` + quoteJS(name) + `) {
						const subs = {};
						spans[4].children.__arr.forEach(k => { subs[String(k.className)] = String(k.textContent); });
						out = {text: String(spans[4]._text), subs: subs};
					}
				}
				walk(c);
			}
		})(__byId["polecats"]);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the Age cell of %s: %v", name, err)
	}
	var cell *struct {
		Text string            `json:"text"`
		Subs map[string]string `json:"subs"`
	}
	if err := json.Unmarshal([]byte(v.String()), &cell); err != nil || cell == nil {
		t.Fatalf("seat %s has no row (%v)", name, err)
	}
	return cell.Text, cell.Subs
}

// seatRowText reads the whole text of the named seat's row, every element under
// it included, so a test can assert a label the row used to draw is gone from
// the row rather than only from the Age cell.
func seatRowText(t *testing.T, dom *pageDOM, name string) string {
	t.Helper()

	v, err := dom.vm.RunString(`(() => {
		let out = null;
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (String(c.className).split(" ").indexOf("workrow") >= 0) {
					const spans = c.children.__arr.filter(x => x.tagName === "SPAN");
					if (!out && spans[0] && spans[0].textContent === ` + quoteJS(name) + `) out = String(c.textContent);
				}
				walk(c);
			}
		})(__byId["polecats"]);
		return out;
	})()`)
	if err != nil {
		t.Fatalf("reading the row of %s: %v", name, err)
	}
	if v.String() == "null" {
		t.Fatalf("seat %s has no row", name)
	}
	return v.String()
}

// seatChild is one element the page drew inside the Age cell: its tag, its
// class and its text, so a test can hold the cell to drawing its readings as
// inline spans in the order it means them (gt-l2gv8u).
type seatChild struct {
	Tag   string `json:"tag"`
	Class string `json:"class"`
	Text  string `json:"text"`
}

// seatAgeChildren reads the Age cell of the named seat's row as its child
// elements in the order the page appended them.
func seatAgeChildren(t *testing.T, dom *pageDOM, name string) []seatChild {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		let out = null;
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (String(c.className).split(" ").indexOf("workrow") >= 0) {
					const spans = c.children.__arr.filter(x => x.tagName === "SPAN");
					if (!out && spans[0] && spans[0].textContent === ` + quoteJS(name) + `) {
						out = spans[4].children.__arr.map(k => ({tag: String(k.tagName), class: String(k.className), text: String(k.textContent)}));
					}
				}
				walk(c);
			}
		})(__byId["polecats"]);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the Age cell children of %s: %v", name, err)
	}
	if v.String() == "null" {
		t.Fatalf("seat %s has no row", name)
	}
	var out []seatChild
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the Age cell children of %s: %v", name, err)
	}
	return out
}

// TestSeatRowAgeCellIsOneLine: a seat charged gate time and refused twice draws
// its readings inline in the Age cell — spans after the duration, in the order
// split, round, rework — so the seat is one line instead of four; the rework
// reading is warned from the second refusal on (gt-l2gv8u).
func TestSeatRowAgeCellIsOneLine(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	s := seat("gastown", "agate", "working", "gt-abc", "a title", "flash", 30*time.Minute)
	s["gate_sec"], s["working_sec"] = 600.0, 1200.0
	s["reject_count"] = 2.0
	callWork(t, dom, workState([]any{s}, nil))

	// The duration stays the cell's own text; the readings follow it as spans.
	if text, _ := seatAgeCell(t, dom, "agate"); text != "30m" {
		t.Errorf("Age cell text = %q, want the duration 30m before the readings", text)
	}
	kids := seatAgeChildren(t, dom, "agate")
	want := []seatChild{
		{Tag: "SPAN", Class: "wsub", Text: "working 20m, gate 10m"},
		{Tag: "SPAN", Class: "wround", Text: "round 3"},
		{Tag: "SPAN", Class: "wrework warnc", Text: "rework ×2"},
	}
	if len(kids) != len(want) {
		t.Fatalf("the Age cell drew %d readings (%+v), want %d: the split, the round and the rework", len(kids), kids, len(want))
	}
	for i, w := range want {
		if kids[i] != w {
			t.Errorf("Age cell child %d = %+v, want %+v", i, kids[i], w)
		}
	}
	for _, k := range kids {
		if k.Tag != "SPAN" {
			t.Errorf("the Age cell draws a %s child (%+v); a block of its own makes the seat a line taller", k.Tag, k)
		}
	}
}

// TestSeatRowHoverKeepsTheFullTitle: capping the bead title near 40 characters
// ellipses the cell but loses nothing — the title span carries the cap class
// and the bead cell's hover still carries the full id, title and description
// (gt-l2gv8u).
func TestSeatRowHoverKeepsTheFullTitle(t *testing.T) {
	t.Parallel()

	const (
		id    = "gt-abc"
		title = "a title long enough that the cap ellipses it"
		desc  = "the body of the bead, which the hover keeps whole"
	)
	dom := newPageDOM(t)
	s := seat("gastown", "agate", "working", id, title, "flash", 30*time.Minute)
	s["description"] = desc
	callWork(t, dom, workState([]any{s}, nil))

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		let out = null;
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (String(c.className).split(" ").indexOf("workrow") >= 0) {
					const spans = c.children.__arr.filter(x => x.tagName === "SPAN");
					if (!out && spans[0] && spans[0].textContent === ` + quoteJS("agate") + `) {
						const title = spans[2].children.__arr.find(x => String(x.className).split(" ").indexOf("wtitle") >= 0);
						out = {hover: String(spans[2].title), cls: title ? String(title.className) : "", text: title ? String(title.textContent) : ""};
					}
				}
				walk(c);
			}
		})(__byId["polecats"]);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the bead cell of agate: %v", err)
	}
	var cell struct {
		Hover string `json:"hover"`
		Cls   string `json:"cls"`
		Text  string `json:"text"`
	}
	if v.String() == "null" {
		t.Fatal("seat agate has no row")
	}
	if err := json.Unmarshal([]byte(v.String()), &cell); err != nil {
		t.Fatalf("reading the bead cell of agate: %v", err)
	}

	t.Run("the hover keeps the whole bead", func(t *testing.T) {
		if want := id + " " + title + "\n\n" + desc; cell.Hover != want {
			t.Errorf("the bead cell hovers %q, want %q", cell.Hover, want)
		}
	})
	t.Run("the title span carries the cap class", func(t *testing.T) {
		if !slices.Contains(strings.Fields(cell.Cls), "wcap") {
			t.Errorf("the title span's class = %q, want it to carry wcap", cell.Cls)
		}
		if cell.Text != title {
			t.Errorf("the title span reads %q, want the whole title %q", cell.Text, title)
		}
	})
	t.Run("the cap is a width with an ellipsis", func(t *testing.T) {
		rule := cssRule(t, ".workrow .wcap")
		for _, want := range []string{"max-width:40ch", "overflow:hidden", "text-overflow:ellipsis", "white-space:nowrap"} {
			if !strings.Contains(rule, want) {
				t.Errorf("the .wcap rule has no %q: %s", want, rule)
			}
		}
	})
}

// TestSeatRowTimes: a seat whose guard charged gate time draws "working Xm, gate
// Ym" under its since-sling time; a seat with no gate time draws no second line;
// and a poll that changes only gate_sec redraws the table, so the line appears
// without another field moving (polecatsKey, gt-qjl0ym).
func TestSeatRowTimes(t *testing.T) {
	t.Parallel()
	drawn := func(t *testing.T, gate float64) (string, map[string]string, *pageDOM) {
		dom := newPageDOM(t)
		s := seat("gastown", "agate", "working", "gt-abc", "a title", "flash", 30*time.Minute)
		if gate > 0 {
			s["gate_sec"], s["working_sec"] = gate, 1800-gate
		}
		callWork(t, dom, workState([]any{s}, nil))
		text, subs := seatAgeCell(t, dom, "agate")
		return text, subs, dom
	}
	t.Run("gate time is drawn", func(t *testing.T) {
		text, subs, _ := drawn(t, 600)
		if text != "30m" || subs["wsub"] != "working 20m, gate 10m" {
			t.Errorf("Age cell = %q with %v, want 30m and a wsub line of working 20m, gate 10m", text, subs)
		}
	})
	t.Run("no gate time draws no second line", func(t *testing.T) {
		if text, subs, _ := drawn(t, 0); text != "30m" || len(subs) != 0 {
			t.Errorf("Age cell = %q with %v, want 30m and no child", text, subs)
		}
	})
	t.Run("a change in only gate_sec redraws", func(t *testing.T) {
		_, _, dom := drawn(t, 0)
		s := seat("gastown", "agate", "working", "gt-abc", "a title", "flash", 30*time.Minute)
		s["gate_sec"], s["working_sec"] = 600.0, 1200.0
		st := workState([]any{s}, nil)
		dom.call(t, "renderPolecats", st)
		if _, subs := seatAgeCell(t, dom, "agate"); subs["wsub"] != "working 20m, gate 10m" {
			t.Errorf("after only gate_sec changed the cell has %v, want the second line", subs)
		}
	})
}

// TestSeatRowUnchanged: a seat carrying none of the new fields draws its
// since-sling time and nothing else in the Age cell, as before.
func TestSeatRowUnchanged(t *testing.T) {
	t.Parallel()
	dom := newPageDOM(t)
	callWork(t, dom, workState([]any{seat("gastown", "agate", "working", "gt-abc", "a title", "flash", 12*time.Minute)}, nil))
	if text, subs := seatAgeCell(t, dom, "agate"); text != "12m" || len(subs) != 0 {
		t.Errorf("Age cell = %q with %v, want 12m and no child", text, subs)
	}
}

// TestSeatRowStartingAndRound: a seat still read as starting draws the working
// and gate split like any other seat that has been charged gate time — the
// phase draws no badge of its own — and a seat refused for review draws "round
// N" with N one above its reject_count, redrawn when only the count moves
// (gt-urycmq, gt-qjl0ym part 2).
func TestSeatRowStartingAndRound(t *testing.T) {
	t.Parallel()
	seatWith := func(fields map[string]any) map[string]any {
		s := seat("gastown", "agate", "working", "gt-abc", "a title", "flash", 30*time.Minute)
		s["gate_sec"], s["working_sec"] = 600.0, 1200.0
		for k, v := range fields {
			s[k] = v
		}
		return s
	}
	cases := []struct {
		name   string
		fields map[string]any
		want   map[string]string
	}{
		{"a starting phase draws the times, no badge", map[string]any{"phase": "starting"}, map[string]string{"wsub": "working 20m, gate 10m"}},
		{"working draws the times and no badge", map[string]any{"phase": "working"}, map[string]string{"wsub": "working 20m, gate 10m"}},
		{"one rejection is round 2", map[string]any{"phase": "working", "reject_count": 1.0}, map[string]string{"wsub": "working 20m, gate 10m", "wround": "round 2", "wrework": "rework ×1"}},
		{"no rejection draws no round", map[string]any{"reject_count": 0.0}, map[string]string{"wsub": "working 20m, gate 10m"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dom := newPageDOM(t)
			callWork(t, dom, workState([]any{seatWith(c.fields)}, nil))
			if _, subs := seatAgeCell(t, dom, "agate"); !maps.Equal(subs, c.want) {
				t.Errorf("Age cell children = %v, want %v", subs, c.want)
			}
		})
	}
	// Each field the row shows is in polecatsKey on its own: a poll that moves
	// only that field must redraw. The phase is no longer one of them — nothing
	// in the row reads it (gt-urycmq).
	for _, c := range []struct {
		name         string
		before, then map[string]any
		want         map[string]string
	}{
		{"reject_count", map[string]any{"phase": "working"}, map[string]any{"phase": "working", "reject_count": 1.0}, map[string]string{"wsub": "working 20m, gate 10m", "wround": "round 2", "wrework": "rework ×1"}},
	} {
		t.Run("a change in only the "+c.name+" redraws", func(t *testing.T) {
			dom := newPageDOM(t)
			callWork(t, dom, workState([]any{seatWith(c.before)}, nil))
			dom.call(t, "renderPolecats", workState([]any{seatWith(c.then)}, nil))
			if _, subs := seatAgeCell(t, dom, "agate"); !maps.Equal(subs, c.want) {
				t.Errorf("after only %s changed the cell has %v, want %v", c.name, subs, c.want)
			}
		})
	}
}

// TestSeatRowStartingDrawsNoBadge: a seat the feed still reads as starting,
// holding a bead but charged no gate time, draws no second line and no element
// of its row reads "starting" — the badge that made every sandbox seat read the
// same is gone (gt-urycmq).
func TestSeatRowStartingDrawsNoBadge(t *testing.T) {
	t.Parallel()
	dom := newPageDOM(t)
	s := seat("gastown", "agate", "working", "gt-abc", "a title", "flash", 30*time.Minute)
	s["phase"] = "starting"
	callWork(t, dom, workState([]any{s}, nil))
	t.Run("a starting seat with no gate time draws no sub-line", func(t *testing.T) {
		if text, subs := seatAgeCell(t, dom, "agate"); text != "30m" || len(subs) != 0 {
			t.Errorf("Age cell = %q with %v, want 30m and no child", text, subs)
		}
	})
	t.Run("no element in the row reads starting", func(t *testing.T) {
		if row := seatRowText(t, dom, "agate"); strings.Contains(row, "starting") {
			t.Errorf("the row of seat agate reads %q, want no 'starting' anywhere in it", row)
		}
	})
}

// TestPolecatRowReworkPageDraws: a seat whose bead was refused for review draws
// "rework ×N" in its Age cell, warned from the second refusal on; a 0 count,
// sent or left off, draws nothing and leaves the row as an explicit zero's (gt-addb76).
func TestPolecatRowReworkPageDraws(t *testing.T) {
	t.Parallel()

	// draw feeds the panel one seat, carrying reject_count only when count is
	// not nil, so nil is the row the JSON's omitempty sends.
	draw := func(t *testing.T, count any) *pageDOM {
		t.Helper()
		s := seat("gastown", "agate", "working", "gt-abc", "a title", "flash", 30*time.Minute)
		if count != nil {
			s["reject_count"] = count
		}
		dom := newPageDOM(t)
		callWork(t, dom, workState([]any{s}, nil))
		return dom
	}
	// pill reads the rework child out of an Age cell: its text and whether it warns.
	pill := func(subs map[string]string) (string, bool) {
		for cls, text := range subs {
			if slices.Contains(strings.Fields(cls), "wrework") {
				return text, slices.Contains(strings.Fields(cls), "warnc")
			}
		}
		return "", false
	}

	cases := []struct {
		name   string
		count  any
		pill   string
		warned bool
	}{
		{"one refusal draws rework ×1", 1.0, "rework ×1", false},
		{"two refusals draw the pill warned", 2.0, "rework ×2", true},
		{"an explicit zero draws nothing", 0.0, "", false},
		{"an absent count draws nothing", nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, subs := seatAgeCell(t, draw(t, c.count), "agate")
			if got, warned := pill(subs); got != c.pill || warned != c.warned {
				t.Errorf("rework pill = %q (warn %v), want %q (warn %v); Age cell children %v", got, warned, c.pill, c.warned, subs)
			}
		})
	}
	t.Run("an absent count leaves the row as an explicit zero draws it", func(t *testing.T) {
		zero, absent := workRows(t, draw(t, 0.0)), workRows(t, draw(t, nil))
		if !reflect.DeepEqual(zero, absent) {
			t.Errorf("the count-absent row's cells = %q, want the explicit-zero row %q", absent, zero)
		}
	})
}
