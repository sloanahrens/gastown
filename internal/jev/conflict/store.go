package conflict

import (
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// RiskAnswer is the answer to the conflict rubric: the level, 0 for no overlap
// and 3 for the same function, and Jev's confidence in it.
type RiskAnswer struct {
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
}

// Answers is what Jev said about one pair. Each noul is the probability that
// its statement holds, 0 to 1.
type Answers struct {
	SameFiles    float64    `json:"same_files"`
	SameFunction float64    `json:"same_function"`
	Risk         RiskAnswer `json:"risk"`
}

// Line is one line of the conflict log: what Jev predicted for one pair of
// in-flight beads. It holds bead IDs, placement stamps and numbers only, never
// a title, a description, a prompt or key text.
type Line struct {
	// TS is when the answer was logged.
	TS time.Time `json:"ts"`
	// Bead is the bead placed later, the one whose placement brought the pair
	// together.
	Bead string `json:"bead"`
	// AttachedAt is Bead's attached_at stamp, which marks its placement.
	AttachedAt time.Time `json:"attached_at"`
	// Other is the bead that was in flight when Bead was placed.
	Other string `json:"other"`
	// OtherAttachedAt is Other's attached_at stamp.
	OtherAttachedAt time.Time `json:"other_attached_at"`
	// Answers is the battery's answers for the pair.
	Answers Answers `json:"answers"`
	// QuestionVersion is Version when the line was asked.
	QuestionVersion int `json:"question_version"`
	// Model is the versioned model ID the API says answered.
	Model string `json:"model"`
}

// LogPath is the conflict log, one JSON line per pair asked.
func LogPath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "jev", "conflict.jsonl")
}

// CursorPath is the file that remembers the last placement a pass asked about.
func CursorPath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "jev", "conflict.cursor.json")
}

// PairsPath is the file that remembers which pairs a pass has asked about, and
// how many passes have failed to answer each one that has not been answered.
func PairsPath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "jev", "conflict.pairs.json")
}

// ErrBusy means another pass holds the run lock.
var ErrBusy = errors.New("another jev conflict pass is running")

// lockRun takes the pass's run lock without waiting, and returns the release.
// The lock sits beside the cursor, so every pass over one cursor contends on it.
func lockRun(cursorPath string) (func(), error) {
	release, err := battery.Lock(cursorPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}

// pairs is the saved state of the pass: every pair it is done with, keyed by
// pairKey and mapped to the later of the two placement stamps, and the failed
// attempts for the pairs it has not yet got an answer for.
type pairs struct {
	Done     map[string]time.Time `json:"done"`
	Attempts map[string]int       `json:"attempts,omitempty"`
}

// readPairs returns the saved state at path. A missing file is an empty state.
func readPairs(path string) (pairs, error) {
	var st pairs
	if _, err := battery.ReadState(path, &st); err != nil {
		return pairs{}, err
	}
	if st.Done == nil {
		st.Done = map[string]time.Time{}
	}
	if st.Attempts == nil {
		st.Attempts = map[string]int{}
	}
	return st, nil
}

// prune drops every pair whose later stamp is before t. A pair is only ever
// formed with a placement at or after the cursor, so a pair whose later stamp
// is before the cursor can never come up again. It reports whether it dropped
// anything, so the caller writes the state only when it changed.
func (st pairs) prune(t time.Time) bool {
	dropped := false
	for key, at := range st.Done {
		if at.Before(t) {
			delete(st.Done, key)
			delete(st.Attempts, key)
			dropped = true
		}
	}
	return dropped
}

// pairKey names one pair of placements. Both placements are in the key, so a
// bead slung again is a new pair with no answer behind it.
func pairKey(a, b placement) string {
	if b.less(a) {
		a, b = b, a
	}
	return a.key() + "|" + b.key()
}

// newer returns the later of the two placement stamps, the time a pair is
// pruned by.
func newer(a, b placement) time.Time {
	if a.at.After(b.at) {
		return a.at
	}
	return b.at
}

// beadFault reports whether err blames the bead's request rather than the
// pass's reach: Jev refused this request, or accepted it and answered
// unusably. A refusal of the credential (401) or of the account (402 and 403)
// is about the pass's reach, not the beads. Only a bead fault counts an
// attempt against the pair; any other failure stops the pass where it is, so
// an outage costs no attempts.
func beadFault(err error) bool {
	var se *jev.StatusError
	if errors.As(err, &se) {
		return se.Status >= 400 && se.Status < 500 &&
			se.Status != http.StatusUnauthorized &&
			!accountRefusal(se.Status)
	}
	return errors.Is(err, jev.ErrBadResponse)
}

// accountRefusal reports whether a 4xx answers for the account rather than for
// this request: 402 Payment Required and 403 Forbidden refuse, or would refuse,
// every request the key makes, so a pair that fails under them says nothing
// about the beads. Counting such a failure would spend the pair's maxAttempts
// on an account or billing problem and give the pair up for good; holding the
// cursor instead asks it again once the account can answer.
func accountRefusal(status int) bool {
	return status == http.StatusPaymentRequired || status == http.StatusForbidden
}
