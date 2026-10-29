package seattoken

import (
	"context"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/intent"
)

// revokeDeadline bounds one seat's whole revoke, so a tracker that never
// answers cannot hold a sandbox delete, a patrol tick or a crew removal.
const revokeDeadline = 30 * time.Second

// Revoker ends every live tracker token of an actor. An actor holding none is
// a success.
type Revoker interface {
	Revoke(ctx context.Context, actor string) error
}

// PolecatSeat is the seat a polecat occupies, whose actor address is
// <rig>/polecats/<name> — the GT_ROLE and BD_ACTOR the launch sets.
func PolecatSeat(rig, name string) intent.Seat {
	return intent.Seat{Rig: rig, Role: "polecats", Name: name}
}

// CrewSeat is the seat a crew member occupies, whose actor address is
// <rig>/crew/<name>.
func CrewSeat(rig, name string) intent.Seat {
	return intent.Seat{Rig: rig, Role: "crew", Name: name}
}

// RevokeSeat removes seat's token file and ends every tracker token issued to
// its actor, for a rig whose route names the tracker. A rig on Dolt has no
// tracker token, so nothing is revoked; a token file it still holds (a rig that
// left the tracker) is removed, since a file is all that is left to end. A nil
// revoker is the host's.
//
// The whole call is bounded by revokeDeadline, whatever deadline ctx carries.
// It is idempotent: a seat with no file and no live token succeeds.
func RevokeSeat(ctx context.Context, townRoot string, seat intent.Seat, revoker Revoker) error {
	ctx, cancel := context.WithTimeout(ctx, revokeDeadline)
	defer cancel()
	onTracker, err := beads.RigOnTracker(townRoot, seat.Rig)
	if err != nil {
		return fmt.Errorf("seattoken: checking the route of %s: %w", seat.String(), err)
	}
	if !onTracker {
		return New(townRoot).Remove(seat)
	}
	if revoker == nil {
		host, err := NewHostRevoker(townRoot)
		if err != nil {
			return fmt.Errorf("seattoken: building the tracker client for %s: %w", seat.String(), err)
		}
		revoker = host
	}
	return New(townRoot).Revoke(ctx, seat, revoker)
}
