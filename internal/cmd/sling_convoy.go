package cmd

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	convoyops "github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/workspace"
)

// slingGenerateShortID generates a short random ID (5 lowercase chars).
func slingGenerateShortID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.EncodeToString(b)[:5])
}

// slingConvoyTown is the town sling's convoy bookkeeping runs in: its root
// and the bd that answers for it. The package functions below find the town
// from the cwd and use the bd on PATH; a unit test builds one over a temp
// town and an in-process bd, so nothing is spawned and no cwd or PATH is read.
type slingConvoyTown struct {
	root string
	// db is the town database; nil is bd pinned to the town's .beads.
	db convoyops.Store
}

// townDB is the town database.
func (c slingConvoyTown) townDB() beads.Client {
	if c.db != nil {
		return c.db
	}
	return beads.NewPinned(c.beadsDir())
}

func (c slingConvoyTown) beadsDir() string { return filepath.Join(c.root, ".beads") }

// convoys is the convoy package's view of the town database.
func (c slingConvoyTown) convoys() convoyops.Town {
	town := convoyops.Town{Root: c.beadsDir(), Out: os.Stdout, Warn: os.Stderr}
	if c.db != nil {
		town.Open = func(string) convoyops.Store { return c.db }
		town.Issues = c.db
	}
	return town
}

// isTrackedByConvoy checks if an issue is already being tracked by a convoy.
// Returns the convoy ID if tracked, empty string otherwise.
func isTrackedByConvoy(beadID string) string {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return ""
	}
	return slingConvoyTown{root: townRoot}.trackingConvoy(beadID)
}

// trackingConvoy returns the open convoy tracking beadID, or "".
//
// Uses convoy.DepListRawIDs for cross-database dep resolution (GH #2624).
// For direction=up queries, the raw SQL approach queries the same table but
// looks for rows where depends_on_id matches the beadID, returning the
// issue_id (which is the convoy). Since this only returns IDs (no issue_type
// or status), we verify each candidate via bd show.
func (c slingConvoyTown) trackingConvoy(beadID string) string {
	// Primary: Use raw dep query to find what tracks this issue (direction=up).
	// This returns convoy IDs that have a "tracks" dep on beadID.
	trackerIDs, err := c.convoys().DepListRawIDs(c.beadsDir(), beadID, "up", "tracks")
	if err == nil && len(trackerIDs) > 0 {
		// Check each tracker to find an open convoy
		for _, trackerID := range trackerIDs {
			tracker, err := c.townDB().Show(trackerID)
			if err != nil {
				continue
			}
			if convoyops.IsConvoyIssue(tracker.Type, tracker.Labels) && tracker.Status == "open" {
				return trackerID
			}
		}
	}

	// Fallback: Query convoys directly by description pattern
	// This is more robust when cross-rig routing has issues (G19, G21)
	// Auto-convoys have description "Auto-created convoy tracking <beadID>"
	return c.convoyByDescription(beadID)
}

// convoyByDescription searches open convoys for one tracking the given beadID.
// Checks both convoy descriptions (for auto-created convoys) and tracked deps
// (for manually-created convoys where the description won't match).
// Returns convoy ID if found, empty string otherwise.
func (c slingConvoyTown) convoyByDescription(beadID string) string {
	convoys, err := c.convoys().ListConvoys("open", false)
	if err != nil {
		return ""
	}

	// Check if any convoy's description mentions tracking this beadID
	// (matches auto-created convoys with "Auto-created convoy tracking <beadID>")
	trackingPattern := fmt.Sprintf("tracking %s", beadID)
	for _, convoy := range convoys {
		if strings.Contains(convoy.Description, trackingPattern) {
			return convoy.ID
		}
	}

	// Check tracked deps of each convoy (for manually-created convoys).
	// This handles the case where cross-rig dep resolution (direction=up) fails
	// but the convoy does have a tracks dependency on the bead.
	for _, convoy := range convoys {
		if c.convoyTracksBead(convoy.ID, beadID) {
			return convoy.ID
		}
	}

	return ""
}

// convoyTracksBead checks if a convoy has a tracks dependency on the given beadID.
// Uses convoy.DepListRawIDs for cross-database dep resolution (GH #2624).
func (c slingConvoyTown) convoyTracksBead(convoyID, beadID string) bool {
	trackedIDs, err := c.convoys().DepListRawIDs(c.beadsDir(), convoyID, "down", "tracks")
	if err != nil {
		return false
	}

	for _, id := range trackedIDs {
		if id == beadID {
			return true
		}
	}
	return false
}

// ConvoyInfo holds convoy details for an issue's tracking convoy.
type ConvoyInfo struct {
	ID            string // Convoy bead ID (e.g., "hq-cv-abc")
	Owned         bool   // true if convoy has gt:owned label
	MergeStrategy string // "mr", "local", or "" (default = mr)
}

// createAutoConvoy creates an auto-convoy for a single issue and tracks it.
// If owned is true, the convoy is marked with the gt:owned label for caller-managed lifecycle.
// mergeStrategy is optional: "mr" or "local" (empty = default mr).
// agent is the runtime agent requested with --agent at sling time (empty if
// none). formula is the formula requested with --formula at sling time (empty
// if none). Both are persisted on the convoy so that a convoy feeder
// re-dispatching this bead after a failed sling re-uses the same agent and
// formula instead of the rig default (gt-yg24, gt-4lor).
// Returns the created convoy ID.
func createAutoConvoy(beadID, beadTitle string, owned bool, mergeStrategy, baseBranch, agent, formula string) (_ string, retErr error) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return "", fmt.Errorf("finding town root: %w", err)
	}
	return slingConvoyTown{root: townRoot}.createAutoConvoy(beadID, beadTitle, owned, mergeStrategy, baseBranch, agent, formula)
}

// createAutoConvoy is createAutoConvoy in this town.
func (c slingConvoyTown) createAutoConvoy(beadID, beadTitle string, owned bool, mergeStrategy, baseBranch, agent, formula string) (string, error) {
	// Guard against flag-like titles propagating into convoy names (gt-e0kx5)
	if beads.IsFlagLikeTitle(beadTitle) {
		return "", fmt.Errorf("refusing to create convoy: bead title %q looks like a CLI flag", beadTitle)
	}

	// Generate convoy ID with hq-cv- prefix for visual distinction
	// The hq-cv- prefix is registered in routes during gt install
	convoyID := fmt.Sprintf("hq-cv-%s", slingGenerateShortID())

	// Create convoy with title "Work: <issue-title>"
	convoyTitle := fmt.Sprintf("Work: %s", beadTitle)
	prose := fmt.Sprintf("Auto-created convoy tracking %s", beadID)
	description := beads.SetConvoyFields(&beads.Issue{Description: prose}, &beads.ConvoyFields{
		Merge:      mergeStrategy,
		BaseBranch: baseBranch,
		Agent:      strings.TrimSpace(agent),
		Formula:    strings.TrimSpace(formula),
	})

	// The pinned client auto-commits, so the convoy is persisted even when
	// gt sling has set BD_DOLT_AUTO_COMMIT=off globally (gt-9xum2).
	if _, err := c.townDB().Create(beads.CreateOptions{
		ID:          convoyID,
		Title:       convoyTitle,
		Description: description,
		Labels:      convoyLabels(owned),
		Priority:    -1,
	}); err != nil {
		return "", fmt.Errorf("creating convoy: %w", err)
	}

	// Add tracking relation: convoy tracks the issue.
	if err := addTrackingRelationWith(c.townDB(), c.root, convoyID, beadID); err != nil {
		fmt.Printf("Warning: Could not create auto-convoy tracking: %v\n", err)
	}

	return convoyID, nil
}

// validateConvoyMergeFlag checks a --merge value for gt sling and gt convoy
// create. "direct" was removed (gt-fcxe9.4): it pushed polecat branches to
// the default branch from gt done with no gate, no om review and no merge
// slot (G2-02), and no convoy ever used it.
func validateConvoyMergeFlag(v string) error {
	switch v {
	case "", "mr", "local":
		return nil
	case "direct":
		return fmt.Errorf("--merge=direct was removed (gt-fcxe9.4): work lands through the merge queue (--merge=mr, the default) or stays on its branch (--merge=local)")
	default:
		return fmt.Errorf("invalid --merge value %q: must be mr or local", v)
	}
}
