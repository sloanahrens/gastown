package townconfig

// Dispatch holds (gt-vty1v).
//
// A rig is held out of new automatic dispatches when its registry entry
// carries a "dispatch_hold" record {since, by, reason}. gt rig hold and gt rig
// release are the only writers (Hold, Release); automatic dispatchers read it
// through dispatch.RigHold, along with the operator hold file and the rig's
// ESTOP.
//
// The record is deliberately not a park: parking also stops the rig's agents
// and the daemon's auto-start, while a hold leaves both alone, so a rig can let
// a landing backlog drain while the work already running finishes. The two
// records are independent — holding a parked rig changes neither record, and
// releasing a hold on a parked rig leaves it parked.

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// DispatchHoldState returns rigName's dispatch-hold record, or nil when the rig
// carries none. A town with no rig registry has no records, so it answers nil;
// a registry that is present and does not load answers an error, which a
// dispatcher must treat as held (gt-vty1v).
func DispatchHoldState(root, rigName string) (*config.RigDispatchHold, error) {
	rigs, err := config.LoadRigsConfigOrEmpty(filepath.Join(root, FileRigs))
	if err != nil {
		return nil, err
	}
	entry, ok := rigs.Rigs[rigName]
	if !ok || entry.DispatchHold == nil {
		return nil, nil
	}
	held := *entry.DispatchHold
	return &held, nil
}

// DescribeHold renders a dispatch-hold record as one line.
func DescribeHold(h *config.RigDispatchHold) string {
	s := "held since " + h.Since.Format(time.RFC3339)
	if h.By != "" {
		s += " by " + h.By
	}
	if h.Reason != "" {
		s += ": " + h.Reason
	}
	return s
}

// Hold writes rec as rigName's dispatch-hold record. A rig that is already held
// keeps its record; the record in effect is returned. Only gt rig hold calls it.
func Hold(root, rigName string, rec config.RigDispatchHold) (config.RigDispatchHold, error) {
	var kept config.RigDispatchHold
	err := updateRig(root, rigName, func(entry *config.RigEntry) {
		if entry.DispatchHold == nil {
			h := rec
			entry.DispatchHold = &h
		}
		kept = *entry.DispatchHold
	})
	if err != nil {
		return config.RigDispatchHold{}, err
	}
	return kept, nil
}

// Release clears rigName's dispatch-hold record and reports whether the rig was
// held. Only gt rig release calls it.
func Release(root, rigName string) (bool, error) {
	was := false
	err := updateRig(root, rigName, func(entry *config.RigEntry) {
		was = entry.DispatchHold != nil
		entry.DispatchHold = nil
	})
	return was, err
}

// DispatchHeldReason is DispatchHoldState as the one-line hold reason an
// automatic dispatcher reports, or "" when the rig may dispatch. An unreadable
// registry fails closed: dispatching through a hold that cannot be ruled out is
// what the record exists to prevent (gt-vty1v).
func DispatchHeldReason(root, rigName string) string {
	held, err := DispatchHoldState(root, rigName)
	if err != nil {
		return fmt.Sprintf("dispatch hold unreadable (%v); treating as held", err)
	}
	if held == nil {
		return ""
	}
	return fmt.Sprintf("%s; release it with: gt rig release %s", DescribeHold(held), rigName)
}
