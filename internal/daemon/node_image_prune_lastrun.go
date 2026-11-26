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
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

const nodeImagePruneLastFile = "node_image_prune_last"

// readNodeImagePruneLast returns the run id the prune last ran for. A missing
// file means no run yet and reads as the empty id; any other read failure is an
// error, so an unreadable record is never mistaken for an unpruned node. The
// record is trimmed of surrounding whitespace, so a trailing newline written by
// another tool does not change the id the prune compares. A trimmed record the
// write could not have produced is an error too, so a record this read accepts
// is one the prune can use.
func readNodeImagePruneLast(townRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(constants.TownRuntimePath(townRoot), nodeImagePruneLastFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the node image prune last-run record: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if err := validateNodeImagePruneRunID(id); err != nil {
		return "", fmt.Errorf("reading the node image prune last-run record: %w", err)
	}
	return id, nil
}

// validateNodeImagePruneRunID reports an id the prune may record and act on: it
// is non-empty and holds only letters, digits, dash and underscore, so a value
// that names a path or a flag is refused. The read and the write share it, so
// the two cannot accept different ids.
func validateNodeImagePruneRunID(runID string) error {
	if runID == "" {
		return errors.New("node image prune run id is empty")
	}
	for _, r := range runID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return fmt.Errorf("node image prune run id %q holds %q, outside letters, digits, dash and underscore", runID, r)
		}
	}
	return nil
}

// writeNodeImagePruneLast records runID as the run the prune last ran for. The
// id must pass validateNodeImagePruneRunID so the read can take it back; the
// file is replaced by a rename at mode 0600.
func writeNodeImagePruneLast(townRoot, runID string) error {
	if err := validateNodeImagePruneRunID(runID); err != nil {
		return err
	}
	dir := constants.TownRuntimePath(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating the town runtime directory: %w", err)
	}
	return atomicfile.WriteFile(filepath.Join(dir, nodeImagePruneLastFile), []byte(runID), 0600)
}
