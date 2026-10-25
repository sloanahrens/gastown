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

// The strip's seat counts are the dispatcher's roster: the live working seats
// a sling is refused against, never every polecat the inventory names. A
// polecat queued for landing or spawning holds no working seat, so a 3-cap
// seat with three working and two queued polecats reads 3/3, not 5/3
// (gt-ly90n).
func TestTheHeaderStripCountsTheDispatchersRoster(t *testing.T) {
	t.Parallel()

	const seat = "deepseek-flash-sandbox-k8s"
	now := time.Date(2026, 10, 24, 15, 47, 0, 0, time.UTC)
	h := NewHub(Config{
		Now: func() time.Time { return now },
		Models: func() *Models {
			return &Models{Unrecorded: seat, SeatCap: 3, OM: "sonnet",
				SeatRows: []ModelSeat{{Agent: seat, Cap: 3, State: "open"}}}
		},
		// The inventory the strip used to count: five polecats on the seat, of
		// which two hold no working seat.
		Summary: func() Summary {
			return Summary{Polecats: []Polecat{
				{Rig: "gastown", Name: "a", Agent: seat, State: StateWorking, CountsTowardCapacity: true},
				{Rig: "gastown", Name: "b", Agent: seat, State: StateWorking, CountsTowardCapacity: true},
				{Rig: "gastown", Name: "c", Agent: seat, State: StateWorking, CountsTowardCapacity: true},
				{Rig: "gastown", Name: "d", Agent: seat, State: StateQueued, CountsTowardCapacity: true},
				{Rig: "gastown", Name: "e", Agent: seat, State: StateSpawning, CountsTowardCapacity: true},
			}}
		},
		SeatRoster: func() SeatRoster {
			return SeatRoster{Live: map[string]int{seat: 3}, Total: 3, Read: true}
		},
	})

	h.pollSummary()
	h.pollModels()
	st := h.State()
	if st.Models == nil || st.Models.Seats == nil {
		t.Fatalf("the strip has no seats after the roster was read: %+v", st.Models)
	}
	if st.Models.Seats.Live != 3 || st.Models.Seats.Cap != 3 {
		t.Errorf("seat count = %d/%d, want the roster's 3/3, not the inventory's 5/3",
			st.Models.Seats.Live, st.Models.Seats.Cap)
	}
	if row := st.Models.SeatRows[0]; row.Live == nil || *row.Live != 3 {
		t.Errorf("%s live = %v, want the roster's 3", seat, row.Live)
	}
	if st.Models.SeatsUnreadable {
		t.Error("a roster read that succeeded is flagged unreadable")
	}
	if st.Models.Unrecorded != seat || st.Models.OM != "sonnet" {
		t.Errorf("strip = %+v", st.Models)
	}
}

// A seat count the strip does not have says which way it is missing: a roster
// read that failed is unknown, not a read still to come (gt-ly90n).
func TestTheHeaderStripTellsAFailedRosterReadFromOneNotMade(t *testing.T) {
	t.Parallel()

	models := func() *Models {
		return &Models{SeatCap: 3, SeatRows: []ModelSeat{{Agent: "deepseek-flash-sandbox-k8s", Cap: 3, State: "open"}}}
	}

	// No roster source at all: the count has not been read.
	h := NewHub(Config{Models: models})
	h.pollModels()
	st := h.State()
	if st.Models == nil || st.Models.Seats != nil || st.Models.SeatsUnreadable {
		t.Fatalf("a strip with no roster source = %+v, want no count and not the failed flag", st.Models)
	}
	if live := st.Models.SeatRows[0].Live; live != nil {
		t.Errorf("a row without a roster read has live = %d", *live)
	}

	// A roster read that failed is not the same reading as one not made.
	h = NewHub(Config{Models: models, SeatRoster: func() SeatRoster { return SeatRoster{} }})
	h.pollModels()
	st = h.State()
	if st.Models == nil || !st.Models.SeatsUnreadable {
		t.Fatalf("a failed roster read = %+v, want the unreadable flag", st.Models)
	}
	if st.Models.Seats != nil {
		t.Errorf("a failed roster read published seats: %+v", st.Models.Seats)
	}

	// The next poll that reads the roster clears the flag and fills the count.
	h = NewHub(Config{Models: models, SeatRoster: func() SeatRoster {
		return SeatRoster{Live: map[string]int{"deepseek-flash-sandbox-k8s": 1}, Total: 1, Read: true}
	}})
	h.pollModels()
	st = h.State()
	if st.Models.SeatsUnreadable {
		t.Error("a successful roster read kept the unreadable flag")
	}
	if st.Models.Seats == nil || st.Models.Seats.Live != 1 {
		t.Errorf("seats = %+v, want 1/3", st.Models.Seats)
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

// The panel's live count is each seat's slice of the dispatcher's roster: a
// seat the roster names no live polecat for reads 0, never a count of its own,
// and a closed seat has no count (gt-lqa6p, gt-ly90n).
func TestTheModelsPanelKeepsTheRostersPerSeatCount(t *testing.T) {
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
		SeatRoster: func() SeatRoster {
			return SeatRoster{Live: map[string]int{"claude-sonnet": 2}, Total: 2, Read: true}
		},
	})

	h.pollModels()
	rows := h.State().Models.SeatRows
	if rows[0].Live == nil || *rows[0].Live != 2 {
		t.Errorf("claude-sonnet seat live = %v, want 2", rows[0].Live)
	}
	// The roster read succeeded, so an empty seat is a zero it counted.
	if rows[1].Live == nil || *rows[1].Live != 0 {
		t.Errorf("deepseek-flash seat live = %v, want 0", rows[1].Live)
	}
	if rows[2].Live != nil {
		t.Errorf("a closed seat has a live count: %d", *rows[2].Live)
	}
}
