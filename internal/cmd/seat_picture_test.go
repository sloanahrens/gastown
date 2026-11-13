package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// TestSeatPictureGolden pins the seat count the two readers print from one
// fixture: the tick line (specdispatch.Budget.Picture) and the seat column of
// `gt pool show` (poolShowLive.seatCount). The tick's inputs are the roster's
// live count, its dead-hooked part and the slings still running per agent, built
// as the tick builds them. A seat reading above its cap splits into its three
// parts, so the overshoot is not read as an admission past the cap; a seat with
// no dead-hooked seat and no sling in flight keeps the short wording.
func TestSeatPictureGolden(t *testing.T) {
	t.Parallel()
	b := specdispatch.Budget{
		Seats: []specdispatch.Seat{
			{Agent: "deepseek-flash", Cap: 8},
			{Agent: "claude-sonnet", Cap: 2},
			{Agent: "sbx-haiku", Cap: 9},
			{Agent: "claude-haiku", Cap: 4},
		},
		MaxTotal: 10,
	}
	// The roster read, the dead-hooked part of it, and the running slings. The
	// haiku seat is the 11/9 reading: roster 9 with one dead-hooked, plus two
	// slings still running.
	live := map[string]int{"deepseek-flash": 10, "claude-sonnet": 1, "sbx-haiku": 9, "claude-haiku": 3}
	dead := map[string]int{"sbx-haiku": 1, "claude-haiku": 2}
	running := []specInFlight{
		{Agent: "deepseek-flash"}, {Agent: "deepseek-flash"},
		{Agent: "sbx-haiku"}, {Agent: "sbx-haiku"},
	}
	b.SetLive(specLiveWithInFlight(live, running))
	b.SetInFlight(specInFlightByAgent(running))
	b.SetDeadHooked(dead)
	show := poolShowLive{live: live, dead: dead, ok: true}

	var got strings.Builder
	got.WriteString("tick: " + b.Picture() + "\n")
	for _, s := range b.Seats {
		got.WriteString("show: " + s.Agent + " " + show.seatCount(s.Agent, s.Cap) + "\n")
	}

	want, err := os.ReadFile(filepath.Join("testdata", "seat_picture.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("seat picture differs from testdata/seat_picture.golden:\n--- got\n%s--- want\n%s", got.String(), want)
	}
}
