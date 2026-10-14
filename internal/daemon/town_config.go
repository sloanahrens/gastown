package daemon

import (
	"github.com/steveyegge/gastown/internal/townconfig"
)

// CheckTownConfig reports every town config file that exists but does not
// load: it is the config kernel's Check (townconfig.Load over mayor/town.json,
// mayor/rigs.json, settings/config.json, the daemon patrol config (its "daemon"
// section, or the legacy mayor/daemon.json on an unmigrated town),
// settings/daemon.env and .dolt-data/config.yaml), one line per broken file.
//
// The town fails closed on a failure (gt-fcxe9.10, gt-y3pgh.1): the daemon
// refuses to start (New), the town-running commands and every agent session
// start refuse (the startup gate in internal/cmd and internal/bdgate), and gt
// doctor reports it. Nothing falls back to compiled defaults and nothing
// rewrites the file.
func CheckTownConfig(townRoot string) error {
	return townconfig.Check(townRoot)
}
