package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// reusedBlockedReader wraps read, a rig's blocked read, to answer memory's stored
// list while its hashes hold (gt-8ihrcc). A successful read is noted under the
// hashes held before the read; a failed one returns its error and stores nothing.
// A nil memory, nil heads or a rig absent from heads reads every time.
func reusedBlockedReader(memory *specAnnotated, heads map[string]beadsql.RigHash, now func() time.Time, read func(rig string) ([]*beads.Issue, error)) func(rig string) ([]*beads.Issue, error) {
	return func(rig string) ([]*beads.Issue, error) {
		at := now()
		if issues, ok := memory.rigBlocked(rig, heads, at); ok {
			return issues, nil
		}
		hash, held := heads[rig]
		issues, err := read(rig)
		if err != nil {
			return nil, err
		}
		if memory != nil && held {
			memory.noteRigBlocked(rig, hash, at, issues)
		}
		return issues, nil
	}
}
