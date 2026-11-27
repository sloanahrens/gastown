package dockerstats

import (
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// Row is the schema v1 resource row for one parsed reading of the gt-dolt
// container, stamped at t.
//
// Rig, Polecat, Bead and Cluster stay nil: gt-dolt is one town-wide process,
// not work attributed to a rig, polecat, bead or cluster. The time is written in
// UTC so the row reads the same whatever zone the sampler runs in.
func Row(s Stats, t time.Time) resourcesampler.Row {
	return resourcesampler.Row{
		V:             resourcesampler.SchemaVersion,
		TS:            t.UTC().Format(time.RFC3339),
		IntervalS:     60,
		Source:        "docker",
		Class:         "control-plane",
		Component:     "dolt-server",
		Name:          "gt-dolt",
		CPUPct:        &s.CPUPct,
		MemBytes:      s.MemBytes,
		MemLimitBytes: &s.MemLimitBytes,
		Workspace:     workspaceid.CustomerZero,
	}
}
