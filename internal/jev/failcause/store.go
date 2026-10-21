package failcause

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// Answer is Jev's answer to one question about one failing test. A noul sets
// Noul; a choice sets Choice and Probabilities.
type Answer struct {
	// Noul is the probability, 0 to 1, that the question's statement holds.
	Noul *float64 `json:"noul,omitempty"`
	// Choice is the option Jev picked.
	Choice string `json:"choice,omitempty"`
	// Probabilities maps every option of the choice to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Line is one line of the cause log: what Jev answered about one Go test that
// failed on a candidate gate. It holds IDs, times, answers and the model, never
// the excerpt the answers were judged from.
type Line struct {
	// TS is when the judging began, which is when the window for will_recur
	// starts.
	TS time.Time `json:"ts"`
	// Rig and Bead name the landing whose gate failed on the test.
	Rig  string `json:"rig"`
	Bead string `json:"bead"`
	// Package and Test name the failing test.
	Package string `json:"package"`
	Test    string `json:"test"`
	// Answers maps each question id to its answer.
	Answers map[string]Answer `json:"answers"`
	// Model is the versioned Jev model that answered.
	Model string `json:"model"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
}

// LogPath is the cause log, one JSON line per failing test.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "failcause.jsonl")
}

// WillRecurWindow is how long after a failure a repeat on another landing
// counts as the test failing again. The scorecard labels will_recur from it.
const WillRecurWindow = 14 * 24 * time.Hour

// CIFailureRetention is how long the CI-failure ledger keeps a record. The
// ledger rewrites itself on each write and drops anything older, so a
// retention shorter than WillRecurWindow means a later write can remove a
// repeat the window would have counted. The scorecard's coverage check depends
// on this value. The daemon's writer uses the same constant; a test in the
// daemon package holds the two together.
const CIFailureRetention = 7 * 24 * time.Hour

// CIFailureLedgerPath is the town's CI-failure ledger, one JSON line per
// failing test of each red candidate verdict. The daemon's landing worker
// writes it.
func CIFailureLedgerPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "ci-failures.jsonl")
}

// CIRecord is one line of the CI-failure ledger, as the daemon writes it: one
// failing (package, test) of one bead's candidate, or an unparsed failure that
// named no test.
type CIRecord struct {
	Time     time.Time `json:"time"`
	Rig      string    `json:"rig"`
	Bead     string    `json:"bead"`
	Package  string    `json:"package,omitempty"`
	Test     string    `json:"test,omitempty"`
	Unparsed bool      `json:"unparsed,omitempty"`
}

// CILedger is the CI-failure ledger as it stands on disk.
type CILedger struct {
	Records []CIRecord
	// Modified is the time of the ledger's last write. Every write rewrites the
	// file, so this is the last time the ledger pruned its old records.
	Modified time.Time
}

// ReadCILedger reads the ledger at path. A missing ledger is reported with
// ok false, not as an error, so a caller can tell an empty ledger from one
// nothing has written yet. Lines that do not parse are skipped, as the daemon
// skips them.
func ReadCILedger(path string) (ledger CILedger, ok bool, err error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return CILedger{}, false, nil
	}
	if err != nil {
		return CILedger{}, false, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: town runtime path
	if err != nil {
		return CILedger{}, false, err
	}
	ledger.Modified = info.ModTime()
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r CIRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		ledger.Records = append(ledger.Records, r)
	}
	return ledger, true, nil
}
