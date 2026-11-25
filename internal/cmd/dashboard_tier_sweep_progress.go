package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// TierSweepProgress is the progress file the sweep runner writes (cmd/sweep-run/progress.go).
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

// findSweepProgress returns the progress.json of the newest run of rig's commit sha8
// under root. A run is a directory named rig-sha8-<tiers>: the run id tierSweepRunnerRunID
// builds in package daemon, unexported, so this name shape is the contract. The rig
// directory is read with os.ReadDir, not globbed, so a root name holding [ or * is a
// path. An unreadable rig directory is an error, never no run; a missing rig directory
// under an existing root is no run, and so is a run with no progress.json.
func findSweepProgress(root, rig, sha8 string) (string, bool, error) {
	if !plainSweepName(rig) || !plainSweepName(sha8) {
		return "", false, fmt.Errorf("find sweep progress: rig %q and sha %q must each be one plain name", rig, sha8)
	}
	if _, err := os.Stat(root); err != nil {
		return "", false, fmt.Errorf("find sweep progress under %s: %w", root, err)
	}
	rigDir := filepath.Join(root, rig)
	entries, err := os.ReadDir(rigDir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read sweep runs %s: %w", rigDir, err)
	}
	prefix := rig + "-" + sha8 + "-"
	var newest string
	var newestAt time.Time
	for _, e := range entries {
		if !e.IsDir() || e.Name() == prefix || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		progress := filepath.Join(rigDir, e.Name(), "progress.json")
		if _, err := os.Lstat(progress); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		info, err := os.Stat(progress)
		if err != nil {
			return "", false, fmt.Errorf("stat sweep progress %s: %w", progress, err)
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = progress, info.ModTime()
		}
	}
	return newest, newest != "", nil
}

// plainSweepName reports whether s is one plain path element: not empty, not a dot
// name, and with no separator, so it cannot climb out of the root.
func plainSweepName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}
