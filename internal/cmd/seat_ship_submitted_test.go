package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

func TestSeatShipSubmitted(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	if _, err := db.Create(beads.CreateOptions{ID: "gt-seat", Labels: []string{"gt:ready-to-land"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Create(beads.CreateOptions{ID: "gt-plain", Labels: []string{"sp:8"}}); err != nil {
		t.Fatal(err)
	}
	t.Run("ready to land", func(t *testing.T) {
		if got, err := seatSubmitted(db, "gt-seat"); err != nil || !got {
			t.Errorf("seatSubmitted(labelled) = %v, %v; want true, nil", got, err)
		}
	})
	t.Run("unlabelled", func(t *testing.T) {
		if got, err := seatSubmitted(db, "gt-plain"); err != nil || got {
			t.Errorf("seatSubmitted(unlabelled) = %v, %v; want false, nil", got, err)
		}
	})
	t.Run("missing bead", func(t *testing.T) {
		if _, err := seatSubmitted(db, "gt-missing"); err == nil {
			t.Error("seatSubmitted(missing bead) returned no error")
		}
	})
}
