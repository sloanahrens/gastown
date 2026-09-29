package cmdtree

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/cmdtree/bdsnapshot"
)

// bdCommandTree is the reduced output of `bd capabilities --json` from the
// beads fork. Refresh it with `make bd-command-tree BEADS_SRC=<checkout>`
// (scripts/refresh-bd-command-tree.sh) whenever the fork's command surface
// or contract_version changes.
//
//go:embed bd-command-tree.json
var bdCommandTree []byte

// LoadBdTree returns the bd command tree from the embedded snapshot.
func LoadBdTree() (*Tree, bdsnapshot.Snapshot, error) {
	var snap bdsnapshot.Snapshot
	if err := json.Unmarshal(bdCommandTree, &snap); err != nil {
		return nil, snap, fmt.Errorf("parse bd-command-tree.json: %w", err)
	}
	if len(snap.Commands) == 0 {
		return nil, snap, fmt.Errorf("bd-command-tree.json lists no commands")
	}
	t := NewTree()
	for _, c := range snap.Commands {
		t.Add(strings.Fields(c.Path), c.Aliases, !c.SubcommandOnly)
	}
	return t, snap, nil
}
