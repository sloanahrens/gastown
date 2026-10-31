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
// The pool, guard and gate columns come from the operator's own files, read
// through Env: settings/config.json (config.TownSettings), the guard's daemon
// document (the settings file's "daemon" section on the two-file layout,
// mayor/daemon.json on the five-file one, internal/config/layout.go), and each
// rig's mayor/rig/.om.json. pool_max_total is 0 when the town sets no total
// cap, which is how config.PolecatPool.GetMaxTotal reads it. A nil column means
// no reading: no pool, no guard, or no gate, not a zero.
//
// settings/config.json is the group's input, so without it there is no policy
// to report and the group is null. Any other probe that fails nils only its own
// column and counts in config_probe_failures, so a nil is never a measured zero.
// When the rig registry cannot be read the rig names are unknown, so no
// om_threshold_<rig> column exists for that snapshot; the failure is counted.
//
// parked_rigs counts the rows of `gt rig list --json` whose status is "parked",
// through Env.RunDir. That command finds the town from its working directory
// and has no flag to name another, so the probe starts it in env.Town: the
// snapshot process's own directory names the town it was started in, or none.
type configSource struct {
	// rigListTimeout bounds the `gt rig list` probe; zero means
	// configRigListTimeout. Tests set it short to cut a hung command at its
	// own deadline.
	rigListTimeout time.Duration
}

// configSettingsFile is the operator settings document under the town root.
var configSettingsFile = filepath.Join("settings", "config.json")

// configDaemonFile is the five-file layout's daemon patrol document.
var configDaemonFile = filepath.Join("mayor", "daemon.json")

// configRigListTimeout bounds `gt rig list`, so a wedged binary costs the
// parked count and not the group.
const configRigListTimeout = 5 * time.Second

// configOverrunDefaultStop mirrors internal/daemon's defaultOverrunGuardStopAfter.
// It is copied rather than imported: the daemon is the process this snapshot
// runs beside, not a library it links.
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
	failures := 0
	if pool := settings.PolecatPool; pool != nil {
		cols["pool_max_total"] = pool.GetMaxTotal()
		cols["pool_idle_fill"] = configIdleFill(pool.IdleFill)
	}
	// An unreadable guard document costs only the guard column; the pool, gate
	// and parked probes do not read it.
	daemon, err := configReadDaemon(env, settings)
	if err != nil {
		failures++
	} else if stop := configOverrunStop(daemon); stop != nil {
		cols["overrun_stop_after_s"] = *stop
	}
	rigs, err := queueRigs(env)
	if err != nil {
		// The registry names the rigs the threshold columns are for, so an
		// unreadable one costs those columns; the pool and parked probes do
		// not read it and still answer.
		failures++
	} else {
		for _, rig := range rigs {
			value, ok := configOmThreshold(env, rig.name)
			cols["om_threshold_"+rig.name] = value
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
// error; a document that cannot be read or parsed is an error.
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

// configParkedRigs counts the parked rigs in `gt rig list --json`, run in
// env.Town so the command answers for the snapshot's town. ok is false when
// the command failed, missed its deadline, printed nothing, or printed
// something that is not the array the command writes.
func (s configSource) configParkedRigs(ctx context.Context, env Env) (value any, ok bool) {
	// An empty town is no town: a command run in "" runs in the process's own
	// working directory, which would count the rigs of whatever town the
	// process was started in. That is a failed probe, not a count.
	if env.RunDir == nil || env.Town == "" {
		return nil, false
	}
	timeout := s.rigListTimeout
	if timeout <= 0 {
		timeout = configRigListTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := env.RunDir(callCtx, env.Town, []string{"gt", "rig", "list", "--json"})
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
