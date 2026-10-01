package land

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func sampleWork() Work {
	return Work{BeadID: "gt-abc", Rig: "gastown", Branch: "polecat/opal/gt-abc+x1", Head: "0123456789abcdef0123456789abcdef01234567", Target: "main", Worker: "opal"}
}

func TestReadyNoteRoundTrip(t *testing.T) {
	t.Parallel()
	w := sampleWork()
	got, ok := ParseReadyNote("earlier notes\n" + FormatReadyNote(w))
	if !ok {
		t.Fatal("ParseReadyNote found no block")
	}
	w.BeadID, w.Rig = "", "" // not carried by the note
	if got != w {
		t.Fatalf("round trip = %+v, want %+v", got, w)
	}
}

func TestReadyNoteLastBlockWins(t *testing.T) {
	t.Parallel()
	old := sampleWork()
	old.Head = strings.Repeat("a", 40)
	cur := sampleWork()
	got, ok := ParseReadyNote(FormatReadyNote(old) + "\n" + FormatReadyNote(cur))
	if !ok || got.Head != cur.Head {
		t.Fatalf("got %+v ok=%v, want head %s", got, ok, cur.Head)
	}
}

func TestReadyNoteFieldsCannotForgeLines(t *testing.T) {
	t.Parallel()
	w := sampleWork()
	w.Worker = "opal\nHead: " + strings.Repeat("f", 40)
	got, ok := ParseReadyNote(FormatReadyNote(w))
	if !ok || got.Head != sampleWork().Head {
		t.Fatalf("forged head leaked: %+v", got)
	}
}

func TestReadyNoteFieldsCannotForgeASubmissionTime(t *testing.T) {
	t.Parallel()
	w := sampleWork()
	w.Submitted = time.Date(2026, 10, 1, 13, 45, 0, 0, time.UTC)
	w.Worker = "opal\nSubmitted: 1999-01-01T00:00:00Z"
	got, ok := ParseReadyNote(FormatReadyNote(w))
	if !ok || !got.Submitted.Equal(w.Submitted) {
		t.Fatalf("forged submitted leaked: %+v", got)
	}
}

// The landing worker orders the queue by the submission time; a block that
// carries none is a bead submitted before the field existed.
func TestReadyNoteRoundTripsTheSubmissionTime(t *testing.T) {
	t.Parallel()
	w := sampleWork()
	w.Submitted = time.Date(2026, 10, 1, 13, 45, 0, 0, time.UTC)
	got, ok := ParseReadyNote("earlier notes\n" + FormatReadyNote(w))
	if !ok {
		t.Fatal("ParseReadyNote found no block")
	}
	if !got.Submitted.Equal(w.Submitted) {
		t.Fatalf("Submitted = %v, want %v", got.Submitted, w.Submitted)
	}
	if plain, ok := ParseReadyNote(FormatReadyNote(sampleWork())); !ok || !plain.Submitted.IsZero() {
		t.Fatalf("a block with no Submitted = %v (ok=%v), want the zero time", plain.Submitted, ok)
	}
}

// A time this code cannot read orders as if the block carried none: the work
// still lands.
func TestReadyNoteIgnoresAnUnreadableSubmissionTime(t *testing.T) {
	t.Parallel()
	block := strings.Replace(FormatReadyNote(sampleWork()), "\nWorker: opal", "\nWorker: opal\nSubmitted: yesterday", 1)
	got, ok := ParseReadyNote(block)
	if !ok || got.Branch != sampleWork().Branch || !got.Submitted.IsZero() {
		t.Fatalf("ParseReadyNote = %+v ok=%v; want the block read with no time", got, ok)
	}
}

func TestWorkFromBeadNeedsLabelAndBlock(t *testing.T) {
	t.Parallel()
	note := FormatReadyNote(sampleWork())
	if _, err := WorkFromBead(&beads.Issue{ID: "gt-abc", Notes: note}, "gastown"); err == nil {
		t.Error("bead without the ready label accepted")
	}
	if _, err := WorkFromBead(&beads.Issue{ID: "gt-abc", Labels: []string{LabelReadyToLand}}, "gastown"); err == nil {
		t.Error("bead without a READY TO LAND block accepted")
	}
	w, err := WorkFromBead(&beads.Issue{ID: "gt-abc", Labels: []string{LabelReadyToLand}, Notes: note}, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if w != sampleWork() {
		t.Fatalf("WorkFromBead = %+v, want %+v", w, sampleWork())
	}
}
