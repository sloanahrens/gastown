package beads

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SlingContextRecord is one open sling context and the store it came from.
type SlingContextRecord struct {
	Issue    *Issue
	WorkDir  string
	BeadsDir string
}

// SlingContextSearchDirs returns the directories that can hold sling
// contexts: the town root plus every rig directory (and its mayor/rig) that
// has a .beads directory.
func SlingContextSearchDirs(townRoot string) ([]string, error) {
	dirs := []string{townRoot}
	seen := map[string]bool{townRoot: true}
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering scheduler beads search dirs: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "mayor" || e.Name() == "settings" {
			continue
		}
		rigDir := filepath.Join(townRoot, e.Name())
		beadsDir := filepath.Join(rigDir, ".beads")
		if _, err := os.Stat(beadsDir); err == nil && !seen[rigDir] {
			dirs = append(dirs, rigDir)
			seen[rigDir] = true
		}
		mayorRigDir := filepath.Join(rigDir, "mayor", "rig")
		mayorBeadsDir := filepath.Join(mayorRigDir, ".beads")
		if _, err := os.Stat(mayorBeadsDir); err == nil && !seen[mayorRigDir] {
			dirs = append(dirs, mayorRigDir)
			seen[mayorRigDir] = true
		}
	}
	return dirs, nil
}

// ListOpenSlingContextRecords returns every open sling context across the
// town's stores. Sling contexts are created in the target rig's beads dir
// (GH#3468), so it scans the town plus every rig. It does not filter by
// readiness or circuit breaker.
//
// It deduplicates by store and context ID: two search dirs can resolve to
// one database (a rig's top-level .beads redirecting to mayor/rig/.beads).
func ListOpenSlingContextRecords(townRoot string) ([]SlingContextRecord, error) {
	var records []SlingContextRecord
	seen := make(map[string]bool)
	dirs, err := SlingContextSearchDirs(townRoot)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		beadsDir := ResolveBeadsDir(dir)
		b := NewWithBeadsDir(dir, beadsDir)
		contexts, err := b.ListOpenSlingContexts()
		if err != nil {
			return nil, fmt.Errorf("listing sling contexts in %s: %w", beadsDir, err)
		}
		for _, ctx := range contexts {
			key := beadsDir + "\x00" + ctx.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			records = append(records, SlingContextRecord{Issue: ctx, WorkDir: dir, BeadsDir: beadsDir})
		}
	}
	return records, nil
}

// AreScheduled reports which of beadIDs have an open sling context under
// townRoot. It fails closed: when the contexts cannot be listed, every ID is
// reported scheduled, so no caller dispatches on a guess.
func AreScheduled(townRoot string, beadIDs []string) map[string]bool {
	result := make(map[string]bool)
	if len(beadIDs) == 0 {
		return result
	}

	contexts, err := ListOpenSlingContextRecords(townRoot)
	if err != nil {
		for _, id := range beadIDs {
			result[id] = true
		}
		return result
	}

	// Cleanup owns stale-state closure; idempotency must not use a different
	// definition of scheduled.
	scheduledWorkBeads := make(map[string]bool)
	for _, rec := range contexts {
		if fields := ParseSlingContextFields(rec.Issue.Description); fields != nil {
			scheduledWorkBeads[fields.WorkBeadID] = true
		}
	}
	for _, id := range beadIDs {
		if scheduledWorkBeads[id] {
			result[id] = true
		}
	}
	return result
}
