package reaper

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/intent"
)

// IntentWorkBeads returns the work bead ids the seats' intent records name.
// It reads the records under <townRoot>/.runtime/agents at the two levels
// seats live at, town and rig. An absent agents directory is an empty set;
// any other listing or record error returns an error, so no caller acts on a
// partial set.
func IntentWorkBeads(townRoot string) (map[string]struct{}, error) {
	base := filepath.Join(townRoot, ".runtime", "agents")
	beads := map[string]struct{}{}
	dirs := []string{base}
	for len(dirs) > 0 {
		dir := dirs[0]
		dirs = dirs[1:]
		entries, err := os.ReadDir(dir)
		if err != nil {
			if dir == base && os.IsNotExist(err) {
				return beads, nil
			}
			return nil, fmt.Errorf("listing intent records %q: %w", dir, err)
		}
		for _, e := range entries {
			switch {
			case e.IsDir() && dir == base:
				dirs = append(dirs, filepath.Join(dir, e.Name()))
			case strings.HasSuffix(e.Name(), ".json"):
				rec, err := intent.ReadPath(filepath.Join(dir, e.Name()))
				if err != nil {
					return nil, err
				}
				if rec.WorkBead != "" {
					beads[rec.WorkBead] = struct{}{}
				}
			}
		}
	}
	return beads, nil
}
