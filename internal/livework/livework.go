// Package livework tells whether a bead a seat hooks is live work.
package livework

import "fmt"

// Check reports whether id names live work: a bead that is open, hooked or
// in_progress. Every other status is not live work. A show that fails or finds
// no bead returns an error, never (false, nil), so a caller cannot mistake an
// unreadable bead for an idle seat.
func Check(id string, show func(id string) (status string, found bool, err error)) (bool, error) {
	status, found, err := show(id)
	if err != nil {
		return false, fmt.Errorf("cannot read the hooked bead %s: %w", id, err)
	}
	if !found {
		return false, fmt.Errorf("the hooked bead %s reads as no bead", id)
	}
	switch status {
	case "open", "hooked", "in_progress":
		return true, nil
	}
	return false, nil
}
