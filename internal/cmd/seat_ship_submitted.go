package cmd

import (
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// seatSubmitted reports whether bead carries the gt:ready-to-land label gt
// done sets (land.LabelReadyToLand). A bead that cannot be read is an error.
func seatSubmitted(c beads.Client, bead string) (bool, error) {
	issue, err := c.Show(bead)
	if err != nil {
		return false, err
	}
	return beads.HasLabel(issue, land.LabelReadyToLand), nil
}
