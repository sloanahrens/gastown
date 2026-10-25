package dashboard

import (
	"strings"
	"testing"
	"time"
)

// The strip sits in the header, where a glance at the page finds it, and keeps
// the elements the watcher reads by id: the verdict, the health line and the
// connection state. The tiles grid it replaced is gone, numbers and all
// (gt-ywlei).
func TestTheStripIsInTheHeader(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	header := page[strings.Index(page, "<header>"):strings.Index(page, "</header>")]
	for _, id := range []string{"verdict", "healthline", "conn", "strip", "models"} {
		if !strings.Contains(header, `id="`+id+`"`) {
			t.Errorf("the header has no #%s", id)
		}
	}
	if strings.Index(header, `id="healthline"`) > strings.Index(header, `id="strip"`) {
		t.Error("the strip is drawn above the health line it belongs beside")
	}
	// One strip: a second container of the same facts is how the top of the
	// page came to say each thing four times.
	if n := strings.Count(page, `id="strip"`); n != 1 {
		t.Errorf("the page has %d strips, want 1", n)
	}
	if strings.Contains(page, `id="tiles"`) {
		t.Error("the page still draws the tiles grid")
	}
	for _, gone := range []string{"function renderTiles(s)", "function renderModels(s)", "function tile(k, v, sub, cls)"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still has %s, whose output the strip replaced", gone)
		}
	}
}

// The models line folds the seat and role tables it carries into one line of
// the strip. It is closed until the viewer opens it, and the choice persists
// (gt-ywlei).
func TestTheModelsLineFoldsTheTablesUnderIt(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	if strings.Index(page, `id="modelspanel"`) < strings.Index(page, "</header>") {
		t.Error("the models panel is not below the header")
	}
	if !strings.Contains(page, `<button id="models" type="button" aria-expanded="false" aria-controls="modelspanel" title="every seat, and the model each role runs">models ▸</button>`) {
		t.Error("the models line is not a closed button in the header strip")
	}
	if !strings.Contains(page, `<div id="modelspanel" hidden></div>`) {
		t.Error("the models panel is not closed by default")
	}
	head := pageFunc(t, "paintModelsLine")
	for _, want := range []string{
		`b.textContent = "models " + (modelsOpen ? "▾" : "▸")`,
		`b.setAttribute("aria-expanded", modelsOpen ? "true" : "false")`,
		`$("modelspanel").hidden = !modelsOpen`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("paintModelsLine has no %q", want)
		}
	}
	// The choice is the viewer's and survives the load, in a key of its own:
	// the panes' folds sit in gt-collapsed keyed by their titles, where a
	// stored true means collapsed, and a key of its own needs no reader to
	// invert it (gt-ywlei).
	for _, want := range []string{
		`const MODELS_OPEN_KEY = "gt-models-open";`,
		`modelsOpen = localStorage.getItem(MODELS_OPEN_KEY) === "1";`,
		`localStorage.setItem(MODELS_OPEN_KEY, modelsOpen ? "1" : "0");`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the models fold does not remember the viewer's choice: no %q", want)
		}
	}
	if strings.Contains(pageFunc(t, "toggleModels"), "gt-collapsed") {
		t.Error("the models fold writes the panes' own store, where a stored true means collapsed")
	}
	if !strings.Contains(page, `$("models").addEventListener("click", toggleModels);`) {
		t.Error("the models line has nothing to open it")
	}
	if !strings.Contains(pageFunc(t, "renderAll"), "renderStrip(state); renderModelsPanel(state);") {
		t.Error("the page's redraw does not draw the strip and the panel it opens")
	}

	body := pageFunc(t, "renderModelsPanel")
	for _, want := range []string{`"drift"`, `rr.model || "unknown"`, "sr.key", "rr.key", "rr.drift",
		// One row per agent in the seat map, with the state gt pool show
		// prints beside it: an open seat, a cap of 0, or a closed window.
		"sr.agent", "sr.state"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderModelsPanel does not draw %s", want)
		}
	}
	// A closed line keeps no table in the DOM, and one with nothing to open
	// says so in a line rather than showing an empty box (gt-ywlei).
	for _, want := range []string{"if (!modelsOpen) { box.replaceChildren(); return; }", `el("div", "empty", "no seats and no roles have been read yet")`} {
		if !strings.Contains(body, want) {
			t.Errorf("renderModelsPanel has no %q", want)
		}
	}
}

// The strip reads the models from the configs, but the seats are the polecats
// the rest of the page counts: a strip that counted its own would be a second
// reading of the town, and free to disagree with the seats pane. A seat is held
// by the polecats the dispatcher counts toward capacity, not by every polecat
// the town has: the idle done ones are not sitting in a seat (gt-nkvrk).
func TestTheHeaderStripCountsTheSeatsThePageCounts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Now:    func() time.Time { return now },
		Models: func() *Models { return &Models{Unrecorded: "deepseek-flash", SeatCap: 4, OM: "sonnet"} },
		Summary: func() Summary {
			return Summary{Polecats: []Polecat{
				{Rig: "gastown", Name: "agate", CountsTowardCapacity: true},
				{Rig: "gastown", Name: "basalt", CountsTowardCapacity: true},
				// Three polecats the dispatcher does not count: idle, done, no
				// work. They hold no seat.
				{Rig: "gastown", Name: "chert"},
				{Rig: "gastown", Name: "dolomite"},
				{Rig: "gastown", Name: "epidote"},
				// A parked rig is stood down on purpose: even a
				// capacity-counting polecat there holds no seat.
				{Rig: "old", Name: "ceded", CountsTowardCapacity: true, RigParked: true},
			}}
		},
	})

	// Before the seats have been read, the strip says so rather than showing
	// an empty town.
	h.pollModels()
	st := h.State()
	if st.Models == nil || st.Models.Seats != nil {
		t.Fatalf("the strip counted seats before any were read: %+v", st.Models)
	}

	h.pollSummary()
	h.pollModels()
	st = h.State()
	if st.Models == nil || st.Models.Seats == nil {
		t.Fatalf("the strip has no seats after the seats were read: %+v", st.Models)
	}
	if st.Models.Seats.Live != 2 || st.Models.Seats.Cap != 4 {
		t.Errorf("seats = %+v, want 2/4", *st.Models.Seats)
	}
	if st.Models.Unrecorded != "deepseek-flash" || st.Models.OM != "sonnet" {
		t.Errorf("strip = %+v", st.Models)
	}
}

// The models panel's seat table prints the two sandbox seats by their short
// names and keeps each full name on hover (gt-2mnrl).
func TestTheModelsPanelNamesTheSandboxSeatsByTheirShortNames(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if _, err := dom.vm.RunString("modelsOpen = true"); err != nil {
		t.Fatalf("opening the models panel: %v", err)
	}
	dom.call(t, "renderModelsPanel", map[string]any{
		"models": map[string]any{"seat_rows": []any{
			map[string]any{"agent": "deepseek-flash-sandbox", "cap": 3, "state": "open", "live": 1, "key": "gastown.deepseek-flash-sandbox"},
			map[string]any{"agent": "deepseek-flash-sandbox-k8s", "cap": 3, "state": "open", "live": 0, "key": "gastown.deepseek-flash-sandbox-k8s"},
		}},
	})
	for _, c := range []struct{ shown, full, key string }{
		{"docker-sbx", "deepseek-flash-sandbox", "gastown.deepseek-flash-sandbox"},
		{"k8s-sbx", "deepseek-flash-sandbox-k8s", "gastown.deepseek-flash-sandbox-k8s"},
	} {
		cell := dom.cellWithText(t, "modelspanel", c.shown)
		if want := c.full + " · " + c.key; cell.Title != want {
			t.Errorf("the models panel's %q carries %q on hover, want %q", c.shown, cell.Title, want)
		}
	}
}

// The models panel's activity table names each agent by the short name the seat
// table above it prints and keeps the whole name on hover, so a row's counts are
// on the seat they name (gt-14gjd).
func TestTheModelsPanelActivityTableNamesAgentsByTheirShortNames(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	if _, err := dom.vm.RunString("modelsOpen = true"); err != nil {
		t.Fatalf("opening the models panel: %v", err)
	}
	dom.call(t, "renderModelsPanel", map[string]any{
		"models": map[string]any{"activity": []any{
			map[string]any{"agent": "deepseek-flash-sandbox", "landed": 2, "rejected": 1},
			map[string]any{"agent": "deepseek-flash-sandbox-k8s", "landed": 0, "rejected": 3},
		}},
	})
	for i, c := range []struct{ shown, full, landed, rejected string }{
		{"docker-sbx", "deepseek-flash-sandbox", "2", "1"},
		{"k8s-sbx", "deepseek-flash-sandbox-k8s", "0", "3"},
	} {
		cell := dom.cellWithText(t, "modelspanel", c.shown)
		if cell.Title != c.full {
			t.Errorf("the activity table's %q carries title %q, want the full name %q", c.shown, cell.Title, c.full)
		}
		// The counts stay with the row, so a short name never stands over
		// another agent's numbers. The activity table is the panel's third:
		// the seat and role tables are drawn above it even when empty.
		for j, want := range []string{c.shown, c.landed, c.rejected} {
			if got := dom.descend(t, "modelspanel", 2, i+1, j).Text; got != want {
				t.Errorf("the activity row %d, cell %d = %q, want %q", i, j, got, want)
			}
		}
	}
}

// A working polecat's row names the model its session runs (gt-lqa6p).
func TestThePolecatPaneNamesEachPolecatsModel(t *testing.T) {
	t.Parallel()

	if !strings.Contains(pageFunc(t, "workRow"), "p.agent") {
		t.Error("the Work panel's seat rows do not show the polecat's agent")
	}
}

// The panel's live count is the polecats the seats pane counts, on each seat's
// agent: one on an old model does not fill a seat it does not hold, and a
// closed seat has no count (gt-lqa6p).
func TestTheModelsPanelCountsLivePolecatsPerSeat(t *testing.T) {
	t.Parallel()

	h := NewHub(Config{
		Now: func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) },
		Models: func() *Models {
			return &Models{Unrecorded: "claude-sonnet", SeatRows: []ModelSeat{
				{Agent: "claude-sonnet", Cap: 3},
				{Agent: "deepseek-flash", Cap: 2},
				{Agent: "claude-haiku", Cap: 0, Closed: true},
			}}
		},
		Summary: func() Summary {
			return Summary{Polecats: []Polecat{
				{Rig: "gastown", Name: "a", Agent: "claude-sonnet", CountsTowardCapacity: true},
				// No recorded agent: the polecat role's own.
				{Rig: "gastown", Name: "b", CountsTowardCapacity: true},
				{Rig: "gastown", Name: "c", Agent: "deepseek-flash", CountsTowardCapacity: true},
				{Rig: "gastown", Name: "d", Agent: "deepseek-flash"},
				{Rig: "old", Name: "e", Agent: "claude-sonnet", CountsTowardCapacity: true, RigParked: true},
			}}
		},
	})

	h.pollModels()
	for _, s := range h.State().Models.SeatRows {
		if s.Live != nil {
			t.Fatalf("seat %q has a live count before the polecats were read", s.Agent)
		}
	}
	h.pollSummary()
	h.pollModels()
	rows := h.State().Models.SeatRows
	if rows[0].Live == nil || *rows[0].Live != 2 {
		t.Errorf("claude-sonnet seat live = %v, want 2", rows[0].Live)
	}
	if rows[1].Live == nil || *rows[1].Live != 1 {
		t.Errorf("deepseek-flash seat live = %v, want 1", rows[1].Live)
	}
	if rows[2].Live != nil {
		t.Errorf("a closed seat has a live count: %d", *rows[2].Live)
	}
}
