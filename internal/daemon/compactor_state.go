package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// The commit-count advisory reaches a reader through no beads record, because
// it raises no escalation: a cycle writes what it found here, and `gt status`
// renders it on the Services line beside the commits/24h advisory — shown,
// and no more (gt-3mu6q).

const compactorStateFileName = "compactor_state.json"

// CompactorAdvisoryStale is how old a recorded advisory may be before readers
// treat it as gone: two 24h runs, so a record survives a missed cycle or a
// daemon restart. Past it the record is a compactor that stopped running, and
// a stale advisory on the status line would be a claim nothing re-checks.
const CompactorAdvisoryStale = 2 * defaultCompactorDogInterval

// CompactorAdvisory is one database whose commit count is over its line.
type CompactorAdvisory struct {
	Database  string `json:"database"`
	Commits   int    `json:"commits"`
	Threshold int    `json:"threshold"`
}

// compactorAdvisoryState is the file's shape: when the cycle measured, and
// what it found. The list is empty for a cycle that found every database
// under its line, so the record says "measured, nothing over" rather than
// leaving the previous cycle's finding to be read as current.
type compactorAdvisoryState struct {
	At         time.Time           `json:"at"`
	Advisories []CompactorAdvisory `json:"advisories"`
}

// compactorStatePath is where a cycle records the advisory gt status reads.
func compactorStatePath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", compactorStateFileName)
}

// saveCompactorAdvisories records the databases a cycle found over the commit
// line, replacing the previous cycle's record.
func saveCompactorAdvisories(townRoot string, at time.Time, advisories []compactorAdvisory) error {
	records := make([]CompactorAdvisory, 0, len(advisories))
	for _, a := range advisories {
		records = append(records, CompactorAdvisory{Database: a.db, Commits: a.commits, Threshold: a.threshold})
	}
	return atomicfile.EnsureDirAndWriteJSON(compactorStatePath(townRoot),
		compactorAdvisoryState{At: at.UTC(), Advisories: records})
}

// LoadCompactorAdvisories returns the databases the last compactor cycle found
// over their commit line: nil when it found none, and nil when the record is
// missing, unreadable or older than CompactorAdvisoryStale.
func LoadCompactorAdvisories(townRoot string, now time.Time) []CompactorAdvisory {
	data, err := os.ReadFile(compactorStatePath(townRoot))
	if err != nil {
		return nil
	}
	var state compactorAdvisoryState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil
	}
	if state.At.IsZero() || now.Sub(state.At) >= CompactorAdvisoryStale {
		return nil
	}
	return state.Advisories
}
