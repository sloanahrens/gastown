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
type beadRespawnRecord struct {
	BeadID      string    `json:"bead_id"`
	Count       int       `json:"count"`
	LastRespawn time.Time `json:"last_respawn"`
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
		return nil, fmt.Errorf("parsing respawn state %s: %w", path, err)
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
// error (gt-u3hc1).
func loadBeadRespawnState(townRoot string) (*beadRespawnState, error) {
	state, err := readBeadRespawnStateFile(beadRespawnStateFile(townRoot))
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = emptyBeadRespawnState()
	}
	legacy, err := readBeadRespawnStateFile(legacyBeadRespawnStateFile(townRoot))
	if err != nil {
		return nil, err
	}
	mergeBeadRespawnStates(state, legacy)
	return state, nil
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
	respawnMu.Lock()
	defer respawnMu.Unlock()

	townRoot := respawnTownRoot(workDir)

	unlock, err := lockRespawnState(townRoot)
	if err != nil {
		return 0, err
	}
	defer unlock()

	state, err := loadBeadRespawnState(townRoot)
	if err != nil {
		return 0, err
	}
	rec, ok := state.Beads[beadID]
	if !ok {
		rec = &beadRespawnRecord{BeadID: beadID}
		state.Beads[beadID] = rec
	}
	rec.Count++
	rec.LastRespawn = time.Now().UTC()
	if err := saveBeadRespawnState(townRoot, state); err != nil {
		return 0, err
	}
	return rec.Count, nil
}

// ResetBeadRespawnCount resets the respawn counter for beadID to zero.
// Used by `gt sling respawn-reset` to allow re-dispatch after investigation.
func ResetBeadRespawnCount(workDir, beadID string) error {
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
	delete(state.Beads, beadID)
	return saveBeadRespawnState(townRoot, state)
}
