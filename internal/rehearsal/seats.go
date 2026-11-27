package rehearsal

import (
	"encoding/json"
	"fmt"

	"github.com/steveyegge/gastown/internal/polecat"
)

// seat is the part of `gt polecat list <rig> --json` (cmd.PolecatListItem)
// that decides whether a seat is stranded.
type seat struct {
	Rig            string `json:"rig"`
	Name           string `json:"name"`
	State          string `json:"state"`
	Issue          string `json:"issue"`
	SessionRunning bool   `json:"session_running"`
}

// Stranded names the seats of rig that still hold a bead or have no running
// session while not idle, in listing order. A seat holding a bead is named
// whether or not its session is live; an idle seat with no bead and no session
// is clean. JSON that does not decode is an error, never an empty list.
func Stranded(listing []byte, rig string) ([]string, error) {
	var seats []seat
	if err := json.Unmarshal(listing, &seats); err != nil {
		return nil, fmt.Errorf("decoding polecat listing: %w", err)
	}
	var stranded []string
	for _, s := range seats {
		if s.Rig == rig && (s.Issue != "" || (!s.SessionRunning && s.State != string(polecat.StateIdle))) {
			stranded = append(stranded, s.Name)
		}
	}
	return stranded, nil
}
