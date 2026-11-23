package seatship

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usageattrib"
)

// CollectInput names the seat Collect reads and the bounds it reads it with.
type CollectInput struct {
	Rig, Polecat, Bead     string
	Submitted              bool
	ProjectsDir, OutboxDir string
	OutboxCap              int64
	TailBytes              int
}

// Collect assembles a finished seat's whole payload from its transcripts,
// outbox and bead state, so the poster has nothing left to read. The usage
// facts are a work bead's polecat record for this rig and bead; the workspace
// is left for the daemon to fill. The transcript tail is read only for an
// unsubmitted seat, since a submitted one's record is already on its bead. Any
// part's error is returned wrapped with the part's name; nothing is retried.
func Collect(in CollectInput) (Payload, error) {
	seat := in.Polecat
	facts := usageattrib.Facts{
		Bead:  in.Bead,
		Kind:  dashboard.UsageKindWork,
		Class: dashboard.UsageClassPolecat,
		Rig:   in.Rig,
		Seat:  &seat,
	}
	usage, ids, err := UsageFrom(in.ProjectsDir, facts)
	if err != nil {
		return Payload{}, fmt.Errorf("seatship: collecting usage: %w", err)
	}
	outbox, err := OutboxFrom(in.OutboxDir, in.OutboxCap)
	if err != nil {
		return Payload{}, fmt.Errorf("seatship: collecting outbox: %w", err)
	}
	p := Payload{
		Rig:        in.Rig,
		Polecat:    in.Polecat,
		Bead:       in.Bead,
		Submitted:  in.Submitted,
		Usage:      &usage,
		SessionIDs: ids,
		Outbox:     outbox,
	}
	if !in.Submitted {
		if p.TranscriptTail, err = TailOf(in.ProjectsDir, in.TailBytes); err != nil {
			return Payload{}, fmt.Errorf("seatship: collecting transcript tail: %w", err)
		}
	}
	return p, nil
}
