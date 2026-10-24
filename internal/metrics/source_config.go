package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// The config group: the operator settings that decide what the town does.
//
// The pool and guard columns come from <town>/settings/config.json, decoded as
// config.TownSettings, the document the config kernel reads
// (internal/townconfig):
//
//	pool_max_total        polecat_pool.max_total. 0 is a town with no total
//	                      cap, which is how internal/config reads an absent or
//	                      zero key (PolecatPool.GetMaxTotal): it is a value,
//	                      not a missing reading. nil when settings carries no
//	                      polecat_pool at all.
//	pool_idle_fill        polecat_pool.idle_fill as written, and nil when it is
//	                      absent. The key is retired (internal/config/types.go
//	                      keeps it verbatim so a rewrite does not drop operator
//	                      data): nothing reads it, so none of its values is
//	                      faked here, but an old setting still describes the
//	                      town that carried it.
//	overrun_stop_after_s  the overrun guard's stop_after in seconds. nil when
//	                      the guard is absent or disabled: a stop that cannot
//	                      fire is not a threshold. An unset or unusable value
//	                      is the guard's own default of 20m, the same fallback
//	                      internal/daemon's overrunDuration makes.
//
// The gate and rig columns come from the rig registry and each rig's own
// files:
//
//	om_threshold_<rig>    the "threshold" in <rig>/mayor/rig/.om.json, the
//	                      bar internal/land's OMThreshold reads from the tree
//	                      under review. The clone's copy is the one
//	                      internal/rig's ReadRepoFile falls back to; reading
//	                      main's copy is a git call this hourly snapshot does
//	                      not pay for. nil when the rig ships no .om.json or
//	                      the file carries no threshold: such a rig has no
//	                      gate, which is not a failed reading.
//	parked_rigs           rigs whose `gt rig list --json` status is "parked".
//	                      Docked rigs are a global shutdown, not a park, and
//	                      are not counted.
//
//	config_probe_failures columns left nil because their input could not be
//	                      read or parsed: a rig's .om.json, the rig registry,
//	                      or `gt rig list`. 0 when every probe answered.
//
// settings/config.json is the group's input, so a missing, unreadable or
// unparseable one is an error and the group is null: without it there is no
// operator policy to report. The daemon patrol document is the settings
// file's "daemon" section on the two-file layout and mayor/daemon.json on the
// five-file one (internal/config/layout.go), so the guard is read from
// whichever layout the town is on; a two-file town with no "daemon" section
// has no guard, and a leftover mayor/daemon.json is not read.
//
// Every other probe costs only its own column: a rig's .om.json that is
// malformed or unreadable leaves that rig's threshold nil and increments
// config_probe_failures, and `gt rig list` that fails,
// prints nothing, prints something that is not an array, or takes longer than
// configRigListTimeout leaves parked_rigs nil and does the same. The rest of
// the group is untouched. `gt rig list` finds the town from the working
// directory, which is the town root when the daemon runs the snapshot, and it
// has no flag to name another.
type configSource struct {
	// rigListTimeout bounds the `gt rig list` probe; zero means
	// configRigListTimeout. Tests set it short to cut a hung command at its
	// own deadline.
	rigListTimeout time.Duration
}

// configSettingsFile is the operator settings document under the town root.
var configSettingsFile = filepath.Join("settings", "config.json")

// configDaemonFile is the five-file layout's daemon patrol document. On the
// two-file layout the same document is settings/config.json's "daemon"
// section (internal/config/layout.go).
var configDaemonFile = filepath.Join("mayor", "daemon.json")

// configRigListTimeout bounds `gt rig list`, so a wedged binary costs the
// parked count and not the group.
const configRigListTimeout = 5 * time.Second

// configOverrunDefaultStop is the overrun guard's default stop_after,
// internal/daemon's defaultOverrunGuardStopAfter. It is mirrored rather than
// imported: internal/daemon is the process this snapshot runs beside, not a
// library a snapshot pulls in, and the value is the guard's contract with its
// operators.
const configOverrunDefaultStop = 20 * time.Minute

func (configSource) Group() string { return "config" }

func (s configSource) Collect(ctx context.Context, env Env) (map[string]any, error) {
	settings, err := configReadSettings(env)
	if err != nil {
		return nil, err
	}
	cols := map[string]any{
		"pool_max_total":        nil,
		"pool_idle_fill":        nil,
		"overrun_stop_after_s":  nil,
		"parked_rigs":           nil,
		"config_probe_failures": 0,
	}
	if pool := settings.PolecatPool; pool != nil {
		cols["pool_max_total"] = pool.GetMaxTotal()
		cols["pool_idle_fill"] = configIdleFill(pool.IdleFill)
	}
	daemon, err := configReadDaemon(env, settings)
	if err != nil {
		return nil, err
	}
	if stop := configOverrunStop(daemon); stop != nil {
		cols["overrun_stop_after_s"] = *stop
	}
	failures := 0
	rigs, err := queueRigs(env)
	if err != nil {
		// The registry names the rigs the threshold columns are for, so an
		// unreadable one costs those columns; the pool and parked probes do
		// not read it and still answer.
		failures++
	} else {
		for _, rig := range rigs {
			value, ok := configOmThreshold(env, rig)
			cols["om_threshold_"+rig] = value
			if !ok {
				failures++
			}
		}
	}
	parked, ok := s.configParkedRigs(ctx, env)
	cols["parked_rigs"] = parked
	if !ok {
		failures++
	}
	cols["config_probe_failures"] = failures
	return cols, nil
}

// configReadSettings reads and decodes settings/config.json.
func configReadSettings(env Env) (*config.TownSettings, error) {
	data, err := env.ReadFile(filepath.Join(env.Town, configSettingsFile))
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", configSettingsFile, err)
	}
	var settings config.TownSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", configSettingsFile, err)
	}
	return &settings, nil
}

// configIdleFill reads the retired idle_fill value as the boolean it was
// written as. A value of another shape is no reading rather than an invented
// one.
func configIdleFill(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

// configReadDaemon returns the daemon patrol document for the town's layout:
// the settings file's "daemon" section on the two-file layout, else
// mayor/daemon.json on the five-file one. No document means no guard, not an
// error.
func configReadDaemon(env Env, settings *config.TownSettings) (*config.DaemonPatrolConfig, error) {
	if settings.Daemon != nil {
		return settings.Daemon, nil
	}
	twoFile, err := configTwoFile(env)
	if err != nil {
		return nil, err
	}
	if twoFile {
		// A two-file town keeps the document in settings/config.json; a
		// leftover mayor/daemon.json is not the town's config (layout.go).
		return nil, nil
	}
	data, err := env.ReadFile(filepath.Join(env.Town, configDaemonFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", configDaemonFile, err)
	}
	var daemon config.DaemonPatrolConfig
	if err := json.Unmarshal(data, &daemon); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", configDaemonFile, err)
	}
	return &daemon, nil
}

// configTwoFile reports whether mayor/town.json carries the registry section,
// the two-file layout's marker (internal/config/layout.go).
func configTwoFile(env Env) (bool, error) {
	data, err := env.ReadFile(filepath.Join(env.Town, config.MachineConfigFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("config: reading %s: %w", config.MachineConfigFile, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return false, fmt.Errorf("config: parsing %s: %w", config.MachineConfigFile, err)
	}
	_, has := top["registry"]
	return has, nil
}

// configOverrunStop is the guard's stop_after in seconds, or nil when the
// guard is absent or disabled. The value is the one the guard itself would
// run with, default included.
func configOverrunStop(daemon *config.DaemonPatrolConfig) *float64 {
	if daemon == nil || daemon.Patrols == nil || daemon.Patrols.OverrunGuard == nil {
		return nil
	}
	guard := daemon.Patrols.OverrunGuard
	if !guard.Enabled {
		return nil
	}
	stop := configOverrunDefaultStop
	if guard.StopAfter != "" {
		if parsed, err := time.ParseDuration(guard.StopAfter); err == nil && parsed > 0 {
			stop = parsed
		}
	}
	seconds := stop.Seconds()
	return &seconds
}

// configOmThreshold reads one rig's om gate threshold. ok is false only when
// the file could not be read or parsed; a rig with no .om.json or no
// threshold has no gate, which is a nil value every rig is allowed.
func configOmThreshold(env Env, rig string) (value any, ok bool) {
	path := filepath.Join(env.Town, rig, "mayor", "rig", ".om.json")
	data, err := env.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	var doc struct {
		Threshold *float64 `json:"threshold"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, false
	}
	if doc.Threshold == nil {
		return nil, true
	}
	return *doc.Threshold, true
}

// configParkedRigs counts the parked rigs in `gt rig list --json`. ok is
// false when the command failed, missed its deadline, printed nothing, or
// printed something that is not the array the command writes.
func (s configSource) configParkedRigs(ctx context.Context, env Env) (value any, ok bool) {
	timeout := s.rigListTimeout
	if timeout <= 0 {
		timeout = configRigListTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := env.Run(callCtx, []string{"gt", "rig", "list", "--json"})
	if err != nil {
		return nil, false
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var rigs []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(trimmed, &rigs); err != nil {
		return nil, false
	}
	parked := 0
	for _, rig := range rigs {
		if strings.EqualFold(rig.Status, "parked") {
			parked++
		}
	}
	return parked, true
}
