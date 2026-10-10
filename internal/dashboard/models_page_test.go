package dashboard

import (
	"strings"
	"testing"
	"time"
)

// The header strip sits in the header, where a glance at the page finds it
// beside the verdict and the health line (gt-bj47s).
func TestTheModelsStripIsInTheHeader(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	header := page[strings.Index(page, "<header>"):strings.Index(page, "</header>")]
	if !strings.Contains(header, `id="models"`) {
		t.Fatal("the header has no models strip (#models)")
	}
	if strings.Index(header, `id="healthline"`) > strings.Index(header, `id="models"`) {
		t.Error("the models strip is drawn above the health line it belongs beside")
	}
}

// The strip names a model, counts the seats and says om, and never leaves a
// field it could not read looking like a value (gt-bj47s).
func TestTheModelsStripReadsAsOneLine(t *testing.T) {
	t.Parallel()

	body := pageFunc(t, "renderModels")
	for _, want := range []string{
		`"polecats "`, `" · seats "`, `" · om "`,
		`m.polecat || "unknown"`, `m.om || "unknown"`,
		"m.seats.cap > 0 ? m.seats.live + " + `"/"` + " + m.seats.cap : String(m.seats.live)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderModels does not compose the strip's %s", want)
		}
	}
	// The seats are read before they are counted: a strip that has not read
	// them is not a town with no polecats.
	if !strings.Contains(body, "m.seats ?") {
		t.Error("renderModels counts seats it has not read")
	}
	if !strings.Contains(string(indexHTML), "renderAll() { if (!state) return; renderHealth(state.health); renderModels(state);") {
		t.Error("the page's redraw does not include the strip")
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
		Models: func() *Models { return &Models{Polecat: "deepseek-flash", SeatCap: 4, OM: "sonnet"} },
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
	if st.Models.Polecat != "deepseek-flash" || st.Models.OM != "sonnet" {
		t.Errorf("strip = %+v", st.Models)
	}
}
