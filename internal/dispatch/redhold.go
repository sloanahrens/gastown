package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// RedHoldFileName names the file the daemon publishes the red hold in.
const RedHoldFileName = "red-hold"

// RedHoldPath returns the red hold file for a town, under its daemon directory.
func RedHoldPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", RedHoldFileName)
}

// The red hold kinds, one per failure that parks automatic dispatch.
const (
	RedHoldSweepRed      = "sweep-red"
	RedHoldSweepNoResult = "sweep-no-result"
	RedHoldMainRed       = "main-red"
	RedHoldRollFailed    = "roll-failed"
	RedHoldImagesFailed  = "images-failed"
	RedHoldPairStuck     = "pair-stuck"
	RedHoldBehindMain    = "behind-main"
	RedHoldGreenStale    = "green-stale"
)

// RedHoldReason is one failure holding automatic dispatch, with the beads that cure it.
type RedHoldReason struct {
	Kind      string    `json:"kind"`
	Since     time.Time `json:"since"`
	Detail    string    `json:"detail,omitempty"`
	CureBeads []string  `json:"cure_beads,omitempty"`
}

// RedHoldRecord is every reason automatic dispatch is held right now.
type RedHoldRecord struct {
	Reasons []RedHoldReason `json:"reasons"`
}

// ReadRedHold reads the published record; a missing file is an empty record and
// no error, and a file that cannot be read or parsed as JSON is an error.
func ReadRedHold(townRoot string) (RedHoldRecord, error) {
	data, err := os.ReadFile(RedHoldPath(townRoot))
	if os.IsNotExist(err) {
		return RedHoldRecord{}, nil
	}
	if err != nil {
		return RedHoldRecord{}, err
	}
	var rec RedHoldRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return RedHoldRecord{}, err
	}
	return rec, nil
}

// WriteRedHold publishes rec atomically, or removes the file for an empty
// record; removing a missing file is not an error.
func WriteRedHold(townRoot string, rec RedHoldRecord) error {
	path := RedHoldPath(townRoot)
	if len(rec.Reasons) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return atomicfile.EnsureDirAndWriteJSON(path, rec)
}
