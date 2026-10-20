package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// A bead the dispatcher holds (unshaped, refused, planning) is annotated once,
// and annotateSpecOnce finds out whether the note is already there by reading
// the bead's comments: one bd call per held bead per tick, nearly all of them
// finding the note present. specAnnotated remembers, for each bead and note
// key, the comment_count the ready board showed when the note was found or
// written. While the board still shows that count no comment has been added
// since, so the note is still there and the read is skipped.
//
// The memory lasts as long as the daemon does (the owner is the one the detail
// cache uses), and a missing, unreadable or foreign file is an empty memory:
// the next tick reads as before, and annotateSpecOnce's read-before-write
// still writes the note once.

// specAnnotatedFile is the memory's file: for each bead, the comment_count
// recorded under the digest of each note key.
type specAnnotatedFile struct {
	Owner string                    `json:"owner"`
	Beads map[string]map[string]int `json:"beads"`
}

type specAnnotated struct {
	path  string // "" keeps the memory for this tick only
	owner string

	seen map[string]map[string]int
	// counts is this tick's board comment_count for each candidate; a bead
	// absent from it is not remembered or skipped.
	counts map[string]int
	// observed marks a board read that listed every rig, so the memory may be
	// pruned to the beads that read listed.
	observed bool
	dirty    bool
}

// loadSpecAnnotated is the memory path kept for the daemon named by owner. A
// tick no daemon started (owner "") keeps nothing between runs.
func loadSpecAnnotated(path, owner string) *specAnnotated {
	a := &specAnnotated{owner: owner, seen: map[string]map[string]int{}, counts: map[string]int{}}
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
	return a
}

// observe takes the comment_count of every candidate of the tick's board read.
func (a *specAnnotated) observe(read specBoardRead) {
	for _, c := range read.Candidates {
		a.counts[c.Spec.ID] = c.Spec.CommentCount
	}
	a.observed = len(read.Errors) == 0
}

// once runs annotate unless the memory holds key on beadID at the count the
// board shows now. annotate reports whether it wrote the note; either way the
// note is there afterwards, and is remembered at the count that includes it.
func (a *specAnnotated) once(beadID, key string, annotate func() (wrote bool, err error)) error {
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
	a.seen[beadID][digest] = count
	a.dirty = true
	return nil
}

// save writes the memory, dropped to the beads the tick's board listed.
func (a *specAnnotated) save() {
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
	}
	if !a.dirty {
		return
	}
	err := os.MkdirAll(filepath.Dir(a.path), 0o700)
	if err == nil {
		err = atomicfile.WriteJSONWithPerm(a.path, specAnnotatedFile{Owner: a.owner, Beads: a.seen}, 0o600)
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
