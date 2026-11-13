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
// `gt pool show` (poolShowLive.seatCount). An overshooting seat of ten, two of
// them dead-hooked, reads the same split on both, so the count is not read as an
// over-admission past its cap of eight.
func TestSeatPictureGolden(t *testing.T) {
	t.Parallel()
	b := specdispatch.Budget{
		Seats:    []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 8}, {Agent: "claude-sonnet", Cap: 2}},
		MaxTotal: 10,
	}
	live := map[string]int{"deepseek-flash": 10, "claude-sonnet": 1}
	dead := map[string]int{"deepseek-flash": 2}
	b.SetLive(live)
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
