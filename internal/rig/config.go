// Package rig provides rig management functionality.
// This file implements the property layer lookup API for unified config access.
package rig

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/wisp"
)

// ConfigSource identifies which layer a config value came from.
type ConfigSource string

const (
	SourceWisp    ConfigSource = "wisp"    // Local wisp layer (.beads-wisp/config/)
	SourceBead    ConfigSource = "bead"    // Rig identity bead labels
	SourceTown    ConfigSource = "town"    // Town defaults (~/gt/settings/config.json)
	SourceSystem  ConfigSource = "system"  // Compiled-in system defaults
	SourceBlocked ConfigSource = "blocked" // Explicitly blocked at wisp layer
	SourceNone    ConfigSource = "none"    // No value found
)

// ConfigResult holds a config lookup result with its source.
type ConfigResult struct {
	Value  interface{}
	Source ConfigSource
}

// SystemDefaults contains compiled-in default values.
// These are the fallback when no other layer provides a value.
var SystemDefaults = map[string]interface{}{
	"status":                  "operational",
	"auto_restart":            true,
	"auto_start_on_up":        false, // If true, rig agents start on gt up even when docked
	"max_polecats":            10,
	"priority_adjustment":     0,
	"dnd":                     false,
	"polecat_branch_template": "", // Empty = use default behavior (polecat/{name}/...)
	"default_formula":         "mol-polecat-work",
}

// StackingKeys defines which keys use stacking semantics (values add up).
// All other keys use override semantics (first non-nil wins).
var StackingKeys = map[string]bool{
	"priority_adjustment": true,
}

// GetConfig looks up a config value through all layers.
// Override semantics: first non-nil value wins.
// Layers are checked in order: wisp -> bead -> town -> system
func (r *Rig) GetConfig(key string) interface{} {
	result := r.GetConfigWithSource(key)
	return result.Value
}

// GetConfigWithSource looks up a config value and returns which layer it came from.
func (r *Rig) GetConfigWithSource(key string) ConfigResult {
	townRoot := filepath.Dir(r.Path)

	// Layer 1: Wisp (transient, local)
	wispCfg := wisp.NewConfig(townRoot, r.Name)
	if wispCfg.IsBlocked(key) {
		return ConfigResult{Value: nil, Source: SourceBlocked}
	}
	if val := wispCfg.Get(key); val != nil {
		return ConfigResult{Value: val, Source: SourceWisp}
	}

	// Layer 2: Rig identity bead labels
	if val := r.getBeadLabel(key); val != nil {
		return ConfigResult{Value: val, Source: SourceBead}
	}

	// Layer 3: Town defaults
	// Note: Town defaults for operational state would typically be in
	// ~/gt/settings/config.json. For now, we skip directly to system defaults.
	// Future: load from config.TownSettings

	// Layer 4: System defaults
	if val, ok := SystemDefaults[key]; ok {
		return ConfigResult{Value: val, Source: SourceSystem}
	}

	return ConfigResult{Value: nil, Source: SourceNone}
}

// GetBoolConfig looks up a boolean config value.
// Returns false if not set or blocked.
//
// Values are coerced with CoerceBool rather than type-asserted, so numeric
// spellings of 0/1 written to the wisp layer (where JSON numbers load back as
// float64) mean the same thing as their boolean counterparts.
func (r *Rig) GetBoolConfig(key string) bool {
	return CoerceBool(r.GetConfig(key))
}

// CoerceBool interprets a stored config value as a boolean.
// Returns false for nil and for values it cannot interpret.
//
// Config values travel through layers that each lose type information: bead
// labels arrive as strings, and wisp values round-trip through JSON, where every
// number loads back as float64. A key with no entry in SystemDefaults also gets
// its value guessed at write time, which lands "1"/"0" as numbers. Numeric 0 is
// therefore false and any other number is true, so those values keep the meaning
// their author intended instead of silently reading as false.
func CoerceBool(v interface{}) bool {
	switch val := v.(type) {
	case nil:
		return false
	case bool:
		return val
	case string:
		// String booleans from bead labels. The spellings match parseBool in
		// internal/cmd, which is what parses values on the way in, plus the
		// "t"/"f" shorthands strconv.ParseBool allows.
		switch strings.ToLower(val) {
		case "true", "yes", "1", "on", "t":
			return true
		case "false", "no", "0", "off", "f":
			return false
		}
		return false
	case int:
		return val != 0
	case int64:
		return val != 0
	case float64:
		return val != 0
	default:
		return false
	}
}

// GetIntConfig looks up an integer config value with stacking semantics.
// For stacking keys, values from wisp and bead layers ADD to the base.
// For non-stacking keys, uses override semantics.
func (r *Rig) GetIntConfig(key string) int {
	townRoot := filepath.Dir(r.Path)

	// Check if this key uses stacking semantics
	if !StackingKeys[key] {
		// Override semantics: return first non-nil
		result := r.GetConfig(key)
		return CoerceInt(result)
	}

	// Stacking semantics: sum up adjustments from all layers

	// Get base value (town or system default)
	base := 0
	if val, ok := SystemDefaults[key]; ok {
		base = CoerceInt(val)
	}

	// Check wisp layer for blocked
	wispCfg := wisp.NewConfig(townRoot, r.Name)
	if wispCfg.IsBlocked(key) {
		return 0 // Blocked returns zero
	}

	// Add bead adjustment
	beadAdj := 0
	if val := r.getBeadLabel(key); val != nil {
		beadAdj = CoerceInt(val)
	}

	// Add wisp adjustment
	wispAdj := 0
	if val := wispCfg.Get(key); val != nil {
		wispAdj = CoerceInt(val)
	}

	return base + beadAdj + wispAdj
}

// GetStringConfig looks up a string config value.
// Returns empty string if not set or blocked.
func (r *Rig) GetStringConfig(key string) string {
	result := r.GetConfig(key)
	if result == nil {
		return ""
	}

	switch v := result.(type) {
	case string:
		return v
	default:
		return ""
	}
}

// getBeadLabel reads a label value from the rig identity bead.
// Returns nil if the rig bead doesn't exist or the label is not set.
func (r *Rig) getBeadLabel(key string) interface{} {
	// Get the rig's beads prefix
	prefix := "gt" // default
	if r.Config != nil && r.Config.Prefix != "" {
		prefix = r.Config.Prefix
	}

	// Construct rig identity bead ID
	rigBeadID := beads.RigBeadIDWithPrefix(prefix, r.Name)

	// Load the bead
	beadsDir := beads.ResolveBeadsDir(r.Path)
	bd := beads.NewWithBeadsDir(r.Path, beadsDir)

	issue, err := bd.Show(rigBeadID)
	if err != nil {
		return nil
	}

	// Parse labels for key:value format
	for _, label := range issue.Labels {
		// Labels are in format "key:value"
		if len(label) > len(key)+1 && label[:len(key)+1] == key+":" {
			return label[len(key)+1:]
		}
	}

	return nil
}

// CoerceInt interprets a stored config value as an integer.
// Returns 0 for nil and for values it cannot interpret.
//
// Bools are accepted because older versions of `gt rig config set` stored a
// value of "1" as boolean true before this package could tell which keys are
// integer-typed (see SystemDefaults); true is read back as 1 and false as 0 so
// those values keep their intended meaning.
func CoerceInt(v interface{}) int {
	switch val := v.(type) {
	case nil:
		return 0
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	case bool:
		if val {
			return 1
		}
		return 0
	case string:
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
		return 0
	default:
		return 0
	}
}
