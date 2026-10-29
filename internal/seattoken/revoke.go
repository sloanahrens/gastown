package seattoken

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/intent"
)

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
// its actor, for a rig whose route names the tracker; a rig on Dolt has no
// tracker token and is left alone. A nil revoker is the host's.
//
// It is idempotent: a seat with no file and no live token succeeds.
func RevokeSeat(ctx context.Context, townRoot string, seat intent.Seat, revoker Revoker) error {
	onTracker, err := beads.RigOnTracker(townRoot, seat.Rig)
	if err != nil {
		return fmt.Errorf("seattoken: revoke %s: %w", seat.String(), err)
	}
	if !onTracker {
		return nil
	}
	if revoker == nil {
		host, err := NewHostRevoker(townRoot)
		if err != nil {
			return fmt.Errorf("seattoken: revoke %s: %w", seat.String(), err)
		}
		revoker = host
	}
	return New(townRoot).Revoke(ctx, seat, revoker)
}
