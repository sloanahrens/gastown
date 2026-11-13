package land

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// localRoundMu serializes Allow's read-check-write in this process, so two
// concurrent calls for one bead cannot both read the same count and both pass.
var localRoundMu sync.Mutex

// localRoundsPerDay is how many local om rounds one bead may take in one local
// day. The third is refused so a polecat submits to the landing worker instead
// of spending rounds on its own machine.
const localRoundsPerDay = 2

// localRoundDayLayout names a calendar day in the clock's location.
const localRoundDayLayout = "2006-01-02"

// ErrLocalRoundBound reports that a bead has used its local rounds for today;
// the polecat submits the work with gt done.
var ErrLocalRoundBound = errors.New("local round bound: this bead has used its local rounds today; submit with gt done")

// Bound counts a bead's local om rounds per local day, kept as one JSON file per
// bead under Dir. It never reads or writes the review log, the verdict store,
// the rounds ledger or any rig counter.
type Bound struct {
	// Dir holds the per-bead state files; it is created on first use.
	Dir string
	// Now, when set, is the clock the day is taken from. Nil means time.Now.
	Now func() time.Time
	// FS, when set, is the filesystem the state is read and written through.
	// Nil means the host's, so a test drives a write failure with a fake.
	FS LedgerFS
}

// localRoundState is one bead's count for one local day, as stored.
type localRoundState struct {
	Bead  string `json:"bead"`
	Day   string `json:"day"`
	Count int    `json:"count"`
}

// Allow records one local round for bead and returns its number for the day,
// or ErrLocalRoundBound once the bead has used its rounds. A state file that
// cannot be read or parsed is an error naming the file, never a reset. Calls in
// one process are serialized; a second process sharing Dir is not covered by
// that lock.
func (b Bound) Allow(bead string) (int, error) {
	if bead == "" || bead == "." || bead == ".." || strings.ContainsAny(bead, `/\`) {
		return 0, fmt.Errorf("local round bound: invalid bead id %q", bead)
	}
	now := time.Now()
	if b.Now != nil {
		now = b.Now()
	}
	day := now.Format(localRoundDayLayout)
	path := filepath.Join(b.Dir, bead+".json")
	fsys := b.fs()

	localRoundMu.Lock()
	defer localRoundMu.Unlock()
	st, err := readLocalRoundState(fsys, path, bead)
	if err != nil {
		return 0, err
	}
	// Only a later day restarts the count. A stored day after today means the
	// clock was set back: the file's own day and count stand, so the bound holds.
	if st.Day < day {
		st = localRoundState{Bead: bead, Day: day}
	}
	if st.Count >= localRoundsPerDay {
		return 0, fmt.Errorf("bead %s: %w", bead, ErrLocalRoundBound)
	}
	st.Count++
	if err := writeLocalRoundState(fsys, b.Dir, path, st); err != nil {
		return 0, err
	}
	return st.Count, nil
}

// fs is the filesystem the state uses.
func (b Bound) fs() LedgerFS {
	if b.FS != nil {
		return b.FS
	}
	return osLedgerFS{}
}

// readLocalRoundState returns the stored count for bead, or a zero state when
// no file exists yet. Any other read or parse failure is an error naming path.
func readLocalRoundState(fsys LedgerFS, path, bead string) (localRoundState, error) {
	body, err := fsys.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return localRoundState{Bead: bead}, nil
	}
	if err != nil {
		return localRoundState{}, fmt.Errorf("local round state %s is unreadable: %w", path, err)
	}
	var st localRoundState
	if err := json.Unmarshal(body, &st); err != nil {
		return localRoundState{}, fmt.Errorf("local round state %s is corrupt: %w", path, err)
	}
	if st.Bead != bead || st.Count < 0 {
		return localRoundState{}, fmt.Errorf("local round state %s is corrupt: bead %q count %d", path, st.Bead, st.Count)
	}
	if _, err := time.Parse(localRoundDayLayout, st.Day); err != nil {
		return localRoundState{}, fmt.Errorf("local round state %s is corrupt: day %q: %w", path, st.Day, err)
	}
	return st, nil
}

// writeLocalRoundState replaces path with st by writing a temp file beside it
// and renaming it over path, so a reader sees either the old count or the new
// one, never a partial file.
func writeLocalRoundState(fsys LedgerFS, dir, path string, st localRoundState) error {
	if err := fsys.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating local round dir %s: %w", dir, err)
	}
	// json cannot fail on localRoundState, which holds two strings and an int.
	body, _ := json.Marshal(st)
	body = append(body, '\n')
	tmp := path + ".tmp"
	if err := fsys.WriteFile(tmp, body, 0o600); err != nil {
		// A failed write can leave a partial temp file; remove it so it is never renamed later.
		_ = fsys.Remove(tmp)
		return fmt.Errorf("writing local round state %s: %w", tmp, err)
	}
	if err := fsys.Rename(tmp, path); err != nil {
		_ = fsys.Remove(tmp)
		return fmt.Errorf("replacing local round state %s: %w", path, err)
	}
	return nil
}
