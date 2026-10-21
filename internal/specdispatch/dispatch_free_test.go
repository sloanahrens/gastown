package specdispatch

import (
	"testing"
	"time"
)

// shutWindow is a seat window that is closed at every instant.
type shutWindow struct{}

func (shutWindow) Closed(time.Time) bool         { return true }
func (shutWindow) OpensAt(t time.Time) time.Time { return t.Add(time.Hour) }

func TestBudgetFreeSeats(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	var closed SeatWindow = shutWindow{}
	cases := []struct {
		name string
		b    Budget
		want int
	}{
		{"empty pool", Budget{Now: now}, 0},
		{"three seats of three, none live", Budget{Now: now, Seats: []Seat{{Agent: "a", Cap: 3}, {Agent: "b", Cap: 3}, {Agent: "c", Cap: 3}}}, 9},
		{"live seats subtract", Budget{Now: now, Seats: []Seat{{Agent: "a", Cap: 3, Live: 2}, {Agent: "b", Cap: 3, Live: 3}}}, 1},
		{"max_total bounds the sum", Budget{Now: now, MaxTotal: 4, LiveTotal: 1, Seats: []Seat{{Agent: "a", Cap: 3}, {Agent: "b", Cap: 3}}}, 3},
		{"over cap reads zero, never negative", Budget{Now: now, Seats: []Seat{{Agent: "a", Cap: 1, Live: 2}}}, 0},
		{"a closed window frees nothing", Budget{Now: time.Date(2026, 10, 21, 23, 0, 0, 0, time.UTC), Seats: []Seat{{Agent: "a", Cap: 3, Window: closed}}}, 0},
	}
	for _, tc := range cases {
		if got := tc.b.FreeSeats(); got != tc.want {
			t.Errorf("%s: FreeSeats = %d, want %d", tc.name, got, tc.want)
		}
	}
}
