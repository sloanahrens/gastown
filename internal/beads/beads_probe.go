// Package beads provides probe bead management.
package beads

import (
	"encoding/json"
	"fmt"
)

// CreateProbeBead creates an ephemeral, non-dispatchable bead for a one-off
// test or routing investigation (e.g. "does --repo route to the rig I
// expect?"). It exists so a probe can never outlive its purpose the way
// om-hoc and be-h2d did (gt-eje7): those were created type=bug at P2, so
// every field a dispatcher sorts on said "real work" and only the title
// (TEST-ROUTING-PROBE-DELETEME) said otherwise.
//
// Two structural guards, not one, because either alone reproduces the bug:
//   - --ephemeral / --wisp-type=probe keeps it out of `bd list` and `bd
//     ready` entirely while open, and gives it a short TTL under `gt
//     compact` (see compact.go's defaultTTLs["probe"]).
//   - --type=chore --priority=4 means that even if it is forgotten and
//     survives past that TTL, `gt compact` promotes it to a permanent bead
//     that reads as low-priority housekeeping, never a dispatchable bug.
//
// Close the returned issue (bd close) as soon as the question it was
// created to answer is settled. A probe left open is the bug this exists
// to prevent, not a smaller version of it.
func (b *Beads) CreateProbeBead(title, description string) (*Issue, error) {
	if IsFlagLikeTitle(title) {
		return nil, fmt.Errorf("refusing to create probe bead: %w (got %q)", ErrFlagTitle, title)
	}

	args := []string{"create", "--json",
		"--title=" + title,
		"--type=chore",
		"--priority=4",
		"--ephemeral",
		"--wisp-type=probe",
		"--labels=gt:probe",
	}
	if description != "" {
		args = append(args, "--description="+description)
	}

	// Default actor from BD_ACTOR env var for provenance tracking.
	// Uses getActor() to respect isolated mode (tests).
	if actor := b.getActor(); actor != "" {
		args = append(args, "--actor="+actor)
	}

	out, err := b.run(args...)
	if err != nil {
		return nil, err
	}

	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parsing bd create output: %w", err)
	}

	return &issue, nil
}
