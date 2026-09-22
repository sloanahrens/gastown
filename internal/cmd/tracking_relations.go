package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

var (
	addTrackingRelationFn    = addTrackingRelation
	removeTrackingRelationFn = removeTrackingRelation
)

func addTrackingRelation(townRoot, trackerID, issueID string) error {
	// Refuse here rather than in each caller: this is the one place a tracks
	// edge is written, and an edge to a non-ID target can never be resolved
	// (gt-gsky).
	if !isTrackingTargetID(issueID) {
		return fmt.Errorf("refusing to record a tracks edge to %q: not a bead ID", issueID)
	}

	if err := mutateTrackingRelationViaStore(townRoot, trackerID, issueID, true); err != nil {
		return fallbackTrackingRelation(townRoot, trackerID, issueID, true, err)
	}
	return nil
}

// removeTrackingRelation is not gated by isTrackingTargetID, unlike
// addTrackingRelation: edges recorded before that gate existed point at targets
// that are not bead IDs, and removal is the only way to clean one up (gt-gsky).
func removeTrackingRelation(townRoot, trackerID, issueID string) error {
	if err := mutateTrackingRelationViaStore(townRoot, trackerID, issueID, false); err != nil {
		return fallbackTrackingRelation(townRoot, trackerID, issueID, false, err)
	}
	return nil
}

func mutateTrackingRelationViaStore(townRoot, trackerID, issueID string, add bool) error {
	resolvedBeads := beads.ResolveBeadsDir(townRoot)
	if resolvedBeads == "" {
		return fmt.Errorf("resolving town beads dir")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b := beads.NewWithBeadsDir(townRoot, resolvedBeads)
	store, cleanup, err := b.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	targetID := trackingDependsOnID(townRoot, issueID)
	actor := os.Getenv("BD_ACTOR")
	if actor == "" {
		actor = detectSender()
	}

	if add {
		dep := &beadsdk.Dependency{
			IssueID:     trackerID,
			DependsOnID: targetID,
			Type:        beadsdk.DependencyType("tracks"),
		}
		return store.AddDependency(ctx, dep, actor)
	}

	return store.RemoveDependency(ctx, trackerID, targetID, actor)
}

func fallbackTrackingRelation(townRoot, trackerID, issueID string, add bool, storeErr error) error {
	targetID := trackingDependsOnID(townRoot, issueID)
	args := []string{"dep", "add", trackerID, targetID, "--type=tracks"}
	if !add {
		args = []string{"dep", "remove", trackerID, targetID, "--type=tracks"}
	}

	if out, err := BdCmd(args...).Dir(townRoot).WithAutoCommit().StripBeadsDir().CombinedOutput(); err != nil {
		output := strings.TrimSpace(string(out))
		if output == "" {
			return fmt.Errorf("tracking relation via store failed: %w; fallback bd path failed: %w", storeErr, err)
		}
		return fmt.Errorf("tracking relation via store failed: %w; fallback bd path failed: %w; output: %s", storeErr, err, output)
	}

	return nil
}

// validateTrackingTargets reports the targets in issues that are not bead IDs,
// naming each offender, so a caller can refuse before it mutates anything
// (gt-gsky).
func validateTrackingTargets(issues []string) error {
	var invalid []string
	for _, issueID := range issues {
		if !isTrackingTargetID(issueID) {
			invalid = append(invalid, strconv.Quote(issueID))
		}
	}
	if len(invalid) == 0 {
		return nil
	}
	return fmt.Errorf("invalid tracking target(s): %s (expected a bead ID like gt-abc123, or external:<rig>:<id> for a cross-rig target)",
		strings.Join(invalid, ", "))
}

// isTrackingTargetID reports whether issueID is a plausible target for a
// tracks edge: either a bead ID, or the external:<rig>:<id> form a cross-rig
// target is stored as.
func isTrackingTargetID(issueID string) bool {
	if rest, ok := strings.CutPrefix(issueID, "external:"); ok {
		rig, id, ok := strings.Cut(rest, ":")
		return ok && isBeadIDToken(rig) && isBeadIDToken(id)
	}
	return isBeadIDToken(issueID)
}

// isBeadIDToken reports whether s is shaped like a bead ID: a leading
// alphanumeric followed by letters, digits, hyphen, underscore, or dot.
//
// This is the check that keeps a convoy *title* out of the dependency table: a
// name like "om-gate coverage: om" opens with a short lowercase word, so the
// cross-rig resolver wrapped it as external:om:<name> and the convoy could
// never resolve that edge — or auto-close — again (gt-gsky).
func isBeadIDToken(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
			// Allowed, but never first: a bead ID opens with its prefix
			// (gt-, hq-, om-), not a separator.
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func trackingDependsOnID(townRoot, issueID string) string {
	if strings.HasPrefix(issueID, "external:") {
		return issueID
	}

	prefix := beads.ExtractPrefix(issueID)
	if prefix == "" {
		return issueID
	}

	if rigName := beads.GetRigNameForPrefix(townRoot, prefix); rigName != "" {
		return fmt.Sprintf("external:%s:%s", strings.TrimSuffix(prefix, "-"), issueID)
	}

	return issueID
}
