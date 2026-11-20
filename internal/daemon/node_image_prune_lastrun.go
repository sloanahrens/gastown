package daemon

// The last pruned update run: the id of the update roll whose images the node
// image prune last ran for, kept in the town's runtime directory so a daemon
// restart does not prune the same roll twice (gt-wklmiw).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

const nodeImagePruneLastFile = "node_image_prune_last"

// readNodeImagePruneLast returns the run id the prune last ran for. A missing
// file means no run yet and reads as the empty id; any other read failure is an
// error, so an unreadable record is never mistaken for an unpruned node.
func readNodeImagePruneLast(townRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(constants.TownRuntimePath(townRoot), nodeImagePruneLastFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the node image prune last-run record: %w", err)
	}
	return string(data), nil
}

// writeNodeImagePruneLast records runID as the run the prune last ran for. The
// id must be non-empty and hold only letters, digits, dash and underscore, so a
// value that names a path or a flag cannot be recorded; the file is replaced by
// a rename at mode 0600.
func writeNodeImagePruneLast(townRoot, runID string) error {
	if runID == "" {
		return errors.New("node image prune run id is empty")
	}
	for _, r := range runID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return fmt.Errorf("node image prune run id %q holds %q, outside letters, digits, dash and underscore", runID, r)
		}
	}
	dir := constants.TownRuntimePath(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating the town runtime directory: %w", err)
	}
	return atomicfile.WriteFile(filepath.Join(dir, nodeImagePruneLastFile), []byte(runID), 0600)
}
