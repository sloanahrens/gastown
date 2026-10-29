package config

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonPatrolConfigIsTheDaemonsSchema(t *testing.T) {
	t.Parallel()
	data := []byte(`{"type":"daemon-patrol-config","version":1,
		"heartbeat":{"enabled":true,"interval":"3m"},
		"env":{"K":"V"},
		"patrols":{"dolt_server":{"enabled":true,"port":3307},
		           "scheduled_maintenance":{"enabled":true,"mode":"gc","gc_min_bytes":1},
		           "dolt_remotes":{"interval":"15m"}}}`)
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", data, &cfg); err != nil {
		t.Fatalf("daemon.json with daemon-only patrols = %v", err)
	}
	if cfg.Patrols.DoltServer == nil || cfg.Patrols.DoltServer.Port != 3307 || cfg.Env["K"] != "V" {
		t.Fatalf("decoded %+v", cfg.Patrols)
	}
	if cfg.Patrols.Count() != 2 {
		t.Errorf("Count = %d, want 2 (retired dolt_remotes not counted)", cfg.Patrols.Count())
	}
	out, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"dolt_remotes":{"interval":"15m"}`) {
		t.Errorf("rewrite dropped the retired key: %s", out)
	}
}

// The dolt_server knobs are durations written as strings, like their siblings
// in this file. They used to be time.Duration fields, which rejected "5s"
// outright under strict decoding and read a bare number as nanoseconds
// (gt-gqftr).
func TestDoltServerConfig_DurationStrings(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"dolt_server":{"enabled":true,"restart_delay":"7s",` +
		`"max_restart_delay":"3m","restart_window":"20m",` +
		`"healthy_reset_interval":"2m","health_check_interval":"45s"}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("duration strings = %v", err)
	}
	ds := cfg.Patrols.DoltServer
	for _, tt := range []struct {
		key  string
		got  time.Duration
		want time.Duration
	}{
		{"restart_delay", ds.RestartDelay(), 7 * time.Second},
		{"max_restart_delay", ds.MaxRestartDelay(), 3 * time.Minute},
		{"restart_window", ds.RestartWindow(), 20 * time.Minute},
		{"healthy_reset_interval", ds.HealthyResetInterval(), 2 * time.Minute},
		{"health_check_interval", ds.HealthCheckInterval(), 45 * time.Second},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.key, tt.got, tt.want)
		}
	}

	// A bare number is a type error, not a nanosecond count: 5 as 5ns would
	// read as a restart with no delay at all.
	err := DecodeJSONFile("daemon.json",
		[]byte(`{"patrols":{"dolt_server":{"enabled":true,"restart_delay":5}}}`),
		&DaemonPatrolConfig{})
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf(`restart_delay: 5 = %v, want ErrUnparseable`, err)
	}
}

// auto_restart is read only when the key is present: absent means the manager
// restarts the server, and false leaves a crashed server down for the operator
// (gt-gqftr).
func TestDoltServerConfig_AutoRestartKey(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		key  string
		want bool
	}{
		{"", true},
		{`,"auto_restart":true`, true},
		{`,"auto_restart":false`, false},
	} {
		body := `{"patrols":{"dolt_server":{"enabled":true` + tt.key + `}}}`
		var cfg DaemonPatrolConfig
		if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
			t.Fatalf("%s = %v", body, err)
		}
		if got := cfg.Patrols.DoltServer.AutoRestartEnabled(); got != tt.want {
			t.Errorf("%s: AutoRestartEnabled = %v, want %v", body, got, tt.want)
		}
	}
}

func TestDaemonPatrolConfigDecodesRebuildOM(t *testing.T) {
	t.Parallel()
	var cfg DaemonPatrolConfig
	body := `{"patrols":{"rebuild_om":{"enabled":true,"interval":"30m"}}}`
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("patrols.rebuild_om = %v", err)
	}
	if got := cfg.Patrols.RebuildOM; got == nil || !got.Enabled || got.Interval != "30m" {
		t.Fatalf("decoded %+v", got)
	}
	// Absent is nil, which the daemon reads as off.
	var empty DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{}}`), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Patrols.RebuildOM != nil {
		t.Errorf("an absent rebuild_om decoded as %+v", empty.Patrols.RebuildOM)
	}
}

// TestPatrolMinIntervalDecodesAndDefaults pins the optional key a patrol that
// coalesces forced runs reads: strict decoding accepts it, an absent or
// unusable value yields the patrol's own fallback, and a nil patrol yields it
// too.
func TestPatrolMinIntervalDecodesAndDefaults(t *testing.T) {
	t.Parallel()
	var cfg DaemonPatrolConfig
	body := `{"patrols":{"rebuild_gt":{"enabled":true,"interval":"1h","min_interval":"45m"}}}`
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("patrols.rebuild_gt = %v", err)
	}
	if got := cfg.Patrols.RebuildGT.MinIntervalOrDefault(30 * time.Minute); got != 45*time.Minute {
		t.Errorf("configured min_interval = %v, want 45m", got)
	}

	fallback := 30 * time.Minute
	for _, p := range []*PatrolConfig{
		nil,
		{},
		{MinInterval: "not a duration"},
		{MinInterval: "-5m"},
		{MinInterval: "0s"},
	} {
		if got := p.MinIntervalOrDefault(fallback); got != fallback {
			t.Errorf("%+v: min_interval = %v, want the fallback %v", p, got, fallback)
		}
	}
}

func TestDaemonPatrolConfigRejectsUnknownPatrolKeys(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"patrols":{"handler":{"enabled":false,"bogus":1}}}`,
		`{"patrols":{"custom":{"enabled":true}}}`,
		`{"heartbeet":{"enabled":true}}`,
	} {
		err := DecodeJSONFile("daemon.json", []byte(body), &DaemonPatrolConfig{})
		if !errors.Is(err, ErrUnparseable) {
			t.Errorf("%s = %v, want ErrUnparseable", body, err)
		}
	}
}

// restart_tracker is retired (gt-gqftr): nothing reads it, but a daemon.json
// that still carries the block must keep loading rather than stop the daemon,
// and the block survives a rewrite verbatim.
func TestDaemonPatrolConfigDecodesRetiredRestartTracker(t *testing.T) {
	t.Parallel()
	block := `{"initial_backoff":"30s","crash_loop_count":5}`
	var cfg DaemonPatrolConfig
	err := DecodeJSONFile("daemon.json",
		[]byte(`{"patrols":{"restart_tracker":`+block+`}}`), &cfg)
	if err != nil {
		t.Fatalf("retired restart_tracker = %v, want it to decode", err)
	}
	if got := string(cfg.Patrols.RestartTracker); got != block {
		t.Errorf("restart_tracker = %s, want it kept verbatim as %s", got, block)
	}
}

// attention.jev_triage is retired (gt-3ey5j): the triage it switched on was
// replaced by the attention battery, so a daemon.json that still carries the
// block must keep loading, and the block survives a rewrite verbatim.
func TestDaemonPatrolConfigDecodesRetiredAttentionJevTriage(t *testing.T) {
	t.Parallel()
	block := `{"jev_triage":{"enabled":true,"model":"jev-1.13.0","confidence_floor":0.7,"max_spend_usd":5}}`
	var cfg DaemonPatrolConfig
	err := DecodeJSONFile("daemon.json",
		[]byte(`{"type":"daemon-patrol-config","version":1,"attention":`+block+`}`), &cfg)
	if err != nil {
		t.Fatalf("retired attention.jev_triage = %v, want it to decode", err)
	}
	if got := string(cfg.Attention); got != block {
		t.Errorf("attention = %s, want it kept verbatim as %s", got, block)
	}
}

// A dolt_server duration that is malformed or not positive reads as the
// default, like the worktree_cleanup durations: a zero restart_delay would
// restart with no backoff at all, and an unset knob must not differ from a bad
// one per caller (gt-gqftr).
func TestDoltServerConfig_BadDurationReadsAsDefault(t *testing.T) {
	t.Parallel()
	knobs := []struct {
		key  string
		read func(*DoltServerConfig) time.Duration
		def  time.Duration
	}{
		{"restart_delay", (*DoltServerConfig).RestartDelay, DefaultDoltRestartDelay},
		{"max_restart_delay", (*DoltServerConfig).MaxRestartDelay, DefaultDoltMaxRestartDelay},
		{"restart_window", (*DoltServerConfig).RestartWindow, DefaultDoltRestartWindow},
		{"healthy_reset_interval", (*DoltServerConfig).HealthyResetInterval, DefaultDoltHealthyResetInterval},
		{"health_check_interval", (*DoltServerConfig).HealthCheckInterval, DefaultDoltHealthCheckInterval},
	}
	for _, k := range knobs {
		for _, value := range []string{`""`, `"soon"`, `"5"`, `"0s"`, `"-3s"`} {
			body := `{"patrols":{"dolt_server":{"enabled":true,"` + k.key + `":` + value + `}}}`
			var cfg DaemonPatrolConfig
			if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
				t.Fatalf("%s = %v", body, err)
			}
			if got := k.read(cfg.Patrols.DoltServer); got != k.def {
				t.Errorf("%s = %v, want the default %v", body, got, k.def)
			}
		}
		// Absent block: the accessors answer on a nil receiver too.
		if got := k.read(nil); got != k.def {
			t.Errorf("nil dolt_server %s = %v, want the default %v", k.key, got, k.def)
		}
	}
}

// The witness, deacon and patrol_watchdog keys are retired (gt-4k3fj.6.1):
// the live mayor/daemon.json still carries them, so they must decode under
// strict decoding, whatever they hold, and survive a rewrite verbatim.
func TestDaemonPatrolConfigDecodesRetiredRoleKeys(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"witness":{"enabled":true,"agent":"witness","disabled_rigs":["gastown"]},` +
		`"deacon":{"enabled":false,"agent":"deacon"},"patrol_watchdog":{"enabled":true,"cadence":"10m","nudge":false}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Patrols.Count() != 0 {
		t.Errorf("Count() = %d, want 0: retired keys are not patrols", cfg.Patrols.Count())
	}
	if cfg.Patrols.RolePatrol("witness") != nil || cfg.Patrols.RolePatrol("deacon") != nil {
		t.Error("RolePatrol returned an entry for a retired role")
	}
	out, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"disabled_rigs":["gastown"]`, `"deacon":{"enabled":false,"agent":"deacon"}`, `"cadence":"10m"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rewrite dropped %s: %s", want, out)
		}
	}
}

// patrols.mayor and patrols.mayor_dispatch are retired with the mayor role
// (gt-rwp7z.3): a daemon.json (or the "daemon" section of settings/config.json)
// that still carries them is tolerated — it decodes without error, the keys
// have no effect, and a rewrite keeps them verbatim.
func TestDaemonPatrolConfigDecodesRetiredMayorKeys(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"mayor":{"enabled":false},` +
		`"mayor_dispatch":{"enabled":false,"interval":"5m"},` +
		`"spec_dispatch":{"enabled":true}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Patrols.Count() != 1 || cfg.Patrols.SpecDispatch == nil {
		t.Errorf("retired keys read as patrols: Count() = %d", cfg.Patrols.Count())
	}
	out, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"mayor":{"enabled":false}`, `"mayor_dispatch":{"enabled":false,"interval":"5m"}`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rewrite dropped %s: %s", want, out)
		}
	}
}

// patrols.steward and patrols.steward_plan are retired with the steward job
// runner (gt-4rizh): the live town's settings/config.json still carries a
// daemon.patrols.steward block, so it must decode without an error and without
// a warning, have no effect, and survive a rewrite verbatim.
func TestDaemonPatrolConfigDecodesRetiredStewardKeys(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"steward":{"enabled":false,"interval":"60s","mode":"shadow"},` +
		`"steward_plan":{"enabled":false,"max_jobs":1},` +
		`"spec_dispatch":{"enabled":true}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Patrols.Count() != 1 || cfg.Patrols.SpecDispatch == nil {
		t.Errorf("retired keys read as patrols: Count() = %d", cfg.Patrols.Count())
	}
	out, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"steward":{"enabled":false,"interval":"60s","mode":"shadow"}`, `"steward_plan":{"enabled":false,"max_jobs":1}`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rewrite dropped %s: %s", want, out)
		}
	}

	// The two-file layout puts the same document in settings/config.json's
	// "daemon" section, so the block has to load through TownSettings too.
	var ts TownSettings
	settings := `{"type":"town-settings","version":1,` +
		`"daemon":{"patrols":{"steward":{"enabled":false},"steward_plan":{"enabled":false}}}}`
	if err := DecodeJSONFile("settings/config.json", []byte(settings), &ts); err != nil {
		t.Fatalf("decode settings/config.json with a daemon.patrols.steward block: %v", err)
	}
	if ts.Daemon == nil {
		t.Fatal("the daemon section did not load")
	}
}

// hookless_agent and max_hookless are retired with the hookless seat class
// (gt-4k3fj.8.7): a spec_dispatch block that still carries them decodes and
// keeps them on a rewrite.
func TestDaemonPatrolConfigDecodesRetiredHooklessKeys(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"spec_dispatch":{"enabled":true,"hookless_agent":"local-coder","max_hookless":2}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"hookless_agent":"local-coder","max_hookless":2`) {
		t.Errorf("rewrite dropped the retired keys: %s", out)
	}
}

// events_prune (gt-ori5j) decodes strictly with its own fields, and a
// daemon.json without it still decodes (the daemon then runs it on defaults).
func TestDaemonPatrolConfigDecodesEventsPrune(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"events_prune":{"enabled":true,"interval":"1h","max_age":"168h","max_bytes":16777216}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := EventsPruneConfig{Enabled: true, IntervalStr: "1h", MaxAgeStr: "168h", MaxBytes: 16777216}
	if cfg.Patrols.EventsPrune == nil || *cfg.Patrols.EventsPrune != want {
		t.Errorf("events_prune = %+v, want %+v", cfg.Patrols.EventsPrune, want)
	}

	var absent DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"handler":{"enabled":true}}}`), &absent); err != nil {
		t.Fatalf("decode without events_prune: %v", err)
	}
	if absent.Patrols.EventsPrune != nil {
		t.Errorf("absent key decoded as %+v", absent.Patrols.EventsPrune)
	}
}

// resource_sampler (gt-qqwey) decodes with the patrol schema's fields, and a
// daemon.json without it still decodes (the daemon then runs it on defaults).
func TestDaemonPatrolConfigDecodesResourceSampler(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"resource_sampler":{"enabled":true,"interval":"10s"}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := cfg.Patrols.ResourceSampler; got == nil || !got.Enabled || got.Interval != "10s" {
		t.Errorf("resource_sampler = %+v, want enabled with interval 10s", got)
	}

	var absent DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"handler":{"enabled":true}}}`), &absent); err != nil {
		t.Fatalf("decode without resource_sampler: %v", err)
	}
	if absent.Patrols.ResourceSampler != nil {
		t.Errorf("absent key decoded as %+v", absent.Patrols.ResourceSampler)
	}
}

// overseer_report (gt-a6fg7) decodes strictly with its own fields, and a
// config without it leaves the patrol unset, which the daemon reads as off.
func TestDaemonPatrolConfigDecodesOverseerReport(t *testing.T) {
	t.Parallel()
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"overseer_report":{"enabled":true,"interval":"15m"}}}`), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := OverseerReportConfig{Enabled: true, IntervalStr: "15m"}
	if cfg.Patrols.OverseerReport == nil || *cfg.Patrols.OverseerReport != want {
		t.Errorf("overseer_report = %+v, want %+v", cfg.Patrols.OverseerReport, want)
	}

	var absent DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"handler":{"enabled":true}}}`), &absent); err != nil {
		t.Fatalf("decode without overseer_report: %v", err)
	}
	if absent.Patrols.OverseerReport != nil {
		t.Errorf("absent key decoded as %+v", absent.Patrols.OverseerReport)
	}

	var typo DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"overseer_report":{"enabled":true,"intervall":"15m"}}}`), &typo); err == nil {
		t.Error("an unknown overseer_report field decoded without error")
	}
}

// TestForgejoTokenDir pins the landing worker's token-directory resolution
// (gt-fn9e6.3): the configured directory wins, and an unconfigured worker
// falls back to the host default. Tokens are never config values, so the only
// thing daemon.json may name is this directory.
func TestForgejoTokenDir(t *testing.T) {
	t.Parallel()

	t.Run("configured directory wins", func(t *testing.T) {
		t.Parallel()
		cfg := &LandingWorkerConfig{Forgejo: &ForgejoWorkerConfig{TokenDir: "/srv/gt/tokens"}}
		got, err := cfg.ForgejoTokenDir()
		if err != nil {
			t.Fatalf("ForgejoTokenDir: %v", err)
		}
		if got != "/srv/gt/tokens" {
			t.Errorf("ForgejoTokenDir() = %q, want the configured directory", got)
		}
	})

	t.Run("unconfigured worker falls back to the host default", func(t *testing.T) {
		t.Parallel()
		want, err := DefaultForgejoTokenDir()
		if err != nil {
			t.Fatalf("DefaultForgejoTokenDir: %v", err)
		}
		for name, cfg := range map[string]*LandingWorkerConfig{
			"nil patrol config":   nil,
			"no forgejo block":    {},
			"empty forgejo block": {Forgejo: &ForgejoWorkerConfig{}},
		} {
			got, err := cfg.ForgejoTokenDir()
			if err != nil {
				t.Errorf("%s: ForgejoTokenDir: %v", name, err)
				continue
			}
			if got != want {
				t.Errorf("%s: ForgejoTokenDir() = %q, want the default %q", name, got, want)
			}
		}
		if want == "" {
			t.Error("DefaultForgejoTokenDir() is empty")
		}
	})
}

// TestForgejoTokenDirDefault drives the default's environment through the
// injectable core: XDG_CONFIG_HOME/gt when set, else ~/.config/gt. Reading
// the real environment here would need t.Setenv, which the unit tier bans.
func TestForgejoTokenDirDefault(t *testing.T) {
	t.Parallel()

	const home = "/home/sloan"
	homeDir := func() (string, error) { return home, nil }

	got, err := forgejoTokenDir(func(string) string { return "/xdg" }, homeDir)
	if err != nil {
		t.Fatalf("forgejoTokenDir(xdg): %v", err)
	}
	if got != "/xdg/gt" {
		t.Errorf("with XDG_CONFIG_HOME = %q, want /xdg/gt", got)
	}

	got, err = forgejoTokenDir(func(string) string { return "" }, homeDir)
	if err != nil {
		t.Fatalf("forgejoTokenDir(home): %v", err)
	}
	if want := filepath.Join(home, ".config", "gt"); got != want {
		t.Errorf("without XDG_CONFIG_HOME = %q, want %q", got, want)
	}

	boom := errors.New("no home")
	if _, err := forgejoTokenDir(func(string) string { return "" },
		func() (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Errorf("forgejoTokenDir(home error) = %v, want the home error", err)
	}
}

// TestDiskMaintenanceConfigRefusesWhatThePatrolCannotActOn: the design's block
// decodes as written, and a value the patrol cannot act on is an error when the
// document is decoded rather than a default later. A mistyped mode must not
// read as a dry run or as an apply.
func TestDiskMaintenanceConfigRefusesWhatThePatrolCannotActOn(t *testing.T) {
	t.Parallel()

	var cfg PatrolsConfig
	design := `{"disk_maintenance": {"enabled": true, "interval": "daily", "window": "04:30", "mode": "apply"}}`
	if err := json.Unmarshal([]byte(design), &cfg); err != nil {
		t.Fatalf("the design's block does not decode: %v", err)
	}
	dm := cfg.DiskMaintenance
	if dm == nil || !dm.Enabled || dm.Interval != "daily" || dm.Window != "04:30" || dm.Mode != "apply" {
		t.Fatalf("decoded %+v, want the block as written", dm)
	}

	bad := map[string]string{
		"not an object":    `{"disk_maintenance": "on"}`,
		"unknown mode":     `{"disk_maintenance": {"mode": "applyy"}}`,
		"unknown interval": `{"disk_maintenance": {"interval": "hourly"}}`,
		"malformed window": `{"disk_maintenance": {"window": "4:30pm"}}`,
		"unknown key":      `{"disk_maintenance": {"enable": true}}`,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			if err := json.Unmarshal([]byte(doc), &p); err == nil {
				t.Fatalf("%s decoded without an error: %+v", name, p.DiskMaintenance)
			}
		})
	}
}

// The daemon.supervisor setting selects the service manager a provisioned
// daemon runs under (docs/plans/2026-10-28-daemon-container-design.md §5): the
// three kinds, and unset keeps the host's own detection.
func TestDaemonPatrolConfig_SupervisorKind(t *testing.T) {
	t.Parallel()

	// The constants A3's compose file and A2's detection are built on; a
	// rename here has to reach them.
	if ContainerModeEnv != "GT_DAEMON_SUPERVISOR" {
		t.Errorf("ContainerModeEnv = %q, want GT_DAEMON_SUPERVISOR", ContainerModeEnv)
	}
	if ContainerNameEnv != "GT_DAEMON_CONTAINER_NAME" {
		t.Errorf("ContainerNameEnv = %q, want GT_DAEMON_CONTAINER_NAME", ContainerNameEnv)
	}
	if SupervisorContainer != "container" || SupervisorLaunchd != "launchd" || SupervisorSystemd != "systemd" {
		t.Errorf("supervisor kinds = %q, %q, %q", SupervisorLaunchd, SupervisorSystemd, SupervisorContainer)
	}

	for _, tt := range []struct{ name, value string }{
		{"launchd", SupervisorLaunchd},
		{"systemd", SupervisorSystemd},
		{"container", SupervisorContainer},
		{"unset", ""},
	} {
		value := tt.value
		body := `{}`
		if value != "" {
			body = `{"supervisor":"` + value + `"}`
		}
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cfg DaemonPatrolConfig
			if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
				t.Fatalf("decode %s = %v", body, err)
			}
			if got, err := cfg.SupervisorKind(); err != nil || got != value {
				t.Fatalf("SupervisorKind() = %q, %v; want %q, nil", got, err, value)
			}
			// The rewrite carries the operator's value, so the next load reads
			// the same kind.
			out, err := json.Marshal(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			hasKey := strings.Contains(string(out), `"supervisor"`)
			if hasKey != (value != "") {
				t.Fatalf("rewrite %s carries the key = %v, want %v", out, hasKey, value != "")
			}
			var reloaded DaemonPatrolConfig
			if err := DecodeJSONFile("daemon.json", out, &reloaded); err != nil {
				t.Fatalf("reload %s = %v", out, err)
			}
			if got, err := reloaded.SupervisorKind(); err != nil || got != value {
				t.Errorf("after the rewrite SupervisorKind() = %q, %v; want %q, nil", got, err, value)
			}
		})
	}
}

func TestDaemonPatrolConfig_SupervisorKindRefusesUnknown(t *testing.T) {
	t.Parallel()
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"supervisor":"kube"}`), &cfg); err != nil {
		t.Fatalf("decode = %v", err)
	}
	got, err := cfg.SupervisorKind()
	if err == nil {
		t.Fatalf("SupervisorKind() = %q, nil; want an error", got)
	}
	if got != "" {
		t.Errorf("SupervisorKind() kind = %q beside the error, want empty", got)
	}
	for _, want := range []string{`"kube"`, SupervisorLaunchd, SupervisorSystemd, SupervisorContainer} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestDaemonPatrolConfig_ContainerNames(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		body        string
		wantName    string
		wantProject string
	}{
		{"both unset", `{}`, "gt-town-daemon-1", "gt-town"},
		{"both set", `{"container_name":"gt-scratch-daemon-1","compose_project":"gt-scratch"}`,
			"gt-scratch-daemon-1", "gt-scratch"},
		{"only the name", `{"container_name":"gt-scratch-daemon-1"}`,
			"gt-scratch-daemon-1", "gt-town"},
		{"only the project", `{"compose_project":"gt-scratch"}`,
			"gt-town-daemon-1", "gt-scratch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cfg DaemonPatrolConfig
			if err := DecodeJSONFile("daemon.json", []byte(tt.body), &cfg); err != nil {
				t.Fatalf("decode %s = %v", tt.body, err)
			}
			if got := cfg.ContainerName(); got != tt.wantName {
				t.Errorf("ContainerName() = %q, want %q", got, tt.wantName)
			}
			if got := cfg.ComposeProject(); got != tt.wantProject {
				t.Errorf("ComposeProject() = %q, want %q", got, tt.wantProject)
			}
		})
	}
}

// A town whose settings omit "daemon" hands the callers a nil config; it keeps
// the host's detection and the container defaults rather than panicking.
func TestDaemonPatrolConfig_NilConfigKeepsDetectionAndDefaults(t *testing.T) {
	t.Parallel()
	var cfg *DaemonPatrolConfig
	if got, err := cfg.SupervisorKind(); got != "" || err != nil {
		t.Errorf("SupervisorKind() = %q, %v; want the empty kind and no error", got, err)
	}
	if got := cfg.ContainerName(); got != DefaultContainerName {
		t.Errorf("ContainerName() = %q, want %q", got, DefaultContainerName)
	}
	if got := cfg.ComposeProject(); got != DefaultComposeProject {
		t.Errorf("ComposeProject() = %q, want %q", got, DefaultComposeProject)
	}
}

// A container_name and compose_project are set together or not at all, and each
// must have the shape docker and compose accept: the container supervisor
// builds argv from both, and a value that reads as a flag or a path would reach
// it unchecked otherwise.
func TestDaemonPatrolConfig_ValidateContainerNames(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{"both unset", `{}`, ""},
		{"both set", `{"container_name":"gt-scratch-daemon-1","compose_project":"gt-scratch"}`, ""},
		{"only the name", `{"container_name":"gt-scratch-daemon-1"}`, "set both or neither"},
		{"only the project", `{"compose_project":"gt-scratch"}`, "set both or neither"},
		{"name leads with a dash", `{"container_name":"-rm","compose_project":"gt-scratch"}`, `daemon.container_name "-rm"`},
		{"name has a space", `{"container_name":"gt scratch","compose_project":"gt-scratch"}`, `daemon.container_name "gt scratch"`},
		{"name has a slash", `{"container_name":"gt/scratch","compose_project":"gt-scratch"}`, `daemon.container_name "gt/scratch"`},
		{"name is one character", `{"container_name":"g","compose_project":"gt-scratch"}`, `daemon.container_name "g"`},
		{"project is uppercase", `{"container_name":"gt-scratch-daemon-1","compose_project":"Gt-Scratch"}`, `daemon.compose_project "Gt-Scratch"`},
		{"project has a dot", `{"container_name":"gt-scratch-daemon-1","compose_project":"gt.scratch"}`, `daemon.compose_project "gt.scratch"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cfg DaemonPatrolConfig
			if err := DecodeJSONFile("daemon.json", []byte(tt.body), &cfg); err != nil {
				t.Fatalf("decode %s = %v", tt.body, err)
			}
			err := cfg.ValidateContainerNames()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateContainerNames() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateContainerNames() = nil, want an error naming %s", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not name %s", err, tt.wantErr)
			}
		})
	}
}

func TestDaemonPatrolConfig_ValidateContainerNamesNilConfig(t *testing.T) {
	t.Parallel()
	var cfg *DaemonPatrolConfig
	if err := cfg.ValidateContainerNames(); err != nil {
		t.Errorf("ValidateContainerNames() on nil = %v, want nil", err)
	}
}
