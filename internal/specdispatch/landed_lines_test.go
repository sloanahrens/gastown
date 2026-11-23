package specdispatch

import "testing"

// The band edges are half-open: 5 opens the 5 to 7 band and 4.99 stays in the
// first.
func TestLandedLinesFindsTheBand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		minutes float64
		want    LinesBand
	}{
		{0, LinesBand{0, 5, 70, 2, 4, 9, 27}},
		{4.99, LinesBand{0, 5, 70, 2, 4, 9, 27}},
		{5, LinesBand{5, 7, 293, 10, 30, 64, 101}},
		{7, LinesBand{7, 9, 721, 38, 67, 119, 236}},
		{9, LinesBand{9, 10, 647, 58, 109, 216, 454}},
		{10, LinesBand{10, 12, 489, 111, 246, 468, 794}},
		{12, LinesBand{12, 16, 76, 139, 256, 475, 859}},
		{15.99, LinesBand{12, 16, 76, 139, 256, 475, 859}},
	} {
		got, found := LandedLines(tc.minutes)
		if !found || got != tc.want {
			t.Errorf("LandedLines(%v) = %+v, found %v; want %+v", tc.minutes, got, found, tc.want)
		}
	}
}

// The table stops below 0 and at 16 and above.
func TestLandedLinesOutsideTheTable(t *testing.T) {
	t.Parallel()
	for _, minutes := range []float64{-1, 16, 20} {
		if _, found := LandedLines(minutes); found {
			t.Errorf("LandedLines(%v) found a band, want none", minutes)
		}
	}
}

// Refuse is the band's P25.
func TestLandedLinesRefuse(t *testing.T) {
	t.Parallel()
	for minutes, want := range map[float64]int{7: 38, 9: 58} {
		band, found := LandedLines(minutes)
		if !found || band.Refuse() != want {
			t.Errorf("LandedLines(%v).Refuse() = %d, found %v, want %d", minutes, band.Refuse(), found, want)
		}
	}
}

// Advise is half of P90 rounded up: 27 gives 14, not 13.
func TestLandedLinesAdvise(t *testing.T) {
	t.Parallel()
	for minutes, want := range map[float64]int{4: 14, 7: 118, 9: 227} {
		band, found := LandedLines(minutes)
		if !found || band.Advise() != want {
			t.Errorf("LandedLines(%v).Advise() = %d, found %v, want %d", minutes, band.Advise(), found, want)
		}
	}
}
