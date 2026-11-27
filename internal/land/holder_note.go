package land

import "strings"

// HolderSeatNoteMarker opens the line naming the polecat seat a submission
// claimed a bead from (gt-e93lin).
const HolderSeatNoteMarker = "HOLDER SEAT"

// FormatHolderSeatNote renders the line HolderSeats reads for assignee,
// collapsed onto one line like every other note field.
func FormatHolderSeatNote(assignee string) string {
	return HolderSeatNoteMarker + " " + NoteField(assignee)
}

// HolderSeats returns the seats the holder notes in notes name, in the order
// written and without duplicates; a line that names no seat names none.
func HolderSeats(notes string) []string {
	var seats []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(notes, "\n") {
		rest, ok := strings.CutPrefix(line, HolderSeatNoteMarker+" ")
		if !ok {
			continue
		}
		if seat := strings.TrimSpace(rest); seat != "" && !seen[seat] {
			seen[seat] = true
			seats = append(seats, seat)
		}
	}
	return seats
}
