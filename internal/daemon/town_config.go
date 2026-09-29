package daemon

import (
	"errors"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
)

func init() {
	// config.SaveDaemonPatrolConfig must refuse the same daemon.json the
	// startup gate refuses, which needs this package's type.
	config.RegisterDaemonPatrolConfigCheck(func(path string) error {
		var cfg DaemonPatrolConfig
		return config.CheckJSONFileParses(path, &cfg)
	})
}

// CheckTownConfig reports every town config file that exists but does not
// decode: mayor/daemon.json into DaemonPatrolConfig and settings/config.json
// into config.TownSettings. An absent file passes (first run creates it).
//
// The town fails closed on a failure (gt-fcxe9.10): the daemon refuses to
// start (New), the town-running commands and every agent session start refuse
// (the startup gate in internal/cmd and internal/bdgate), and gt doctor reports
// it. Nothing falls back to compiled defaults and nothing rewrites the file.
func CheckTownConfig(townRoot string) error {
	var daemonCfg DaemonPatrolConfig
	var settings config.TownSettings
	return errors.Join(
		config.CheckJSONFileParses(PatrolConfigFile(townRoot), &daemonCfg),
		config.CheckJSONFileParses(filepath.Join(townRoot, "settings", "config.json"), &settings),
	)
}
