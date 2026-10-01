package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

var addTrackingRelationFn = addTrackingRelation

// trackingDeps is the part of beads.Client that writes tracks edges.
type trackingDeps interface {
	AddTypedDependency(issue, dependsOn, depType string) error
}

// townTrackingDeps returns the bd client for the town database, where
// convoys (hq-cv-*) and their tracks edges live.
func townTrackingDeps(townRoot string) (trackingDeps, error) {
	resolved := beads.ResolveBeadsDir(townRoot)
	if resolved == "" {
		return nil, fmt.Errorf("resolving town beads dir")
	}
	return beads.NewWithBeadsDir(townRoot, resolved), nil
}

func addTrackingRelation(townRoot, trackerID, issueID string) error {
	// Refuse here rather than in each caller: this is the one place a tracks
	// edge is written, and an edge to a non-ID target can never be resolved
	// (gt-gsky). The refusal comes before the client, so it needs no town.
	if !isTrackingTargetID(issueID) {
		return fmt.Errorf("refusing to record a tracks edge to %q: not a bead ID", issueID)
	}
	deps, err := townTrackingDeps(townRoot)
	if err != nil {
		return err
	}
	return addTrackingRelationWith(deps, townRoot, trackerID, issueID)
}

// addTrackingRelationWith writes trackerID --tracks--> issueID through bd
// (bd dep add --type=tracks); a cross-rig target is stored as
// external:<rig>:<id>. It is the only write path: the in-process store call
// it replaced wrote through the v1.0.5 library (gt-7iwy0.2).
func addTrackingRelationWith(deps trackingDeps, townRoot, trackerID, issueID string) error {
	if !isTrackingTargetID(issueID) {
		return fmt.Errorf("refusing to record a tracks edge to %q: not a bead ID", issueID)
	}
	targetID := trackingDependsOnID(townRoot, issueID)
	if err := deps.AddTypedDependency(trackerID, targetID, "tracks"); err != nil {
		return fmt.Errorf("recording %s tracks %s: %w", trackerID, targetID, err)
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
		return ok && beads.IsBeadIDToken(rig) && beads.IsBeadIDToken(id)
	}
	return beads.IsBeadIDToken(issueID)
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
