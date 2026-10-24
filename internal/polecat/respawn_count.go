package polecat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/workspace"
)

// respawnMu serializes in-process access to the respawn state file.
// Cross-process serialization is handled by lock.FlockAcquire on a
// sibling .flock file (see RecordBeadRespawn, ShouldBlockRespawn, etc.).
var respawnMu sync.Mutex

// beadRespawnRecord tracks how many times a single bead has been reset for re-dispatch.
//
// Since is when the current run of counts began. A reset deletes the record, so
// a later increment starts a new run and GiveBackBeadRespawn can tell them apart.
type beadRespawnRecord struct {
	BeadID      string    `json:"bead_id"`
	Count       int       `json:"count"`
	LastRespawn time.Time `json:"last_respawn"`
	Since       time.Time `json:"since"`
}

// RespawnMark names one increment from RecordBeadRespawnMark; the zero mark
// names none.
type RespawnMark struct {
	Count int
	Since time.Time
}

// beadRespawnState holds respawn counts for all tracked beads.
type beadRespawnState struct {
	Beads       map[string]*beadRespawnRecord `json:"beads"`
	LastUpdated time.Time                     `json:"last_updated"`
}

// beadRespawnStateFile is the live location of the respawn counts, in
// <town>/.runtime/ rather than the retired witness directory (gt-4k3fj.6.1).
// The witness/ copy is migrated here on first use (gt-la690).
func beadRespawnStateFile(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "bead-respawn-counts.json")
}

// legacyBeadRespawnStateFile is where the counts sat while the witness role
// owned them. The file is live state in towns built before the move, and
// discarding it would hand every bead its full respawn budget back at once, so
// it is read and merged into beadRespawnStateFile on first use (gt-la690).
func legacyBeadRespawnStateFile(townRoot string) string {
	return filepath.Join(townRoot, "witness", "bead-respawn-counts.json")
}

func emptyBeadRespawnState() *beadRespawnState {
	return &beadRespawnState{Beads: make(map[string]*beadRespawnRecord)}
}

// corruptRespawnStateError is a state file that was read but does not parse, as
// against one that could not be read: only the first is a file `gt sling
// respawn-reset` may move aside (gt-jw3ed).
type corruptRespawnStateError struct {
	path string
	err  error
}

func (e *corruptRespawnStateError) Error() string {
	return fmt.Sprintf("parsing respawn state %s: %v", e.path, e.err)
}

func (e *corruptRespawnStateError) Unwrap() error { return e.err }

// readBeadRespawnStateFile returns the state in path, or nil when the file is
// not there. A file that cannot be read or parsed is an error, an empty state
// being what re-arms every bead's respawn budget at once — the town-wide
// breaker wipe a truncated file used to cause (gt-u3hc1).
func readBeadRespawnStateFile(path string) (*beadRespawnState, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from trusted townRoot
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading respawn state %s: %w", path, err)
	}
	var state beadRespawnState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, &corruptRespawnStateError{path: path, err: err}
	}
	if state.Beads == nil {
		state.Beads = make(map[string]*beadRespawnRecord)
	}
	return &state, nil
}

// loadBeadRespawnState reads the tracked respawn counts, folding in the legacy
// witness/ file while it is still there: a bead must never read as never
// respawned just because the read happened before the migration's first write.
// A missing file is an empty state; one that cannot be read or parsed is an
// error (gt-u3hc1). A legacy file that does not parse names the command that
// moves it aside, the only way out short of deleting it by hand (gt-jw3ed).
func loadBeadRespawnState(townRoot string) (*beadRespawnState, error) {
	state, err := readLiveBeadRespawnState(townRoot)
	if err != nil {
		return nil, err
	}
	legacy, err := readBeadRespawnStateFile(legacyBeadRespawnStateFile(townRoot))
	if err != nil {
		if corruptLegacyRespawnState(townRoot, err) != nil {
			return nil, fmt.Errorf("%w; `gt sling respawn-reset <bead-id>` moves it aside", err)
		}
		return nil, err
	}
	mergeBeadRespawnStates(state, legacy)
	return state, nil
}

// readLiveBeadRespawnState reads bead-respawn-counts.json alone, the file every
// writer starts from. A missing file is an empty state; one that cannot be read
// or parsed is an error (gt-u3hc1).
func readLiveBeadRespawnState(townRoot string) (*beadRespawnState, error) {
	state, err := readBeadRespawnStateFile(beadRespawnStateFile(townRoot))
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = emptyBeadRespawnState()
	}
	return state, nil
}

// corruptLegacyRespawnState returns err's corrupt-file error when it is the
// legacy witness/ file that failed to parse, nil for anything else.
func corruptLegacyRespawnState(townRoot string, err error) *corruptRespawnStateError {
	var corrupt *corruptRespawnStateError
	if errors.As(err, &corrupt) && corrupt.path == legacyBeadRespawnStateFile(townRoot) {
		return corrupt
	}
	return nil
}

// quarantineLegacyRespawnState renames the unparseable legacy file aside, next
// to itself, and returns where it went. The file is kept rather than deleted:
// its counts are unreadable but an operator may still recover them by hand.
// A file already removed by someone else returns "", nil: the blocker this
// clears is gone, and failing here would keep the reset down for a file the
// operator already dealt with (gt-601lm).
func quarantineLegacyRespawnState(townRoot string) (string, error) {
	legacy := legacyBeadRespawnStateFile(townRoot)
	aside := fmt.Sprintf("%s.corrupt-%s", legacy, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(legacy, aside); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("moving corrupt respawn state aside: %w", err)
	}
	return aside, nil
}

// mergeBeadRespawnStates folds src into dst per bead, keeping the larger count
// and the later last_respawn: both files hold real increments, and a merge that
// preferred either side outright would drop the other's.
func mergeBeadRespawnStates(dst, src *beadRespawnState) {
	if src == nil {
		return
	}
	for id, rec := range src.Beads {
		if rec == nil {
			continue
		}
		existing, ok := dst.Beads[id]
		if !ok {
			copied := *rec
			dst.Beads[id] = &copied
			continue
		}
		if rec.Count > existing.Count {
			existing.Count = rec.Count
		}
		if rec.LastRespawn.After(existing.LastRespawn) {
			existing.LastRespawn = rec.LastRespawn
		}
	}
}

// saveBeadRespawnState writes state beside its destination and renames it into
// place, so a failed or partial write leaves the previous counts readable —
// where truncating the file in place leaves the unparseable one that reads as
// empty (gt-u3hc1). Once the rename lands the legacy witness/ copy is cleared
// (gt-la690).
func saveBeadRespawnState(townRoot string, state *beadRespawnState) error {
	stateFile := beadRespawnStateFile(townRoot)
	dir := filepath.Dir(stateFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating runtime dir: %w", err)
	}
	state.LastUpdated = time.Now().UTC()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling respawn state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".bead-respawn-counts-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp respawn state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // already gone once the rename lands
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing respawn state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing respawn state: %w", err)
	}
	if err := os.Rename(tmpName, stateFile); err != nil {
		return fmt.Errorf("replacing respawn state: %w", err)
	}
	removeLegacyBeadRespawnState(townRoot)
	return nil
}

// removeLegacyBeadRespawnState clears the retired witness/ copy and its flock.
// Its failures are dropped rather than returned: the merged counts are already
// on disk, and a respawn that goes unrecorded because a stale file would not
// delete is the spawn storm this counter exists to stop. The directory removal
// fails whenever the operator's witness/ still holds anything else, which is
// the point of trying it here rather than by hand (gt-la690).
func removeLegacyBeadRespawnState(townRoot string) {
	legacy := legacyBeadRespawnStateFile(townRoot)
	_ = os.Remove(legacy)
	_ = os.Remove(legacy + ".flock")
	_ = os.Remove(filepath.Dir(legacy))
}

// respawnTownRoot resolves the town a rig path belongs to, falling back to the
// path itself outside a workspace.
func respawnTownRoot(workDir string) string {
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		return workDir
	}
	return townRoot
}

// lockRespawnState takes the cross-process lock guarding the respawn state.
// The flock file sits beside the state file and flockAcquire creates no parent
// directories, so the state dir is created first: a first run in a fresh town
// used to fail the open and fall through to an unlocked read-modify-write.
//
// A lock that cannot be taken is an error and never a silent fall-through —
// every caller below is a read-modify-write that is only safe while holding it
// (gt-u3hc1).
func lockRespawnState(townRoot string) (func(), error) {
	stateFile := beadRespawnStateFile(townRoot)
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		return nil, fmt.Errorf("creating runtime dir: %w", err)
	}
	unlock, err := lock.FlockAcquire(stateFile + ".flock")
	if err != nil {
		return nil, fmt.Errorf("locking respawn state: %w", err)
	}
	return unlock, nil
}

// ShouldBlockRespawn returns true if the bead has already been respawned
// MaxBeadRespawns times (from operational config). When true, the caller
// should stop and escalate instead of sending RECOVERED_BEAD to deacon
// for re-dispatch. This is the primary circuit breaker for spawn storms
// (clown show #22).
//
// State that cannot be read or locked blocks, and says so on stderr: the counts
// are the only record that this bead is near its limit, and reading "could not
// tell" as "never respawned" is what let a truncated file re-arm the breaker
// town-wide (gt-u3hc1).
func ShouldBlockRespawn(workDir, beadID string) bool {
	respawnMu.Lock()
	defer respawnMu.Unlock()

	townRoot := respawnTownRoot(workDir)
	maxRespawns := config.LoadOperationalConfig(townRoot).GetRecoveryConfig().MaxBeadRespawnsV()

	unlock, err := lockRespawnState(townRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v; blocking respawn of %s\n", err, beadID)
		return true
	}
	defer unlock()

	state, err := loadBeadRespawnState(townRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v; blocking respawn of %s\n", err, beadID)
		return true
	}
	rec, ok := state.Beads[beadID]
	if !ok {
		return false
	}
	return rec.Count >= maxRespawns
}

// RecordBeadRespawn increments the respawn count for beadID and returns the new
// count, or an error with nothing recorded. workDir is the rig path; townRoot
// is resolved internally via workspace.Find.
//
// Serialized via respawnMu (in-process) and flock (cross-process) to prevent
// concurrent patrol cycles from racing on the load-modify-save cycle. A count
// that cannot be recorded is an error rather than a silent skip: the caller's
// respawn would otherwise proceed uncounted, which is the spawn storm this
// counter exists to stop (gt-u3hc1).
func RecordBeadRespawn(workDir, beadID string) (int, error) {
	mark, err := RecordBeadRespawnMark(workDir, beadID)
	return mark.Count, err
}

// RecordBeadRespawnMark is RecordBeadRespawn for a caller that may give the
// increment back: it returns a mark that names this increment.
func RecordBeadRespawnMark(workDir, beadID string) (RespawnMark, error) {
	respawnMu.Lock()
	defer respawnMu.Unlock()

	townRoot := respawnTownRoot(workDir)

	unlock, err := lockRespawnState(townRoot)
	if err != nil {
		return RespawnMark{}, err
	}
	defer unlock()

	state, err := loadBeadRespawnState(townRoot)
	if err != nil {
		return RespawnMark{}, err
	}
	now := time.Now().UTC()
	rec, ok := state.Beads[beadID]
	if !ok {
		rec = &beadRespawnRecord{BeadID: beadID, Since: now}
		state.Beads[beadID] = rec
	}
	rec.Count++
	rec.LastRespawn = now
	if err := saveBeadRespawnState(townRoot, state); err != nil {
		return RespawnMark{}, err
	}
	return RespawnMark{Count: rec.Count, Since: rec.Since}, nil
}

// GiveBackBeadRespawn removes the increment one sling attempt recorded, when
// that attempt started no polecat. mark is what RecordBeadRespawnMark returned
// for the attempt; the zero mark records nothing, which gives nothing back
// rather than charging another attempt's death to this one.
//
// A record that is gone, or began after the mark, was reset since the attempt
// and already lost the increment, so nothing else comes off. Otherwise one
// increment comes off and the other attempts' increments in the run stay. The
// sling gives back an image-drift refusal, which is the seat's precondition
// rather than a death of the bead (gt-cjmt5).
func GiveBackBeadRespawn(workDir, beadID string, mark RespawnMark) error {
	if mark.Count <= 0 {
		return nil
	}

	respawnMu.Lock()
	defer respawnMu.Unlock()

	townRoot := respawnTownRoot(workDir)

	unlock, err := lockRespawnState(townRoot)
	if err != nil {
		return err
	}
	defer unlock()

	state, err := loadBeadRespawnState(townRoot)
	if err != nil {
		return err
	}
	rec, ok := state.Beads[beadID]
	if !ok || !rec.Since.Equal(mark.Since) {
		return nil
	}
	rec.Count--
	if rec.Count <= 0 {
		delete(state.Beads, beadID)
	}
	return saveBeadRespawnState(townRoot, state)
}

// ResetBeadRespawnCount resets the respawn counter for beadID to zero.
// Used by `gt sling respawn-reset` to allow re-dispatch after investigation.
//
// It is the one path that moves a corrupt legacy witness/ file aside and goes
// on: every other caller refuses on it, which blocks every bead until the file
// is gone, so an operator who runs the reset has chosen to lose its counts.
// A corrupt file at the live location is still refused (gt-jw3ed).
func ResetBeadRespawnCount(workDir, beadID string) error {
	respawnMu.Lock()
	defer respawnMu.Unlock()

	townRoot := respawnTownRoot(workDir)

	unlock, err := lockRespawnState(townRoot)
	if err != nil {
		return err
	}
	defer unlock()

	// The live state is read once, before the quarantine: afterwards the legacy
	// file is gone, so a second load can only report a failure the first one
	// already ruled out, and its error would arrive without the aside location.
	state, err := readLiveBeadRespawnState(townRoot)
	if err != nil {
		return err
	}
	legacy, err := readBeadRespawnStateFile(legacyBeadRespawnStateFile(townRoot))
	if err != nil {
		corrupt := corruptLegacyRespawnState(townRoot, err)
		if corrupt == nil {
			return err
		}
		aside, qerr := quarantineLegacyRespawnState(townRoot)
		if qerr != nil {
			return fmt.Errorf("%w; %v", err, qerr)
		}
		if aside != "" {
			fmt.Fprintf(os.Stderr, "Warning: %v; moved it to %s\n", corrupt, aside)
		}
	} else {
		mergeBeadRespawnStates(state, legacy)
	}
	delete(state.Beads, beadID)
	return saveBeadRespawnState(townRoot, state)
}

// ResetBeadRespawnForRound clears beadID's respawn count when the last spawn it
// records came before submitted, and reports whether it cleared it.
//
// Only a spawn the bead never submitted after is a death, and only deaths count
// toward the limit. A spawn recorded before the newest submission is a round of
// work instead: the polecat started, submitted, and the landing worker sent the
// attempt back. Counting those spent a live bead's budget on work, and three
// legitimate rework rounds put the fourth sling at the limit for good
// (gt-tatcn).
//
// submitted is the bead's newest submission for landing, zero when none is
// readable. Zero clears nothing, and neither does a submission older than the
// last recorded spawn: a spawn since the submission is the death this counter
// exists to stop. That is also why the bead's rework label is not consulted —
// it records the refusal but not when it happened, so it cannot say which
// spawns came before the refusal, and a rework bead whose polecat dies at start
// must still reach the limit.
//
// workDir is the rig path; townRoot is resolved internally via workspace.Find.
func ResetBeadRespawnForRound(workDir, beadID string, submitted time.Time) (bool, error) {
	if submitted.IsZero() {
		return false, nil
	}
	respawnMu.Lock()
	defer respawnMu.Unlock()

	townRoot := respawnTownRoot(workDir)

	unlock, err := lockRespawnState(townRoot)
	if err != nil {
		return false, err
	}
	defer unlock()

	state, err := loadBeadRespawnState(townRoot)
	if err != nil {
		return false, err
	}
	rec, ok := state.Beads[beadID]
	if !ok {
		return false, nil
	}
	if !submitted.After(rec.LastRespawn) {
		return false, nil
	}
	delete(state.Beads, beadID)
	if err := saveBeadRespawnState(townRoot, state); err != nil {
		return false, err
	}
	return true, nil
}
