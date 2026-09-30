package daemon

import (
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/plugin"
)

// handlePlugins runs every due cooldown plugin. This is the main entry point
// called from heartbeat.
//
// There is no agent behind a plugin any more: a script plugin runs in the
// daemon and a failure logs and escalates (gt-ckunw). The LLM dog pack that
// used to take non-script plugins and failed scripts was retired after its
// dispatches stopped fixing what they were sent for.
func (d *Daemon) handlePlugins() {
	rigsConfig, err := d.loadRigsConfig()
	if err != nil {
		d.logger.Printf("Handler: failed to load rigs config: %v", err)
		return
	}
	d.dispatchPlugins(rigsConfig)
}

// dispatchPlugins scans for plugins, evaluates cooldown gates, and starts
// each eligible script plugin.
func (d *Daemon) dispatchPlugins(rigsConfig *config.RigsConfig) {
	// Get rig names for scanner
	var rigNames []string
	if rigsConfig != nil {
		for name := range rigsConfig.Rigs {
			rigNames = append(rigNames, name)
		}
	}

	scanner := plugin.NewScanner(d.config.TownRoot, rigNames)
	plugins, err := scanner.DiscoverAll()
	if err != nil {
		d.logger.Printf("Handler: failed to discover plugins: %v", err)
		return
	}

	if len(plugins) == 0 {
		return
	}

	recorder := plugin.NewRecorder(d.config.TownRoot)

	for _, p := range plugins {
		// Never auto-dispatch manual-gate plugins — they require an explicit trigger.
		if p.Gate != nil && p.Gate.Type == plugin.GateManual {
			d.logger.Printf("Handler: skipping plugin %s (gate=manual, requires explicit trigger)", p.Name)
			continue
		}

		// Only dispatch plugins with cooldown gates.
		if p.Gate == nil || p.Gate.Type != plugin.GateCooldown {
			continue
		}

		// Evaluate cooldown: skip if plugin ran recently.
		if p.Gate.Duration != "" {
			count, err := recorder.CountRunsSince(p.Name, p.Gate.Duration)
			if err != nil {
				d.logger.Printf("Handler: error checking cooldown for plugin %s: %v", p.Name, err)
				continue
			}
			if count > 0 {
				continue // Still in cooldown
			}
		}

		// Only script plugins run automatically: the daemon executes run.sh
		// itself, writes the run record when it finishes (with its real
		// result), and the in-flight guard keeps the next heartbeat from
		// starting a second copy. A plugin with nothing to execute has no
		// runner since the dog pack was retired (gt-ckunw); it is left
		// unrecorded so the log keeps saying so.
		if !runsAsScript(p) {
			d.logger.Printf("Handler: skipping plugin %s (not a script plugin with a run.sh; nothing runs it)", p.Name)
			continue
		}
		d.startScriptPlugin(p, recorder)
	}
}

// loadRigsConfig loads the rigs configuration from mayor/rigs.json.
func (d *Daemon) loadRigsConfig() (*config.RigsConfig, error) {
	rigsPath := filepath.Join(d.config.TownRoot, "mayor", "rigs.json")
	return config.LoadRigsConfig(rigsPath)
}

// loadOperationalConfig loads operational thresholds from town settings.
// Returns a valid (never nil) config — accessors return defaults for nil fields.
func (d *Daemon) loadOperationalConfig() *config.OperationalConfig {
	return config.LoadOperationalConfig(d.config.TownRoot)
}
