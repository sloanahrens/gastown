package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// TierSweepProgress is the progress file the sweep runner rewrites after each
// tier (cmd/sweep-run/progress.go): the tiers finished so far and the one running.
type TierSweepProgress struct {
	Rig     string                        `json:"rig"`
	SHA     string                        `json:"sha"`
	Tiers   []dashboard.TierSweepRunStage `json:"tiers"`
	Running string                        `json:"running"`
}

// readTierSweepProgress decodes the progress file at path. A missing file is no
// progress yet: (zero, false, nil). Any other failure is an error naming the path.
func readTierSweepProgress(path string) (TierSweepProgress, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return TierSweepProgress{}, false, nil
	}
	if err != nil {
		return TierSweepProgress{}, false, fmt.Errorf("read tier sweep progress %s: %w", path, err)
	}
	var p TierSweepProgress
	if err := json.Unmarshal(raw, &p); err != nil {
		return TierSweepProgress{}, false, fmt.Errorf("decode tier sweep progress %s: %w", path, err)
	}
	return p, true, nil
}
