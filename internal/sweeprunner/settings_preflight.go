package sweeprunner

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/config"
)

type settingsDocument struct {
	Daemon struct {
		Patrols struct {
			TierSweep struct {
				Runner *config.TierSweepRunnerConfig `json:"runner"`
			} `json:"tier_sweep"`
		} `json:"patrols"`
	} `json:"daemon"`
}

// SettingsPreflight returns a builder that reads the settings when it is called,
// not now: the daemon pod reads the settings a caller hands it, the host's copy
// may be older. A failed read, an undecodable document, an absent runner block
// and a block CapacityPreflight refuses each write one line naming the cause to
// out and return nil: a preflight that did not answer is not a pass.
func SettingsPreflight(townRoot string, read func() ([]byte, error), out io.Writer) func() *Preflight {
	return func() *Preflight {
		data, err := read()
		if err != nil {
			fmt.Fprintf(out, "sweeprunner: cannot read the town settings: %v\n", err)
			return nil
		}
		var settings settingsDocument
		if err := json.Unmarshal(data, &settings); err != nil {
			fmt.Fprintf(out, "sweeprunner: cannot decode the town settings: %v\n", err)
			return nil
		}
		cfg := settings.Daemon.Patrols.TierSweep.Runner
		if cfg == nil {
			fmt.Fprintln(out, "sweeprunner: the town settings name no daemon.patrols.tier_sweep.runner")
			return nil
		}
		preflight, err := CapacityPreflight(cfg, townRoot)
		if err != nil {
			fmt.Fprintf(out, "sweeprunner: cannot build the capacity preflight: %v\n", err)
			return nil
		}
		return preflight
	}
}
