package metrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// configTestTown is the town the config tests read, so every path the source
// joins is the one the fixture writes.
const configTestTown = "/town"

// configTestPaths are the files the source reads under configTestTown.
func configTestPaths() (settings, rigs, town, daemon string) {
	return filepath.Join(configTestTown, "settings", "config.json"),
		filepath.Join(configTestTown, "mayor", "rigs.json"),
		filepath.Join(configTestTown, "mayor", "town.json"),
		filepath.Join(configTestTown, "mayor", "daemon.json")
}

// configOmPath is one rig's .om.json, in the rig's mayor/rig clone.
func configOmPath(rig string) string {
	return filepath.Join(configTestTown, rig, "mayor", "rig", ".om.json")
}

// configRigsFixture is a mayor/rigs.json registering the named rigs.
func configRigsFixture(names ...string) string {
	var b strings.Builder
	b.WriteString(`{"version":1,"rigs":{`)
	for i, name := range names {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `%q:{}`, name)
	}
	b.WriteString("}}")
	return b.String()
}

// configSettingsFixture is a settings/config.json in the shape the operator's
// town has: a polecat pool with the retired idle_fill still written, and the
// daemon patrol document in the same file (the two-file layout).
const configSettingsFixture = `{
  "type": "town-settings",
  "version": 1,
  "polecat_pool": {
    "idle_fill": true,
    "local_agent": "local-coder-polecat",
    "max_local": 0,
    "max_priority": 4,
    "max_total": 12,
    "seats": {"claude-haiku": {"cap": 2}}
  },
  "daemon": {
    "patrols": {
      "overrun_guard": {"dry_run": false, "enabled": true, "interval": "1m", "nudge_after": "12m", "stop_after": "30m"}
    }
  }
}`

// configTestEnv serves the named files and runs commands through run.
func configTestEnv(t *testing.T, files map[string]string, errs map[string]error, run Runner) Env {
	t.Helper()
	env := envAt(fixedNow())
	env.Town = configTestTown
	env.ReadFile = func(path string) ([]byte, error) {
		if err := errs[path]; err != nil {
			return nil, err
		}
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, fs.ErrNotExist
	}
	if run != nil {
		env.Run = run
	}
	return env
}

// configRigList answers `gt rig list --json` with out, and records nothing
// else.
func configRigList(t *testing.T, out string) Runner {
	t.Helper()
	return func(_ context.Context, argv []string) ([]byte, error) {
		if got := strings.Join(argv, " "); got != "gt rig list --json" {
			t.Errorf("ran %q, want gt rig list --json", got)
		}
		return []byte(out), nil
	}
}

// The group reads the pool, the guard and the gate from the operator's own
// files, and the parked count from `gt rig list --json`.
func TestSourceConfigReadsPoolOverrunGateAndParked(t *testing.T) {
	t.Parallel()
	settings, rigs, _, _ := configTestPaths()
	files := map[string]string{
		settings:                configSettingsFixture,
		rigs:                    configRigsFixture("beads", "gastown"),
		configOmPath("gastown"): `{"threshold": 0.75, "depth": "standard", "timeout": 900}`,
	}
	run := configRigList(t, `[
  {"name": "gastown", "beads_prefix": "gt", "repo_path": "", "status": "operational", "sessions": 1},
  {"name": "beads", "beads_prefix": "be", "repo_path": "", "status": "parked", "sessions": 0}
]`)
	got, err := configSource{}.Collect(context.Background(), configTestEnv(t, files, nil, run))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"pool_max_total":        12,
		"pool_idle_fill":        true,
		"overrun_stop_after_s":  1800.0,
		"parked_rigs":           1,
		"om_threshold_gastown":  0.75,
		"om_threshold_beads":    nil,
		"config_probe_failures": 0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %#v, want %#v", got, want)
	}
}

// A town that configures no pool has no cap to report: the columns are nil,
// not a cap of zero.
func TestSourceConfigMissingPoolLeavesNilColumns(t *testing.T) {
	t.Parallel()
	settings, rigs, _, _ := configTestPaths()
	files := map[string]string{
		settings: `{"type": "town-settings", "version": 1}`,
		rigs:     configRigsFixture("gastown"),
	}
	got, err := configSource{}.Collect(context.Background(), configTestEnv(t, files, nil, configRigList(t, `[]`)))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["pool_max_total"] != nil || got["pool_idle_fill"] != nil || got["overrun_stop_after_s"] != nil {
		t.Errorf("pool = %v/%v, overrun = %v, want nil for a town with no pool or guard",
			got["pool_max_total"], got["pool_idle_fill"], got["overrun_stop_after_s"])
	}
	if got["config_probe_failures"] != 0 {
		t.Errorf("config_probe_failures = %v, want 0: an absent setting is not a failed probe", got["config_probe_failures"])
	}
}

// The guard's stop is the value the guard would run with: the configured one,
// or its own 20m default when the value is unset or unusable. A disabled guard
// has no threshold at all.
func TestSourceConfigOverrunStop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		guard string
		want  any
	}{
		{name: "configured", guard: `{"enabled": true, "stop_after": "9m"}`, want: 540.0},
		{name: "unset falls back to the default", guard: `{"enabled": true}`, want: 1200.0},
		{name: "unusable falls back to the default", guard: `{"enabled": true, "stop_after": "soon"}`, want: 1200.0},
		{name: "zero falls back to the default", guard: `{"enabled": true, "stop_after": "0s"}`, want: 1200.0},
		{name: "disabled is no threshold", guard: `{"enabled": false, "stop_after": "9m"}`, want: nil},
		{name: "no guard", guard: `null`, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings, rigs, _, _ := configTestPaths()
			files := map[string]string{
				settings: `{"type": "town-settings", "version": 1, "daemon": {"patrols": {"overrun_guard": ` + tc.guard + `}}}`,
				rigs:     configRigsFixture("gastown"),
			}
			got, err := configSource{}.Collect(context.Background(), configTestEnv(t, files, nil, configRigList(t, `[]`)))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got["overrun_stop_after_s"] != tc.want {
				t.Errorf("overrun_stop_after_s = %v, want %v", got["overrun_stop_after_s"], tc.want)
			}
		})
	}
}

// On the five-file layout the patrol document is mayor/daemon.json; on the
// two-file one it is the settings file's "daemon" section, and a leftover
// mayor/daemon.json is not the town's config.
func TestSourceConfigReadsTheDaemonDocumentOfTheTownsLayout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		townJSON string
		daemon   string
		want     any
	}{
		{
			name:   "five-file layout reads mayor/daemon.json",
			daemon: `{"type": "daemon-patrol-config", "version": 1, "patrols": {"overrun_guard": {"enabled": true, "stop_after": "30m"}}}`,
			want:   1800.0,
		},
		{
			name:     "two-file layout ignores a leftover daemon.json",
			townJSON: `{"version": 1, "registry": {"version": 1, "rigs": {"gastown": {}}}}`,
			daemon:   `{"type": "daemon-patrol-config", "version": 1, "patrols": {"overrun_guard": {"enabled": true, "stop_after": "30m"}}}`,
			want:     nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings, _, town, daemon := configTestPaths()
			files := map[string]string{
				settings: `{"type": "town-settings", "version": 1}`,
				daemon:   tc.daemon,
			}
			if tc.townJSON != "" {
				files[town] = tc.townJSON
			}
			got, err := configSource{}.Collect(context.Background(), configTestEnv(t, files, nil, configRigList(t, `[]`)))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got["overrun_stop_after_s"] != tc.want {
				t.Errorf("overrun_stop_after_s = %v, want %v", got["overrun_stop_after_s"], tc.want)
			}
		})
	}
}

// A guard document that cannot be read or parsed costs only the guard column:
// the pool and the parked count still answer, and the failure is counted.
func TestSourceConfigUnreadableDaemonDocumentCostsOnlyTheGuard(t *testing.T) {
	t.Parallel()
	settings, rigs, town, daemon := configTestPaths()
	tests := []struct {
		name  string
		files map[string]string
		errs  map[string]error
	}{
		{name: "malformed daemon.json", files: map[string]string{daemon: `{"patrols":`}},
		{name: "unreadable daemon.json", errs: map[string]error{daemon: fs.ErrPermission}},
		{name: "malformed town.json", files: map[string]string{town: `{"registry":`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{
				settings: `{"type": "town-settings", "version": 1, "polecat_pool": {"max_total": 12}}`,
				rigs:     configRigsFixture("gastown"),
			}
			for path, data := range tc.files {
				files[path] = data
			}
			got, err := configSource{}.Collect(context.Background(), configTestEnv(t, files, tc.errs, configRigList(t, `[]`)))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got["overrun_stop_after_s"] != nil {
				t.Errorf("overrun_stop_after_s = %v, want nil for an unreadable guard document", got["overrun_stop_after_s"])
			}
			if got["config_probe_failures"] != 1 {
				t.Errorf("config_probe_failures = %v, want 1", got["config_probe_failures"])
			}
			if got["pool_max_total"] != 12 || got["parked_rigs"] != 0 {
				t.Errorf("pool_max_total = %v, parked_rigs = %v, want 12 and 0 beside the guard gap", got["pool_max_total"], got["parked_rigs"])
			}
		})
	}
}

// A rig with no .om.json, or one with no threshold, has no gate: that is a nil
// value, not a failure. A file that will not read or parse leaves only that
// rig's column nil and increments the failure count.
func TestSourceConfigOmThresholds(t *testing.T) {
	t.Parallel()
	settings, rigs, _, _ := configTestPaths()
	files := map[string]string{
		settings:                    `{"type": "town-settings", "version": 1}`,
		rigs:                        configRigsFixture("bad", "good", "none", "nothreshold", "unreadable"),
		configOmPath("good"):        `{"threshold": 0.6, "depth": "standard"}`,
		configOmPath("nothreshold"): `{"depth": "standard", "timeout": 900}`,
		configOmPath("bad"):         `{"threshold":`,
		configOmPath("unreadable"):  `{"threshold": 0.5}`,
	}
	errs := map[string]error{configOmPath("unreadable"): fs.ErrPermission}
	got, err := configSource{}.Collect(context.Background(), configTestEnv(t, files, errs, configRigList(t, `[]`)))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_threshold_good":        0.6,
		"om_threshold_none":        nil,
		"om_threshold_nothreshold": nil,
		"om_threshold_bad":         nil,
		"om_threshold_unreadable":  nil,
		"config_probe_failures":    2,
	}
	for name, value := range want {
		if !reflect.DeepEqual(got[name], value) {
			t.Errorf("%s = %#v, want %#v", name, got[name], value)
		}
	}
}

// The parked count costs its own column: a `gt rig list` that fails, prints
// nothing, prints prose or prints a broken array leaves parked_rigs nil and
// counts the failure, and every other column still fills.
func TestSourceConfigRigListFailureLeavesOnlyParkedNil(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  Runner
	}{
		{name: "command fails", run: func(context.Context, []string) ([]byte, error) {
			return nil, errors.New("gt: command failed")
		}},
		{name: "prints nothing", run: configRigList(t, "")},
		{name: "prints prose", run: configRigList(t, "No rigs configured.\n")},
		{name: "prints a broken array", run: configRigList(t, `[{"name":`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings, rigs, _, _ := configTestPaths()
			files := map[string]string{
				settings:                configSettingsFixture,
				rigs:                    configRigsFixture("gastown"),
				configOmPath("gastown"): `{"threshold": 0.75}`,
			}
			got, err := configSource{}.Collect(context.Background(), configTestEnv(t, files, nil, tc.run))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if got["parked_rigs"] != nil {
				t.Errorf("parked_rigs = %v, want nil", got["parked_rigs"])
			}
			if got["config_probe_failures"] != 1 {
				t.Errorf("config_probe_failures = %v, want 1", got["config_probe_failures"])
			}
			for _, column := range []string{"pool_max_total", "overrun_stop_after_s", "om_threshold_gastown"} {
				if got[column] == nil {
					t.Errorf("%s = nil, want it filled while only gt rig list failed", column)
				}
			}
		})
	}
}

// A `gt rig list` that never returns is cut at the source's own deadline; the
// caller's context stays alive, so the deadline that freed it can only be the
// probe's. Nothing sleeps: the wait is the 20 ms deadline.
func TestSourceConfigHungRigListIsCutAtItsOwnDeadline(t *testing.T) {
	t.Parallel()
	settings, rigs, _, _ := configTestPaths()
	files := map[string]string{
		settings:                configSettingsFixture,
		rigs:                    configRigsFixture("gastown"),
		configOmPath("gastown"): `{"threshold": 0.75}`,
	}
	env := configTestEnv(t, files, nil, func(ctx context.Context, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	caller, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	src := configSource{rigListTimeout: 20 * time.Millisecond}

	got, err := src.Collect(caller, env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if caller.Err() != nil {
		t.Fatalf("the caller's context ended (%v): the probe's own deadline did not free the command", caller.Err())
	}
	if got["parked_rigs"] != nil || got["config_probe_failures"] != 1 {
		t.Errorf("parked_rigs = %v, config_probe_failures = %v, want nil and 1", got["parked_rigs"], got["config_probe_failures"])
	}
	for _, column := range []string{"pool_max_total", "overrun_stop_after_s", "om_threshold_gastown"} {
		if got[column] == nil {
			t.Errorf("%s = nil, want it filled while only gt rig list hung", column)
		}
	}
}

// An unreadable rig registry costs the rig columns and nothing else: the pool,
// the guard and the parked count do not read it.
func TestSourceConfigUnreadableRigRegistryCostsTheRigColumns(t *testing.T) {
	t.Parallel()
	settings, rigs, _, _ := configTestPaths()
	files := map[string]string{
		settings: configSettingsFixture,
		rigs:     `{"rigs":`,
	}
	got, err := configSource{}.Collect(context.Background(), configTestEnv(t, files, nil, configRigList(t, `[]`)))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got["config_probe_failures"] != 1 {
		t.Errorf("config_probe_failures = %v, want 1 for the registry", got["config_probe_failures"])
	}
	if got["pool_max_total"] != 12 || got["parked_rigs"] != 0 {
		t.Errorf("pool_max_total = %v, parked_rigs = %v, want 12 and 0 beside the registry gap", got["pool_max_total"], got["parked_rigs"])
	}
}

// settings/config.json is the group's input: without a readable one there is
// no policy to report, so the group is null rather than a row of invented
// defaults. The source never panics.
func TestSourceConfigMissingOrMalformedSettingsIsANilGroup(t *testing.T) {
	t.Parallel()
	settings, _, _, _ := configTestPaths()
	tests := []struct {
		name  string
		files map[string]string
		errs  map[string]error
	}{
		{name: "missing", files: nil},
		{name: "malformed", files: map[string]string{settings: `{"polecat_pool":`}},
		{name: "unreadable", errs: map[string]error{settings: fs.ErrPermission}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := configSource{}.Collect(context.Background(), configTestEnv(t, tc.files, tc.errs, configRigList(t, `[]`)))
			if err == nil {
				t.Fatalf("Collect = %v, want an error so the group is null", got)
			}
			if got != nil {
				t.Fatalf("Collect = %v, want a nil group", got)
			}
		})
	}
}
