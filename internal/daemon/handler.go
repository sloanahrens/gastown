package daemon

import (
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/plugin"
)

// pluginRunSource is where a tick reads its cooldown gates and records the
// runs it starts. recorderSource over the town's *plugin.Recorder is the
// production one; a test injects a fake to see what a tick asks it.
type pluginRunSource interface {
	LoadRunLedger(since time.Duration) (pluginRunLedger, error)
	RecordRun(rec plugin.PluginRunRecord) (string, error)
}

// pluginRunLedger answers one plugin's cooldown gate from a single read of the
// run ledger (plugin.RunLedger).
type pluginRunLedger interface {
	Count(pluginName string, window time.Duration) (int, error)
}

// recorderSource adapts the town's *plugin.Recorder to pluginRunSource.
type recorderSource struct {
	recorder *plugin.Recorder
}

func (s recorderSource) LoadRunLedger(since time.Duration) (pluginRunLedger, error) {
	ledger, err := s.recorder.LoadRunLedger(since)
	if err != nil {
		return nil, err
	}
	return ledger, nil
}

func (s recorderSource) RecordRun(rec plugin.PluginRunRecord) (string, error) {
	return s.recorder.RecordRun(rec)
}

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
		// A missing mayor/rigs.json or one bad rig entry costs the rig-level
		// plugins, not every plugin: town plugins need no rig.
		d.logger.Printf("Handler: failed to load rigs config, dispatching town plugins only: %v", err)
		rigsConfig = nil
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

	runs := d.pluginRuns
	if runs == nil {
		runs = recorderSource{plugin.NewRecorder(d.config.TownRoot)}
	}

	// One read of the run ledger answers every cooldown gate in this tick.
	var ledger pluginRunLedger
	if lookback := ledgerLookback(plugins); lookback > 0 {
		ledger, err = runs.LoadRunLedger(lookback)
		if err != nil {
			// Fail closed: a gate whose ledger cannot be read is not known to
			// be open, so no cooldown-gated plugin runs this tick.
			d.logger.Printf("Handler: failed to read the plugin run ledger, skipping cooldown-gated plugins: %v", err)
		}
	}

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
		window, err := p.Gate.RunLedgerWindow()
		if err != nil {
			d.logger.Printf("Handler: error checking cooldown for plugin %s: %v", p.Name, err)
			continue
		}
		// A window that reaches no run holds nothing closed: the plugin runs
		// without a ledger answer.
		if window > 0 {
			if ledger == nil {
				continue // The tick logged why the ledger is unreadable.
			}
			count, err := ledger.Count(p.Name, window)
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
		d.startScriptPlugin(p, runs)
	}
}

// ledgerLookback is the span one tick's read of the run ledger must cover: the
// longest window any plugin's gate reads, so every plugin is answered from
// that one read. Zero means no gate reads the ledger and the tick loads none.
func ledgerLookback(plugins []*plugin.Plugin) time.Duration {
	var longest time.Duration
	for _, p := range plugins {
		window, err := p.Gate.RunLedgerWindow()
		if err != nil {
			// The plugin is refused with a log line when its gate is
			// evaluated; it sets no span for the rest of the tick.
			continue
		}
		if window > longest {
			longest = window
		}
	}
	return longest
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
