package metrics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/models"
	"github.com/steveyegge/gastown/internal/rig"
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
//
// Four columns say what was in force when the snapshot was taken, so a series
// can be cut at the changes that matter without reading the daemon log:
//
//	gt_commit       the build the daemon runs, from daemon/state.json "commit"
//	                (the installer's record of the in-force build)
//	directive_hash  the first 12 hex of the sha256 of the polecat directive
//	                text, resolved from the central layer and its legacy
//	                fallback (config.CentralDirectiveDir; a rig's own layer is
//	                not read, so the column is the town's directive)
//	seat_models     each polecat_pool.seats agent -> the model it runs: the
//	                --model its town-settings agent config names, else the
//	                model family its name carries, else the name itself
//	gate_ids        each rig -> the first 12 hex of the sha256 of its gate
//	                command line (merge_queue presubmit, lint, build and test
//	                commands), from the rig-root config.json floor and the
//	                rig-local settings/config.json override
//
// A missing file is no value, never a measured one: a town with no
// daemon/state.json records gt_commit null, not an empty commit. A file that
// is there and cannot be read or parsed leaves its column null and counts in
// config_probe_failures, the group's record of every probe that answered
// nothing it could trust.
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

// configStateFile is the daemon's state document under the town root. Its
// "commit" is the build the running daemon was started from, which the
// installer reads back when it decides whether an install is needed.
var configStateFile = filepath.Join("daemon", "state.json")

// configRegimeHashLen is how many hex characters the regime columns keep of a
// sha256, the count the system prompt's "Directives: sha256 <12>" evidence line
// prints (gt-uhzl7), so the log's hashes are as short as that line's.
const configRegimeHashLen = 12

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
		"gt_commit":             nil,
		"directive_hash":        nil,
		"seat_models":           nil,
		"gate_ids":              nil,
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
		// Gate ids share the registry's rig names: one rig whose gate config
		// cannot be read leaves the whole map null, because a map missing that
		// rig would read as that rig having no gate.
		value, failed := configGateIDs(env, rigs)
		cols["gate_ids"] = value
		if failed {
			failures++
		}
	}
	if value, failed := configGtCommit(env); failed {
		failures++
	} else {
		cols["gt_commit"] = value
	}
	if value, failed := configDirectiveHash(env); failed {
		failures++
	} else {
		cols["directive_hash"] = value
	}
	cols["seat_models"] = configSeatModels(settings)
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

// configGtCommit reads the in-force build the daemon recorded in
// daemon/state.json: the commit the installer compares against when it decides
// whether an install is due. A missing file or a state that names no commit
// (a daemon that could not resolve its own build) is no value, not a failure;
// a file that is there and cannot be read or parsed is a failed probe.
func configGtCommit(env Env) (value any, failed bool) {
	data, err := env.ReadFile(filepath.Join(env.Town, configStateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		return nil, true
	}
	var state struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, true
	}
	commit := strings.TrimSpace(state.Commit)
	if commit == "" {
		return nil, false
	}
	return commit, false
}

// configDirectiveHash is the polecat directive's identity: the first
// configRegimeHashLen hex of the sha256 of the text the prompt renders, through
// config.ReadRoleDirectiveFrom with Env's reader, so the files, the Changelog
// strip and the join are the reader's own. No directive file at all is no
// value; a file that is there and cannot be read is a failed probe, and so is
// the whole column, because a hash over the files that happened to read would
// name a directive text that is not in force.
func configDirectiveHash(env Env) (value any, failed bool) {
	text, found, err := config.ReadRoleDirectiveFrom(constants.RolePolecat, env.Town, "", env.ReadFile)
	if err != nil {
		return nil, true
	}
	if !found {
		return nil, false
	}
	return configRegimeHash(text), false
}

// configSeatModels maps each seat's agent to the model it runs. The model is
// the --model its town-settings agent config names; an agent with no entry
// there is read from its name, which is how the seats are spelled (a
// claude-haiku seat runs haiku). No pool or no seats is no map.
func configSeatModels(settings *config.TownSettings) any {
	if settings.PolecatPool == nil || len(settings.PolecatPool.Seats) == 0 {
		return nil
	}
	out := make(map[string]string, len(settings.PolecatPool.Seats))
	for agent := range settings.PolecatPool.Seats {
		out[agent] = configSeatModel(settings, agent)
	}
	return out
}

// configSeatModel is one seat's model: the agent config's command line when
// the town defines that agent, else the agent's own name.
func configSeatModel(settings *config.TownSettings, agent string) string {
	cfg := settings.Agents[agent]
	if cfg == nil {
		return models.ModelName([]string{agent})
	}
	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		command = "claude"
	}
	argv := make([]string, 0, len(cfg.Args)+1)
	argv = append(argv, command)
	argv = append(argv, cfg.Args...)
	return models.ModelName(argv)
}

// configGateIDs maps each rig to the identity of its gate recipe: the first
// configRegimeHashLen hex of the sha256 of the gate's command line. A rig whose
// gate config cannot be read leaves the whole map no value: a map that omitted
// it would read as a rig with no gate.
func configGateIDs(env Env, rigs []queueRig) (value any, failed bool) {
	if len(rigs) == 0 {
		return nil, false
	}
	out := make(map[string]string, len(rigs))
	for _, rig := range rigs {
		id, ok, err := configRigGateID(env, rig.name)
		if err != nil {
			return nil, true
		}
		if ok {
			out[rig.name] = id
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, false
}

// configRigGateID hashes one rig's configured gate command line. The two file
// tiers read here are the rig-root config.json floor and the rig-local
// settings/config.json override, resolved through the rig package's precedence
// (rig.ResolveMergeQueueConfigTiers) so this is not a second resolver. Its
// repo-committed .gastown/settings.json tier is read from git and so is passed
// nil, which a rig that only merged content configures shows as no gate id. ok
// is false for a rig with no configured gate command, whose gate is the
// built-in default rather than a recipe this source can name.
func configRigGateID(env Env, rigName string) (id string, ok bool, err error) {
	rigPath := filepath.Join(env.Town, rigName)
	floor, err := configReadRigMergeQueue(env, filepath.Join(rigPath, "config.json"))
	if err != nil {
		return "", false, err
	}
	local, err := configReadRigMergeQueue(env, config.RigSettingsPath(rigPath))
	if err != nil {
		return "", false, err
	}
	mq := rig.ResolveMergeQueueConfigTiers(floor, nil, local)
	if mq == nil {
		return "", false, nil
	}
	text := configGateRecipeText(mq)
	if text == "" {
		return "", false, nil
	}
	return configRegimeHash(text), true, nil
}

// configReadRigMergeQueue reads one rig settings document's merge_queue
// section. A missing file is no section; a file that is there and cannot be
// read or parsed is an error.
func configReadRigMergeQueue(env Env, path string) (*config.MergeQueueConfig, error) {
	data, err := env.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		MergeQueue *config.MergeQueueConfig `json:"merge_queue"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc.MergeQueue, nil
}

// configGateRecipeText is the gate command line the id covers: the steps a
// landing runs, each with the field it comes from, so a command that moves
// from one step to another changes the id. An empty command is no step, which
// is how the default for that step applies. No commands at all is "".
func configGateRecipeText(mq *config.MergeQueueConfig) string {
	var b strings.Builder
	for _, step := range []struct {
		name    string
		command string
	}{
		{"presubmit", mq.PresubmitCommand},
		{"lint", mq.LintCommand},
		{"build", mq.BuildCommand},
		{"test", mq.TestCommand},
	} {
		command := strings.TrimSpace(step.command)
		if command == "" {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\n", step.name, command)
	}
	return b.String()
}

// configRegimeHash is the regime columns' identity form: the first
// configRegimeHashLen hex of sha256(text).
func configRegimeHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:configRegimeHashLen]
}
