package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// A bead the dispatcher holds (unshaped, refused, planning) is annotated once,
// and annotateSpecOnce finds out whether the note is already there by reading
// the bead's comments: one bd call per held bead per tick, nearly all of them
// finding the note present. specAnnotated remembers, for each bead and note
// key, the comment_count at which the note was last known present. While the
// board still shows that count no comment has been added since, so the note is
// still there and the read is skipped.
//
// The same file carries the shape and label verdicts of the candidates the
// scan read (specCandidateVerdict), for the same reason: a candidate whose
// board row has not moved answers the same verdict without the details read
// (gt-km9jt).
//
// The memory lasts as long as the daemon does (the owner is the one the detail
// cache uses), and a missing, unreadable or foreign file is an empty memory:
// the next tick reads as before, and annotateSpecOnce's read-before-write
// still writes the note once.

// specAnnotatedFile is the memory's file: for each bead, the comment_count
// recorded under the digest of each note key, and the shape-and-label verdict
// the candidate scan last reached (specCandidateVerdict).
type specAnnotatedFile struct {
	Owner    string                          `json:"owner"`
	Beads    map[string]map[string]int       `json:"beads"`
	Verdicts map[string]specCandidateVerdict `json:"verdicts,omitempty"`
}

type specAnnotated struct {
	// mu guards the maps, the way the detail cache guards its own: one caller
	// drives the memory today, and a second one costs a slow read rather than
	// a map race (gt-fpxlb).
	mu sync.Mutex

	path  string // "" keeps the memory for this tick only
	owner string

	seen map[string]map[string]int
	// verdicts is each candidate's shape-and-label verdict as the tick that
	// read it decided, keyed by bead id.
	verdicts map[string]specCandidateVerdict
	// counts is this tick's board comment_count for each candidate; a bead
	// absent from it is not remembered or skipped.
	counts map[string]int
	// observed marks a board read that listed every rig, so the memory may be
	// pruned to the beads that read listed.
	observed bool
	dirty    bool
}

// loadSpecAnnotated loads the memory at path for the daemon named by owner. A
// tick no daemon started (owner "") keeps nothing between runs.
func loadSpecAnnotated(path, owner string) *specAnnotated {
	a := &specAnnotated{
		owner:    owner,
		seen:     map[string]map[string]int{},
		verdicts: map[string]specCandidateVerdict{},
		counts:   map[string]int{},
	}
	if owner == "" {
		return a
	}
	a.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		return a
	}
	var f specAnnotatedFile
	if json.Unmarshal(data, &f) != nil || f.Owner != owner || f.Beads == nil {
		return a
	}
	a.seen = f.Beads
	if f.Verdicts != nil {
		a.verdicts = f.Verdicts
	}
	return a
}

// specAnnotatedTick is one tick's annotation memory, wired to the three points
// the dispatcher drives it at: the board read it observes, the Annotate call,
// and the save at the tick's end. The tick and its tests share this value, so a
// test runs the wiring the tick runs rather than a copy of it (gt-fpxlb).
type specAnnotatedTick struct {
	observe  func(specBoardRead)
	annotate func(beadID, key, text string) error
	save     func()
	// memory is the value behind the wiring, which the candidate scan keeps its
	// verdicts in.
	memory *specAnnotated
}

// newSpecAnnotatedTick loads the memory the town's annotated path keeps for the
// daemon named by owner, and wires it to a tick. storeFor is the store a note
// is read and written through; it is asked only when the memory does not hold
// the note.
func newSpecAnnotatedTick(townRoot, owner string, storeFor func(beadID string) beads.Client) *specAnnotatedTick {
	a := loadSpecAnnotated(dispatch.SpecAnnotatedPath(townRoot), owner)
	return &specAnnotatedTick{
		observe: a.observe,
		memory:  a,
		annotate: func(beadID, key, text string) error {
			return a.once(beadID, key, func() (bool, error) {
				return annotateSpecWrote(storeFor(beadID), beadID, key, text)
			})
		},
		save: a.save,
	}
}

// observe takes the comment_count of every candidate of the tick's board read.
func (a *specAnnotated) observe(read specBoardRead) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range read.Candidates {
		a.counts[c.Spec.ID] = c.Spec.CommentCount
	}
	a.observed = len(read.Errors) == 0
}

// once runs annotate unless the memory holds key on beadID at the count the
// board shows now. annotate reports whether it wrote the note; either way the
// note is there afterwards, and is remembered at the count that includes it.
func (a *specAnnotated) once(beadID, key string, annotate func() (wrote bool, err error)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	count, known := a.counts[beadID]
	digest := specKeyDigest(key)
	if n, ok := a.seen[beadID][digest]; known && ok && n == count {
		return nil
	}
	wrote, err := annotate()
	if err != nil {
		return err
	}
	if !known {
		return nil
	}
	if wrote {
		count++
		a.counts[beadID] = count
	}
	if a.seen[beadID] == nil {
		a.seen[beadID] = map[string]int{}
	}
	// Comments only append, so every key already remembered for this bead is
	// still present at the count this note was found or written at: raising
	// them keeps a second key from leaving the first one's entry behind the
	// board's count, which would read its comments again next tick (gt-fpxlb).
	for d, n := range a.seen[beadID] {
		if n < count {
			a.seen[beadID][d] = count
		}
	}
	a.seen[beadID][digest] = count
	a.dirty = true
	return nil
}

// save writes the memory, dropped to the beads the tick's board listed.
func (a *specAnnotated) save() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.path == "" {
		return
	}
	if a.observed {
		for id := range a.seen {
			if _, listed := a.counts[id]; !listed {
				delete(a.seen, id)
				a.dirty = true
			}
		}
		for id := range a.verdicts {
			if _, listed := a.counts[id]; !listed {
				delete(a.verdicts, id)
				a.dirty = true
			}
		}
	}
	if !a.dirty {
		return
	}
	err := os.MkdirAll(filepath.Dir(a.path), 0o700)
	if err == nil {
		err = atomicfile.WriteJSONWithPerm(a.path, specAnnotatedFile{Owner: a.owner, Beads: a.seen, Verdicts: a.verdicts}, 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: spec annotated set not saved: %v\n", err)
		return
	}
	a.dirty = false
}

// specKeyDigest is a note key's short digest, so the file stays small however
// long the notes are.
func specKeyDigest(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:8])
}
