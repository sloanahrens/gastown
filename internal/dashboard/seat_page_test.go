package dashboard

import (
	"encoding/json"
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
