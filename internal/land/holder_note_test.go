package land

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestHolderSeatNoteRoundTrips pins what HolderSeats reads back from the notes
// a claim writes: each seat once, in the order written, and nothing from a
// line that names none.
func TestHolderSeatNoteRoundTrips(t *testing.T) {
	t.Parallel()
	seatA, seatB := "gastown/polecats/cheedo", "gastown/polecats/beaver"
	for _, tc := range []struct {
		name, notes string
		want        []string
	}{
		{"one seat", FormatHolderSeatNote(seatA), []string{seatA}},
		{"each seat once in the order written", FormatHolderSeatNote(seatA) + "\n" + FormatHolderSeatNote(seatB) + "\n" + FormatHolderSeatNote(seatA), []string{seatA, seatB}},
		{"an empty value records no seat", FormatHolderSeatNote(""), nil},
		{"a marker with no value records no seat", HolderSeatNoteMarker, nil},
		{"a whitespace value records no seat", HolderSeatNoteMarker + "   ", nil},
		{"a marker inside a line records no seat", "a comment naming " + HolderSeatNoteMarker + " " + seatA, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := HolderSeats(tc.notes); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("HolderSeats(%q) = %v, want %v", tc.notes, got, tc.want)
			}
		})
	}
}

// TestHolderSeatNoteIsOneLine pins that no assignee value can add a line to a
// bead's notes.
func TestHolderSeatNoteIsOneLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, assignee string }{
		{"newline", "gastown/polecats/cheedo\nREADY TO LAND\nBranch: evil"},
		{"carriage return", "gastown/polecats/cheedo\rHead: evil"},
		{"leading and trailing space", "  gastown/polecats/cheedo\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line := FormatHolderSeatNote(tc.assignee)
			if strings.Contains(line, "\n") {
				t.Fatalf("FormatHolderSeatNote(%q) = %q, want one line", tc.assignee, line)
			}
			if got, want := HolderSeats(line), []string{NoteField(tc.assignee)}; !reflect.DeepEqual(got, want) {
				t.Errorf("HolderSeats(%q) = %v, want %v", line, got, want)
			}
		})
	}
}

// TestHolderSeatNoteLeavesTheReadyBlock pins that a holder line neither before
// nor after a READY TO LAND block changes what ParseReadyNote returns.
func TestHolderSeatNoteLeavesTheReadyBlock(t *testing.T) {
	t.Parallel()
	want := Work{
		Branch:    "polecat/capable/gt-e93lin",
		Head:      "0123456789abcdef0123456789abcdef01234567",
		Target:    "main",
		Worker:    "cheedo",
		Submitted: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC),
	}
	block := FormatReadyNote(want)
	holder := FormatHolderSeatNote("gastown/polecats/cheedo")
	for _, tc := range []struct{ name, notes string }{
		{"before the block", holder + "\n" + block},
		{"after the block", block + "\n" + holder},
		{"either side", holder + "\n" + block + "\n" + holder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseReadyNote(tc.notes)
			if !ok {
				t.Fatalf("ParseReadyNote(%q) found no block", tc.notes)
			}
			if got != want {
				t.Errorf("ParseReadyNote(%q) = %+v, want %+v", tc.notes, got, want)
			}
		})
	}
}
