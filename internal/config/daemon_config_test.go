package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

func TestDaemonPatrolConfigDecodesRebuildHM(t *testing.T) {
	t.Parallel()
	var cfg DaemonPatrolConfig
	body := `{"patrols":{"rebuild_hm":{"enabled":true,"interval":"30m"}}}`
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("patrols.rebuild_hm = %v", err)
	}
	if got := cfg.Patrols.RebuildHM; got == nil || !got.Enabled || got.Interval != "30m" {
		t.Fatalf("decoded %+v", got)
	}
	// Absent is nil, which the daemon reads as off.
	var empty DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{}}`), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Patrols.RebuildHM != nil {
		t.Errorf("an absent rebuild_hm decoded as %+v", empty.Patrols.RebuildHM)
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

// bird_reports (gt-cbsat) decodes strictly with its own fields, defaults its
// hour, age and reporters, and refuses a value the patrol cannot act on: a
// mistyped setting must not read as "no alarm".
func TestBirdReportsConfigDefaultsAndRefusals(t *testing.T) {
	t.Parallel()

	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"bird_reports":{"enabled":true,"start_hour":7,"max_age":"30h","reporters":["dunlin","overseer"]}}}`), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	br := cfg.Patrols.BirdReports
	if br == nil || !br.Enabled {
		t.Fatalf("bird_reports = %+v, want enabled", br)
	}
	if got := br.StartHourValue(); got != 7 {
		t.Errorf("start_hour = %d, want 7", got)
	}
	if age, err := br.MaxAgeValue(); err != nil || age != 30*time.Hour {
		t.Errorf("max_age = %v, %v; want 30h", age, err)
	}
	if got := br.ReportersValue(); !slices.Equal(got, []string{"dunlin", "overseer"}) {
		t.Errorf("reporters = %v, want the configured pair", got)
	}

	var absent DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"handler":{"enabled":true}}}`), &absent); err != nil {
		t.Fatalf("decode without bird_reports: %v", err)
	}
	if absent.Patrols.BirdReports != nil {
		t.Errorf("absent key decoded as %+v", absent.Patrols.BirdReports)
	}

	// The accessors answer for a block that is not there, so the patrol reads
	// the same defaults whether a town wrote the key or not.
	var none *BirdReportsConfig
	if got := none.StartHourValue(); got != BirdReportsDefaultStartHour {
		t.Errorf("unset start_hour = %d, want the %d default", got, BirdReportsDefaultStartHour)
	}
	if age, err := none.MaxAgeValue(); err != nil || age != 26*time.Hour {
		t.Errorf("unset max_age = %v, %v; want 26h", age, err)
	}
	if got := none.ReportersValue(); !slices.Equal(got, DefaultBirdReporters()) || len(got) != 10 {
		t.Errorf("unset reporters = %v, want the ten defaults", got)
	}

	// 0 is midnight, a real hour, so it must not read as "unset".
	var midnight DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"bird_reports":{"start_hour":0}}}`), &midnight); err != nil {
		t.Fatalf("decode start_hour 0: %v", err)
	}
	if got := midnight.Patrols.BirdReports.StartHourValue(); got != 0 {
		t.Errorf("start_hour 0 read as %d, want 0", got)
	}

	bad := map[string]string{
		"not an object":         `{"bird_reports": "on"}`,
		"unknown key":           `{"bird_reports": {"enable": true}}`,
		"hour past 23":          `{"bird_reports": {"start_hour": 24}}`,
		"negative hour":         `{"bird_reports": {"start_hour": -1}}`,
		"unparseable age":       `{"bird_reports": {"max_age": "soon"}}`,
		"zero age":              `{"bird_reports": {"max_age": "0s"}}`,
		"negative age":          `{"bird_reports": {"max_age": "-1h"}}`,
		"empty reporter":        `{"bird_reports": {"reporters": [""]}}`,
		"dot-dot reporter":      `{"bird_reports": {"reporters": [".."]}}`,
		"separator in reporter": `{"bird_reports": {"reporters": ["a/b"]}}`,
		"backslash in reporter": `{"bird_reports": {"reporters": ["a\\b"]}}`,
		"NUL in reporter":       `{"bird_reports": {"reporters": ["dunlin\u0000"]}}`,
		"reporter listed twice": `{"bird_reports": {"reporters": ["dunlin", "dunlin"]}}`,
		"reporters not a list":  `{"bird_reports": {"reporters": "dunlin"}}`,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			if err := json.Unmarshal([]byte(doc), &p); err == nil {
				t.Fatalf("%s decoded without an error: %+v", name, p.BirdReports)
			}
		})
	}
}

// findings_filer (gt-rws9m) decodes strictly with its own fields, defaults its
// interval and per-pass cap, and refuses a value the patrol cannot act on: a
// typo must not read as a patrol that runs on something else.
func TestFindingsFilerConfigDefaultsAndRefusals(t *testing.T) {
	t.Parallel()

	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"findings_filer":{"enabled":true,"interval":"30m","max_per_run":2}}}`), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ff := cfg.Patrols.FindingsFiler
	if ff == nil || !ff.Enabled {
		t.Fatalf("findings_filer = %+v, want enabled", ff)
	}
	if got, err := ff.IntervalValue(); err != nil || got != 30*time.Minute {
		t.Errorf("interval = %v, %v; want 30m", got, err)
	}
	if got := ff.MaxPerRunValue(); got != 2 {
		t.Errorf("max_per_run = %d, want 2", got)
	}

	var absent DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"handler":{"enabled":true}}}`), &absent); err != nil {
		t.Fatalf("decode without findings_filer: %v", err)
	}
	if absent.Patrols.FindingsFiler != nil {
		t.Errorf("absent key decoded as %+v", absent.Patrols.FindingsFiler)
	}

	// The accessors answer for a block that is not there, so the patrol reads
	// the same defaults whether a town wrote the key or not.
	var none *FindingsFilerConfig
	if got, err := none.IntervalValue(); err != nil || got != FindingsFilerDefaultInterval {
		t.Errorf("unset interval = %v, %v; want %v", got, err, FindingsFilerDefaultInterval)
	}
	if got := none.MaxPerRunValue(); got != FindingsFilerDefaultMaxPerRun {
		t.Errorf("unset max_per_run = %d, want %d", got, FindingsFilerDefaultMaxPerRun)
	}

	bad := map[string]string{
		"not an object":           `{"findings_filer": "on"}`,
		"unknown key":             `{"findings_filer": {"enable": true}}`,
		"unparseable interval":    `{"findings_filer": {"interval": "soon"}}`,
		"zero interval":           `{"findings_filer": {"interval": "0s"}}`,
		"negative interval":       `{"findings_filer": {"interval": "-5m"}}`,
		"zero max_per_run":        `{"findings_filer": {"max_per_run": 0}}`,
		"negative max_per_run":    `{"findings_filer": {"max_per_run": -1}}`,
		"max_per_run not a count": `{"findings_filer": {"max_per_run": "five"}}`,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			if err := json.Unmarshal([]byte(doc), &p); err == nil {
				t.Fatalf("%s decoded without an error: %+v", name, p.FindingsFiler)
			}
		})
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

	const home = "/home/user"
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

// TestHMSyncConfigRefusesWhatThePatrolCannotActOn: the block decodes as
// written, and an interval or key the patrol cannot schedule is an error when
// the document is decoded rather than the default later. A mistyped interval
// must not read as 30m.
func TestHMSyncConfigRefusesWhatThePatrolCannotActOn(t *testing.T) {
	t.Parallel()

	var cfg PatrolsConfig
	design := `{"hm_sync": {"enabled": true, "interval": "15m", "command": "hm"}}`
	if err := json.Unmarshal([]byte(design), &cfg); err != nil {
		t.Fatalf("the block does not decode: %v", err)
	}
	hc := cfg.HMSync
	if hc == nil || !hc.Enabled || hc.Interval != "15m" || hc.Command != "hm" {
		t.Fatalf("decoded %+v, want the block as written", hc)
	}

	bad := map[string]string{
		"not an object":     `{"hm_sync": "on"}`,
		"unknown key":       `{"hm_sync": {"enable": true}}`,
		"not a duration":    `{"hm_sync": {"interval": "every half hour"}}`,
		"zero interval":     `{"hm_sync": {"interval": "0s"}}`,
		"negative interval": `{"hm_sync": {"interval": "-5m"}}`,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			if err := json.Unmarshal([]byte(doc), &p); err == nil {
				t.Fatalf("%s decoded without an error: %+v", name, p.HMSync)
			}
		})
	}
}

// TestHMSyncIntervalDuration: an empty block or one with no interval is the
// 30m default, a set interval is that duration, and a value the patrol cannot
// schedule is refused with its key named rather than read as the default.
func TestHMSyncIntervalDuration(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		block *HMSyncConfig
		want  time.Duration
	}{
		{"no block", nil, 30 * time.Minute},
		{"no interval", &HMSyncConfig{}, 30 * time.Minute},
		{"a set interval", &HMSyncConfig{Interval: "45s"}, 45 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.block.IntervalDuration()
			if err != nil || got != tt.want {
				t.Errorf("IntervalDuration = %v, %v; want %v and no error", got, err, tt.want)
			}
		})
	}
	for _, tt := range []struct{ name, interval string }{
		{"unparseable", "every half hour"},
		{"zero", "0s"},
		{"negative", "-5m"},
	} {
		t.Run("refused: "+tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := (&HMSyncConfig{Interval: tt.interval}).IntervalDuration()
			if err == nil || got != 0 || !strings.Contains(err.Error(), "patrols.hm_sync.interval") {
				t.Errorf("IntervalDuration(%q) = %v, %v; want a refusal naming the key", tt.interval, got, err)
			}
		})
	}
}

// TestDiskMaintenanceConfigRefusesWhatThePatrolCannotActOn: the design's block
// decodes as written, and a value the patrol cannot act on is an error when the
// document is decoded rather than a default later. A mistyped mode must not
// read as a dry run or as an apply.
func TestDiskMaintenanceConfigRefusesWhatThePatrolCannotActOn(t *testing.T) {
	t.Parallel()

	var cfg PatrolsConfig
	design := `{"disk_maintenance": {"enabled": true, "interval": "daily", "window": "04:30", "mode": "apply", "targets": {"registry": {"interval": "2h"}}}}`
	if err := json.Unmarshal([]byte(design), &cfg); err != nil {
		t.Fatalf("the design's block does not decode: %v", err)
	}
	dm := cfg.DiskMaintenance
	if dm == nil || !dm.Enabled || dm.Interval != "daily" || dm.Window != "04:30" || dm.Mode != "apply" {
		t.Fatalf("decoded %+v, want the block as written", dm)
	}
	if got := dm.Targets[DiskMaintenanceTargetRegistry].Interval; got != "2h" {
		t.Errorf("registry interval decoded as %q, want 2h", got)
	}

	bad := map[string]string{
		"not an object":      `{"disk_maintenance": "on"}`,
		"unknown mode":       `{"disk_maintenance": {"mode": "applyy"}}`,
		"unknown interval":   `{"disk_maintenance": {"interval": "hourly"}}`,
		"malformed window":   `{"disk_maintenance": {"window": "4:30pm"}}`,
		"unknown key":        `{"disk_maintenance": {"enable": true}}`,
		"unknown target":     `{"disk_maintenance": {"targets": {"go-build": {"interval": "2h"}}}}`,
		"malformed cadence":  `{"disk_maintenance": {"targets": {"registry": {"interval": "soon"}}}}`,
		"sub-minute cadence": `{"disk_maintenance": {"targets": {"registry": {"interval": "30s"}}}}`,
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

// TestTierSweepRunnerImageRegistrySetting: image_registry is optional, and an
// empty value or a bare host:port decodes. A scheme, a path, whitespace, a
// missing port and a port out of range are each refused, naming the key, so a
// URL typed where a host:port belongs never reaches the daemon (gt-gg9hv.26).
func TestTierSweepRunnerImageRegistrySetting(t *testing.T) {
	t.Parallel()

	const head = `{"backend":"docker","image":"installed","cpus":8,"memory":24,"jobs":4,"output_root":"/tmp/o","context":"c"`
	good := map[string]struct{ block, want string }{
		"no registry":    {head + `}`, ""},
		"empty registry": {head + `,"image_registry":""}`, ""},
		"host and port":  {head + `,"image_registry":"localhost:5001"}`, "localhost:5001"},
	}
	for name, tt := range good {
		t.Run("accepts "+name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			if err := json.Unmarshal([]byte(`{"tier_sweep": {"runner": `+tt.block+`}}`), &p); err != nil {
				t.Fatalf("a runner block with %s does not decode: %v", name, err)
			}
			if got := p.TierSweep.Runner.ImageRegistry; got != tt.want {
				t.Errorf("image_registry = %q, want %q", got, tt.want)
			}
		})
	}

	bad := map[string]string{
		"scheme":            head + `,"image_registry":"http://localhost:5001"}`,
		"path":              head + `,"image_registry":"localhost:5001/v2"}`,
		"whitespace":        head + `,"image_registry":"localhost :5001"}`,
		"no port":           head + `,"image_registry":"localhost"}`,
		"no host":           head + `,"image_registry":":5001"}`,
		"unparseable host":  head + `,"image_registry":"local%zz:5001"}`,
		"port out of range": head + `,"image_registry":"localhost:99999"}`,
	}
	for name, block := range bad {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			err := json.Unmarshal([]byte(`{"tier_sweep": {"runner": `+block+`}}`), &p)
			if err == nil {
				t.Fatalf("decoded image_registry with a %s: %+v", name, p.TierSweep.Runner)
			}
			if !strings.Contains(err.Error(), "runner.image_registry") {
				t.Errorf("error = %v, want it to name runner.image_registry", err)
			}
		})
	}
}

// TestImageRegistryBase: the base URL of a bare image_registry setting is the
// setting under http://. Every consumer builds its URL here, so none of them
// can read the setting as a different registry (gt-vv9ir0).
func TestImageRegistryBase(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ setting, want string }{
		"localhost":     {"localhost:5001", "http://localhost:5001"},
		"address":       {"10.0.0.5:5000", "http://10.0.0.5:5000"},
		"ipv6 loopback": {"[::1]:5001", "http://[::1]:5001"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ImageRegistryBase(tt.setting); got != tt.want {
				t.Errorf("ImageRegistryBase(%q) = %q, want %q", tt.setting, got, tt.want)
			}
		})
	}
}

// image_rigs names the rigs whose green sweep publishes an image ref. It
// defaults to none, so a town that never sets it pushes nothing; a named rig
// is returned; and a name the town does not register is refused with the name,
// because the push builds the rig's repository path from it (gt-gg9hv.61).
func TestTierSweepImageRigs(t *testing.T) {
	t.Parallel()
	known := []string{"gastown", "otherrig"}

	t.Run("defaults to none", func(t *testing.T) {
		t.Parallel()
		var p PatrolsConfig
		if err := json.Unmarshal([]byte(`{"tier_sweep": {"enabled": true}}`), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got, err := p.TierSweep.TierSweepImageRigs(known)
		if err != nil || len(got) != 0 {
			t.Fatalf("TierSweepImageRigs = %v, %v; want none", got, err)
		}
	})

	t.Run("a named rig enables it", func(t *testing.T) {
		t.Parallel()
		var p PatrolsConfig
		if err := json.Unmarshal([]byte(`{"tier_sweep": {"enabled": true, "image_rigs": ["otherrig"]}}`), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got, err := p.TierSweep.TierSweepImageRigs(known)
		if err != nil {
			t.Fatalf("TierSweepImageRigs: %v", err)
		}
		if len(got) != 1 || got[0] != "otherrig" {
			t.Fatalf("TierSweepImageRigs = %v, want [otherrig]", got)
		}
	})

	t.Run("an unknown rig is refused by name", func(t *testing.T) {
		t.Parallel()
		var p PatrolsConfig
		if err := json.Unmarshal([]byte(`{"tier_sweep": {"enabled": true, "image_rigs": ["gastwon"]}}`), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		_, err := p.TierSweep.TierSweepImageRigs(known)
		if err == nil || !strings.Contains(err.Error(), "gastwon") || !strings.Contains(err.Error(), "image_rigs") {
			t.Fatalf("TierSweepImageRigs = %v, want a refusal naming gastwon and image_rigs", err)
		}
	})
}

// A runner block moves the integration and race tiers off the host, so the
// engine and limit fields are required and a key or value no run can use is
// refused at decode, naming the key: a block the daemon half-understands would
// start a container the operator never described (gt-z4ppb).
func TestTierSweepRunnerConfigStrict(t *testing.T) {
	t.Parallel()

	const block = `{"backend": "docker", "image": "gt-polecat:main", "cpus": 8, "memory": 24, "jobs": 4, "output_root": "/tmp/gt-sweep", "context": "colima-gt-sbx", "dind": true}`
	var p PatrolsConfig
	if err := json.Unmarshal([]byte(`{"tier_sweep": {"enabled": true, "runner": `+block+`}}`), &p); err != nil {
		t.Fatalf("a runner block does not decode: %v", err)
	}
	r := p.TierSweep.Runner
	if r == nil {
		t.Fatal("the runner block decoded as nil")
	}
	if r.Backend != TierSweepRunnerDocker || r.Image != "gt-polecat:main" || r.CPUs != 8 || r.Memory != 24 ||
		r.Jobs != 4 || r.OutputRoot != "/tmp/gt-sweep" || r.Context != "colima-gt-sbx" || !r.Dind {
		t.Fatalf("decoded %+v, want the block as written", r)
	}

	// The block also decodes through the config kernel's strict walk, which is
	// the path a town's settings/config.json takes.
	doc := `{"type":"daemon-patrol-config","version":1,"patrols":{"tier_sweep":{"enabled":true,"runner":` + block + `}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("settings.json", []byte(doc), &cfg); err != nil {
		t.Fatalf("settings.json with a runner block = %v", err)
	}
	if cfg.Patrols.TierSweep.Runner == nil {
		t.Error("the kernel's decode dropped the runner block")
	}

	bad := map[string]struct{ block, wantKey string }{
		"unknown key":      {`{"backend":"docker","image":"i","cpus":8,"memory":24,"jobs":4,"output_root":"/tmp/o","context":"c","backends":"docker"}`, "backends"},
		"cpus below range": {`{"backend":"docker","image":"i","cpus":0,"memory":24,"jobs":4,"output_root":"/tmp/o","context":"c"}`, "runner.cpus"},
		"cpus above range": {`{"backend":"docker","image":"i","cpus":17,"memory":24,"jobs":4,"output_root":"/tmp/o","context":"c"}`, "runner.cpus"},
		"no backend":       {`{"image":"i","cpus":8,"memory":24,"jobs":4,"output_root":"/tmp/o","context":"c"}`, "runner.backend"},
		"unknown backend":  {`{"backend":"podman","image":"i","cpus":8,"memory":24,"jobs":4,"output_root":"/tmp/o","context":"c"}`, "runner.backend"},
		"memory too small": {`{"backend":"docker","image":"i","cpus":8,"memory":1,"jobs":4,"output_root":"/tmp/o","context":"c"}`, "runner.memory"},
		"jobs too small":   {`{"backend":"docker","image":"i","cpus":8,"memory":24,"jobs":0,"output_root":"/tmp/o","context":"c"}`, "runner.jobs"},
		"relative output":  {`{"backend":"docker","image":"i","cpus":8,"memory":24,"jobs":4,"output_root":"out","context":"c"}`, "runner.output_root"},
		"no context":       {`{"backend":"docker","image":"i","cpus":8,"memory":24,"jobs":4,"output_root":"/tmp/o"}`, "runner.context"},
	}
	for name, tt := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			err := json.Unmarshal([]byte(`{"tier_sweep": {"runner": `+tt.block+`}}`), &p)
			if err == nil {
				t.Fatalf("decoded a runner block the daemon cannot launch: %+v", p.TierSweep.Runner)
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantKey)
			}
		})
	}

	// The image is the one optional field: empty and installed both decode, so
	// the daemon follows the installed build (gt-c3sue).
	for name, image := range map[string]string{"empty": "", "installed": TierSweepRunnerInstalledImage} {
		t.Run("image "+name, func(t *testing.T) {
			t.Parallel()
			block := `{"backend":"docker","image":"` + image + `","cpus":8,"memory":24,"jobs":4,"output_root":"/tmp/o","context":"c"}`
			var p PatrolsConfig
			if err := json.Unmarshal([]byte(`{"tier_sweep": {"runner": `+block+`}}`), &p); err != nil {
				t.Fatalf("a runner block whose image is %q does not decode: %v", image, err)
			}
			if r := p.TierSweep.Runner; r == nil || r.Image != image {
				t.Fatalf("decoded %+v, want image %q", r, image)
			}
		})
	}
}

// TestSweepRunnerPodTotalMinimum holds the Kubernetes runner block's floor to
// the whole pod: with dind the setting must hold the sidecar's fixed share and
// still leave the runner its minimum, so 3 CPUs and 6 GB is the smallest block
// that loads, a smaller figure is refused naming the setting and the minimum,
// and without dind the runner's own minimum applies (gt-gg9hv.38).
func TestSweepRunnerPodTotalMinimum(t *testing.T) {
	t.Parallel()
	const base = `"image":"i","jobs":4,"output_root":"/out","context":"gt-control","backend":"kubernetes"`
	for _, tc := range []struct {
		name  string
		block string
		// want is the error a refused block must hold; empty loads.
		want string
	}{
		{
			name:  "the minimum with dind loads",
			block: `{` + base + `,"cpus":3,"memory":6,"dind":true}`,
		},
		{
			name:  "one CPU below the minimum with dind",
			block: `{` + base + `,"cpus":2,"memory":6,"dind":true}`,
			want:  "runner.cpus = 2: want at least 3",
		},
		{
			name:  "one GB below the minimum with dind",
			block: `{` + base + `,"cpus":3,"memory":5,"dind":true}`,
			want:  "runner.memory = 5: want at least 6 GB",
		},
		{
			name:  "the runner minimum without dind loads",
			block: `{` + base + `,"cpus":1,"memory":2}`,
		},
		{
			name:  "the dind floor does not apply without dind",
			block: `{` + base + `,"cpus":1,"memory":2,"dind":false}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var p PatrolsConfig
			err := json.Unmarshal([]byte(`{"tier_sweep":{"runner":`+tc.block+`}}`), &p)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("a block at the minimum does not load: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("decoded a pod total below the dind minimum: %+v", p.TierSweep.Runner)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to hold %q", err, tc.want)
			}
		})
	}
}

// TestTierSweepConfigRunnerShellDefaultsFalse: runner.shell is the opt-in that
// sends the shell stage to the runner. A block that omits it keeps the shell
// stage on the host, and one that sets it decodes true through both the plain
// and the kernel decoders (gt-b0mwr).
func TestTierSweepConfigRunnerShellDefaultsFalse(t *testing.T) {
	t.Parallel()
	const block = `{"backend": "docker", "image": "gt-polecat:main", "cpus": 8, "memory": 24, "jobs": 4, "output_root": "/out/gt-sweep", "context": "colima-gt-sbx"`

	var without PatrolsConfig
	if err := json.Unmarshal([]byte(`{"tier_sweep": {"runner": `+block+`}}}`), &without); err != nil {
		t.Fatalf("a runner block without shell does not decode: %v", err)
	}
	if without.TierSweep.Runner == nil || without.TierSweep.Runner.Shell {
		t.Fatalf("runner without a shell key = %+v, want Shell false", without.TierSweep.Runner)
	}

	const withShell = `{"tier_sweep": {"runner": ` + block + `, "shell": true}}}`
	var with PatrolsConfig
	if err := json.Unmarshal([]byte(withShell), &with); err != nil {
		t.Fatalf("a runner block with shell does not decode: %v", err)
	}
	if with.TierSweep.Runner == nil || !with.TierSweep.Runner.Shell {
		t.Fatalf("runner with shell true = %+v, want Shell true", with.TierSweep.Runner)
	}

	doc := `{"type":"daemon-patrol-config","version":1,"patrols":` + withShell + `}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("settings.json", []byte(doc), &cfg); err != nil {
		t.Fatalf("settings.json with runner.shell = %v", err)
	}
	if cfg.Patrols.TierSweep.Runner == nil || !cfg.Patrols.TierSweep.Runner.Shell {
		t.Error("the kernel's decode dropped runner.shell")
	}
}

// TestDiskMaintenanceCacheTrimInterval: the cache trim's cadence is 2h unless the
// setting names another, and a value the patrol cannot schedule is an error
// that names the setting, at decode and when read, never a fallback to the
// default (gt-gg9hv.24.2).
func TestDiskMaintenanceCacheTrimInterval(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		interval string
		want     time.Duration
		wantErr  string
	}{
		{name: "empty is the 2h default", want: 2 * time.Hour},
		{name: "a valid override is read", interval: "45m", want: 45 * time.Minute},
		{name: "not a duration", interval: "soon", wantErr: "not a Go duration"},
		{name: "zero", interval: "0s", wantErr: "one-minute minimum"},
		{name: "negative", interval: "-2h", wantErr: "one-minute minimum"},
		{name: "sub-minute", interval: "30s", wantErr: "one-minute minimum"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DiskMaintenanceConfig{CacheTrimInterval: tt.interval}.CacheTrimEvery()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) ||
					!strings.Contains(err.Error(), "patrols.disk_maintenance.cache_trim_interval") {
					t.Fatalf("CacheTrimEvery(%q) = %v, %v; want an error naming the setting and %q", tt.interval, got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CacheTrimEvery(%q) error = %v", tt.interval, err)
			}
			if got != tt.want {
				t.Errorf("CacheTrimEvery(%q) = %v, want %v", tt.interval, got, tt.want)
			}
		})
	}

	var cfg PatrolsConfig
	if err := json.Unmarshal([]byte(`{"disk_maintenance": {"cache_trim_interval": "45m"}}`), &cfg); err != nil {
		t.Fatalf("a valid cache_trim_interval does not decode: %v", err)
	}
	if got := cfg.DiskMaintenance.CacheTrimInterval; got != "45m" {
		t.Errorf("cache_trim_interval decoded as %q, want 45m", got)
	}
	for name, doc := range map[string]string{
		"malformed": `{"disk_maintenance": {"cache_trim_interval": "soon"}}`,
		"zero":      `{"disk_maintenance": {"cache_trim_interval": "0s"}}`,
	} {
		var p PatrolsConfig
		if err := json.Unmarshal([]byte(doc), &p); err == nil {
			t.Errorf("%s cache_trim_interval decoded without an error: %+v", name, p.DiskMaintenance)
		}
	}
}

// TestCacheTrimEarlyPercent: the early cache trim's percent defaults to 75, a
// value from 50 to 95 is read, and a value outside that range is refused with
// an error naming the setting, at decode and from code alike.
func TestCacheTrimEarlyPercent(t *testing.T) {
	t.Parallel()
	pct := func(p int) *int { return &p }
	cases := []struct {
		name    string
		percent *int
		want    int
		wantErr bool
	}{
		{name: "absent is the 75 default", want: 75},
		{name: "the lower bound is read", percent: pct(50), want: 50},
		{name: "the upper bound is read", percent: pct(95), want: 95},
		{name: "49 is refused", percent: pct(49), wantErr: true},
		{name: "96 is refused", percent: pct(96), wantErr: true},
		{name: "zero is refused, not the default", percent: pct(0), wantErr: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DiskMaintenanceConfig{CacheTrimEarlyPercent: tt.percent}.CacheTrimEarlyPercentValue()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "patrols.disk_maintenance.cache_trim_early_percent") {
					t.Fatalf("CacheTrimEarlyPercentValue() = %v, %v; want an error naming the setting", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CacheTrimEarlyPercentValue() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("CacheTrimEarlyPercentValue() = %d, want %d", got, tt.want)
			}
		})
	}

	var cfg PatrolsConfig
	if err := json.Unmarshal([]byte(`{"disk_maintenance": {"cache_trim_early_percent": 80}}`), &cfg); err != nil {
		t.Fatalf("a valid cache_trim_early_percent does not decode: %v", err)
	}
	if cfg.DiskMaintenance.CacheTrimEarlyPercent == nil || *cfg.DiskMaintenance.CacheTrimEarlyPercent != 80 {
		t.Fatalf("cache_trim_early_percent decoded as %v, want 80", cfg.DiskMaintenance.CacheTrimEarlyPercent)
	}
	encoded, err := json.Marshal(cfg.DiskMaintenance)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"cache_trim_early_percent":80`) {
		t.Errorf("encoded %s does not carry the setting back out", encoded)
	}
	for name, doc := range map[string]string{
		"below the range": `{"disk_maintenance": {"cache_trim_early_percent": 49}}`,
		"above the range": `{"disk_maintenance": {"cache_trim_early_percent": 96}}`,
	} {
		var p PatrolsConfig
		if err := json.Unmarshal([]byte(doc), &p); err == nil {
			t.Errorf("%s cache_trim_early_percent decoded without an error: %+v", name, p.DiskMaintenance)
		}
	}
}

// TestDiskMaintenanceCICacheSettings: the CI build-cache trim's three settings
// default to 1h, 12h and 30GB, and a value the patrol cannot act on is refused
// naming its own key, zero and negative values included, never read as the
// default (gt-gg9hv.57).
func TestDiskMaintenanceCICacheSettings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		field   string
		value   string
		want    string
		wantErr string
	}{
		{name: "interval empty is the 1h default", field: "ci_cache_trim_interval", want: "1h0m0s"},
		{name: "interval override is read", field: "ci_cache_trim_interval", value: "30m", want: "30m0s"},
		{name: "interval zero is refused", field: "ci_cache_trim_interval", value: "0s", wantErr: "one-minute minimum"},
		{name: "interval negative is refused", field: "ci_cache_trim_interval", value: "-1h", wantErr: "one-minute minimum"},
		{name: "interval malformed is refused", field: "ci_cache_trim_interval", value: "hourly", wantErr: "not a Go duration"},
		{name: "max age empty is the 12h default", field: "ci_cache_max_age", want: "12h0m0s"},
		{name: "max age override is read", field: "ci_cache_max_age", value: "24h", want: "24h0m0s"},
		{name: "max age zero is refused", field: "ci_cache_max_age", value: "0h", wantErr: "one-minute minimum"},
		{name: "max age negative is refused", field: "ci_cache_max_age", value: "-12h", wantErr: "one-minute minimum"},
		{name: "warn bytes empty is the 30GB default", field: "ci_cache_warn_bytes", want: "32212254720"},
		{name: "warn bytes override is read", field: "ci_cache_warn_bytes", value: "512MB", want: "536870912"},
		{name: "warn bytes zero is refused", field: "ci_cache_warn_bytes", value: "0GB", wantErr: "zero"},
		{name: "warn bytes negative is refused", field: "ci_cache_warn_bytes", value: "-30GB", wantErr: "not a size"},
		{name: "warn bytes malformed is refused", field: "ci_cache_warn_bytes", value: "lots", wantErr: "not a size"},
		{name: "warn bytes overflow is refused", field: "ci_cache_warn_bytes", value: "99999999TB", wantErr: "too large"},
		{name: "warn bytes digits past int64 are refused", field: "ci_cache_warn_bytes", value: "99999999999999999999", wantErr: "not a size"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := DiskMaintenanceConfig{}
			var (
				got string
				err error
			)
			switch tt.field {
			case "ci_cache_trim_interval":
				cfg.CICacheTrimInterval = tt.value
				var d time.Duration
				d, err = cfg.CICacheTrimEvery()
				got = d.String()
			case "ci_cache_max_age":
				cfg.CICacheMaxAge = tt.value
				var d time.Duration
				d, err = cfg.CICacheMaxAgeDuration()
				got = d.String()
			case "ci_cache_warn_bytes":
				cfg.CICacheWarnBytes = tt.value
				var n int64
				n, err = cfg.CICacheWarnLimit()
				got = strconv.FormatInt(n, 10)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) ||
					!strings.Contains(err.Error(), "patrols.disk_maintenance."+tt.field) {
					t.Fatalf("%s = %q: got %s, %v; want an error naming the key and %q", tt.field, tt.value, got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s = %q: error %v", tt.field, tt.value, err)
			}
			if got != tt.want {
				t.Errorf("%s = %q: got %s, want %s", tt.field, tt.value, got, tt.want)
			}
		})
	}

	for name, doc := range map[string]string{
		"interval zero":   `{"disk_maintenance": {"ci_cache_trim_interval": "0s"}}`,
		"max age zero":    `{"disk_maintenance": {"ci_cache_max_age": "0h"}}`,
		"warn bytes zero": `{"disk_maintenance": {"ci_cache_warn_bytes": "0GB"}}`,
	} {
		var p PatrolsConfig
		if err := json.Unmarshal([]byte(doc), &p); err == nil {
			t.Errorf("%s decoded without an error: %+v", name, p.DiskMaintenance)
		}
	}
	var p PatrolsConfig
	doc := `{"disk_maintenance": {"ci_cache_trim_interval": "2h", "ci_cache_max_age": "6h", "ci_cache_warn_bytes": "10GB"}}`
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("valid ci_cache settings do not decode: %v", err)
	}
	if p.DiskMaintenance.CICacheTrimInterval != "2h" || p.DiskMaintenance.CICacheMaxAge != "6h" || p.DiskMaintenance.CICacheWarnBytes != "10GB" {
		t.Errorf("ci_cache settings decoded as %+v", p.DiskMaintenance)
	}
}

// TestDiskMaintenanceCIEnginePruneSettings: the CI engine prune's settings default
// to 1h, 48h and ["ci-base"]; a value the patrol cannot act on is refused naming
// its own key, and an explicit empty keep list is refused rather than read as
// the default (gt-i02983).
func TestDiskMaintenanceCIEnginePruneSettings(t *testing.T) {
	t.Parallel()
	t.Run("interval empty is the 1h default, override is read", func(t *testing.T) {
		t.Parallel()
		every, err := DiskMaintenanceConfig{}.CIEnginePruneEvery()
		if err != nil || every.String() != "1h0m0s" {
			t.Fatalf("empty interval = %v, %v; want 1h0m0s", every, err)
		}
		every, err = DiskMaintenanceConfig{CIEnginePruneInterval: "30m"}.CIEnginePruneEvery()
		if err != nil || every.String() != "30m0s" {
			t.Fatalf("interval 30m = %v, %v; want 30m0s", every, err)
		}
	})
	t.Run("interval refused naming its key", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{"0s", "-1h", "hourly"} {
			_, err := DiskMaintenanceConfig{CIEnginePruneInterval: raw}.CIEnginePruneEvery()
			if err == nil || !strings.Contains(err.Error(), "patrols.disk_maintenance.ci_engine_prune_interval") {
				t.Errorf("interval %q: error %v, want one naming the key", raw, err)
			}
		}
	})
	t.Run("age empty is the 48h default, override is read, zero refused", func(t *testing.T) {
		t.Parallel()
		age, err := DiskMaintenanceConfig{}.CIEnginePruneAgeDuration()
		if err != nil || age.String() != "48h0m0s" {
			t.Fatalf("empty age = %v, %v; want 48h0m0s", age, err)
		}
		age, err = DiskMaintenanceConfig{CIEnginePruneAge: "24h"}.CIEnginePruneAgeDuration()
		if err != nil || age.String() != "24h0m0s" {
			t.Fatalf("age 24h = %v, %v; want 24h0m0s", age, err)
		}
		_, err = DiskMaintenanceConfig{CIEnginePruneAge: "0h"}.CIEnginePruneAgeDuration()
		if err == nil || !strings.Contains(err.Error(), "patrols.disk_maintenance.ci_engine_prune_age") {
			t.Errorf("age 0h: error %v, want one naming the key", err)
		}
	})
	t.Run("keep repos: absent is ci-base, explicit list is read", func(t *testing.T) {
		t.Parallel()
		repos, err := DiskMaintenanceConfig{}.CIEngineKeepRepoList()
		if err != nil || !slices.Equal(repos, []string{"ci-base"}) {
			t.Fatalf("absent keep repos = %v, %v; want [ci-base]", repos, err)
		}
		repos, err = DiskMaintenanceConfig{CIEngineKeepRepos: []string{"ci-base", "org/tools"}}.CIEngineKeepRepoList()
		if err != nil || !slices.Equal(repos, []string{"ci-base", "org/tools"}) {
			t.Fatalf("keep repos = %v, %v; want the list", repos, err)
		}
	})
	t.Run("keep repos: explicit empty and a malformed name are refused", func(t *testing.T) {
		t.Parallel()
		for name, repos := range map[string][]string{
			"explicit empty":    {},
			"with a space":      {"ci base"},
			"with a dash lead":  {"-ci-base"},
			"with a tag":        {"ci-base:latest"},
			"with a slash lead": {"/ci-base"},
		} {
			_, err := DiskMaintenanceConfig{CIEngineKeepRepos: repos}.CIEngineKeepRepoList()
			if err == nil || !strings.Contains(err.Error(), "patrols.disk_maintenance.ci_engine_keep_repos") {
				t.Errorf("%s: error %v, want one naming the key", name, err)
			}
		}
	})
	t.Run("the patrol block refuses a bad value at decode", func(t *testing.T) {
		t.Parallel()
		for name, doc := range map[string]string{
			"interval zero":    `{"disk_maintenance": {"ci_engine_prune_interval": "0s"}}`,
			"age zero":         `{"disk_maintenance": {"ci_engine_prune_age": "0h"}}`,
			"keep repos empty": `{"disk_maintenance": {"ci_engine_keep_repos": []}}`,
		} {
			var p PatrolsConfig
			if err := json.Unmarshal([]byte(doc), &p); err == nil {
				t.Errorf("%s decoded without an error: %+v", name, p.DiskMaintenance)
			}
		}
		var p PatrolsConfig
		doc := `{"disk_maintenance": {"ci_engine_prune_interval": "2h", "ci_engine_prune_age": "72h", "ci_engine_keep_repos": ["ci-base"]}}`
		if err := json.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatalf("valid ci_engine settings do not decode: %v", err)
		}
		if p.DiskMaintenance.CIEnginePruneInterval != "2h" || p.DiskMaintenance.CIEnginePruneAge != "72h" ||
			!slices.Equal(p.DiskMaintenance.CIEngineKeepRepos, []string{"ci-base"}) {
			t.Errorf("ci_engine settings decoded as %+v", p.DiskMaintenance)
		}
	})
}

// TestDiskMaintenanceTargetEvery: a cadence is a Go duration of at least a
// minute, and an empty one keeps the daily window.
func TestDiskMaintenanceTargetEvery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		interval string
		want     time.Duration
		wantErr  string
	}{
		{name: "empty keeps the daily window"},
		{name: "hours", interval: "2h", want: 2 * time.Hour},
		{name: "the minimum", interval: "1m", want: time.Minute},
		{name: "not a duration", interval: "soon", wantErr: "not a Go duration"},
		{name: "zero", interval: "0s", wantErr: "one-minute minimum"},
		{name: "negative", interval: "-2h", wantErr: "one-minute minimum"},
		{name: "sub-minute", interval: "30s", wantErr: "one-minute minimum"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DiskMaintenanceTarget{Interval: tt.interval}.Every()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Every(%q) = %v, %v; want an error naming %q", tt.interval, got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Every(%q) = %v", tt.interval, err)
			}
			if got != tt.want {
				t.Errorf("Every(%q) = %v, want %v", tt.interval, got, tt.want)
			}
		})
	}
}

// The daemon.supervisor setting selects the service manager a provisioned
// daemon runs under: the three kinds, and unset keeps the host's own
// detection.
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
	if LaunchdJobEnv != "XPC_SERVICE_NAME" {
		t.Errorf("LaunchdJobEnv = %q, want XPC_SERVICE_NAME", LaunchdJobEnv)
	}
	if SystemdInvocationEnv != "INVOCATION_ID" {
		t.Errorf("SystemdInvocationEnv = %q, want INVOCATION_ID", SystemdInvocationEnv)
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

// The dev-tag step reads the polecat image from the CI registry the setting names.
// Unset, the setting is the images workflow's repository; set, it must be a bare
// host:port and a repository path, never a URL (gt-f1igu0).
func TestDaemonPatrolConfig_PolecatImageSource(t *testing.T) {
	t.Parallel()
	if got := (&DaemonPatrolConfig{}).PolecatImageSource(); got != DefaultPolecatImageSource {
		t.Errorf("PolecatImageSource() unset = %q, want the default %q", got, DefaultPolecatImageSource)
	}
	var nilCfg *DaemonPatrolConfig
	if got := nilCfg.PolecatImageSource(); got != DefaultPolecatImageSource {
		t.Errorf("PolecatImageSource() on nil = %q, want the default", got)
	}
	if DefaultPolecatImageSource != "forgejo:3000/bot-registry/gastown-polecat" {
		t.Errorf("DefaultPolecatImageSource = %q, want forgejo:3000/bot-registry/gastown-polecat", DefaultPolecatImageSource)
	}
	for _, tt := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{"unset", `{}`, ""},
		{"host and repository", `{"polecat_image_source":"registry.internal:5000/owner/gastown-polecat"}`, ""},
		{"scheme", `{"polecat_image_source":"https://forgejo:3000/bot-registry/gastown-polecat"}`, "not a URL with a scheme"},
		{"no repository path", `{"polecat_image_source":"forgejo:3000"}`, "want a repository path after host:port"},
		{"no port", `{"polecat_image_source":"forgejo/bot-registry/gastown-polecat"}`, "want host:port"},
		{"uppercase repository", `{"polecat_image_source":"forgejo:3000/Bot-Registry/gastown-polecat"}`, "want a repository path of lowercase"},
		{"whitespace", `{"polecat_image_source":"forgejo:3000/bot registry/gastown-polecat"}`, "want no whitespace"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cfg DaemonPatrolConfig
			if err := DecodeJSONFile("daemon.json", []byte(tt.body), &cfg); err != nil {
				t.Fatalf("decode %s = %v", tt.body, err)
			}
			err := ValidateDaemonPatrolConfig(&cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateDaemonPatrolConfig() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateDaemonPatrolConfig() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// A town carries the container compose file only when it was set up as a
// container town, and the answer reads no settings, so it holds when they
// cannot be read. A compose file that cannot be statted for a reason other than
// being absent counts as present: the answer fails toward the container.
func TestMayBeContainerTown(t *testing.T) {
	t.Parallel()
	withCompose := t.TempDir()
	composePath := ContainerComposeFile(withCompose)
	if err := os.MkdirAll(filepath.Dir(composePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A regular file where the town's daemon directory should be makes the
	// stat fail with ENOTDIR, not ErrNotExist.
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, "daemon"), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		town string
		want bool
	}{
		{"compose file present", withCompose, true},
		{"no compose file", t.TempDir(), false},
		{"stat fails for another reason", blocked, true},
	} {
		if got := MayBeContainerTown(tc.town); got != tc.want {
			t.Errorf("%s: MayBeContainerTown = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestContainerKubeconfigFile pins the one definition of the container's
// derived kubeconfig: the compose render writes this path and the daemon's
// container-mode patrol and cache trim read it, each pinned to the same literal
// in its own package.
func TestContainerKubeconfigFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	want := filepath.Join(town, ".runtime", "container", "kubeconfig.yaml")
	if got := ContainerKubeconfigFile(town); got != want {
		t.Errorf("ContainerKubeconfigFile = %q, want %q", got, want)
	}
}

// A spec_dispatch block that sets no max_in_flight runs the code default of
// two slings at once, and a zero or negative value is the default too: the cap
// never reads as unbounded (gt-16pqa).
func TestSpecDispatchInFlightCapDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		json string
		want int
	}{
		{"absent", `{}`, DefaultSpecMaxInFlight},
		{"zero", `{"max_in_flight":0}`, DefaultSpecMaxInFlight},
		{"negative", `{"max_in_flight":-3}`, DefaultSpecMaxInFlight},
		{"one", `{"max_in_flight":1}`, 1},
		{"set", `{"max_in_flight":7}`, 7},
	} {
		var sd SpecDispatchConfig
		if err := json.Unmarshal([]byte(tc.json), &sd); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := sd.InFlightCap(); got != tc.want {
			t.Errorf("%s: InFlightCap = %d, want %d", tc.name, got, tc.want)
		}
	}
	var nilBlock *SpecDispatchConfig
	if got := nilBlock.InFlightCap(); got != DefaultSpecMaxInFlight {
		t.Errorf("nil block: InFlightCap = %d, want the default %d", got, DefaultSpecMaxInFlight)
	}
}

// The jev_scorecard patrol's block decodes its four keys, and a value the
// patrol cannot act on is refused at decode rather than read as a default.
func TestJevScorecardConfigDecodes(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"jev_scorecard":{"enabled":true,"hour":"03:15",` +
		`"interval":"12h","batteries":["attention","drift"]}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decoding jev_scorecard = %v", err)
	}
	c := cfg.Patrols.JevScorecard
	if c == nil || !c.Enabled || c.Hour != "03:15" || c.IntervalStr != "12h" {
		t.Fatalf("jev_scorecard = %+v, want enabled at 03:15 every 12h", c)
	}
	if got := c.BatteriesValue(); !slices.Equal(got, []string{"attention", "drift"}) {
		t.Errorf("batteries = %v, want [attention drift]", got)
	}
	if minute, err := c.ClockMinute(); err != nil || minute != 3*60+15 {
		t.Errorf("ClockMinute = %d (%v), want 03:15", minute, err)
	}
	if interval, err := c.IntervalDuration(); err != nil || interval != 12*time.Hour {
		t.Errorf("IntervalDuration = %v (%v), want 12h", interval, err)
	}
}

// A block that leaves the settings out runs on the patrol's defaults; an empty
// battery list means the patrol refreshes nothing, not that it scores every
// registered battery.
func TestJevScorecardConfigDefaults(t *testing.T) {
	t.Parallel()
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"jev_scorecard":{"enabled":true}}}`), &cfg); err != nil {
		t.Fatalf("decoding jev_scorecard = %v", err)
	}
	c := cfg.Patrols.JevScorecard
	if minute, err := c.ClockMinute(); err != nil || minute != 4*60+30 {
		t.Errorf("default clock minute = %d (%v), want 04:30", minute, err)
	}
	if interval, err := c.IntervalDuration(); err != nil || interval != JevScorecardDefaultInterval {
		t.Errorf("default interval = %v (%v), want %v", interval, err, JevScorecardDefaultInterval)
	}
	if got := c.BatteriesValue(); len(got) != 0 {
		t.Errorf("default batteries = %v, want none", got)
	}
}

// TestJevScorecardConfigRefusesBadValues names the key for each value the
// patrol cannot act on: a typo must not quietly read as the default.
func TestJevScorecardConfigRefusesBadValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"hour not a clock", `{"hour":"4am"}`, "jev_scorecard.hour"},
		{"interval not a duration", `{"interval":"daily"}`, "jev_scorecard.interval"},
		{"interval zero", `{"interval":"0h"}`, "jev_scorecard.interval"},
		{"interval zero seconds", `{"interval":"0s"}`, "jev_scorecard.interval"},
		{"interval negative", `{"interval":"-1h"}`, "jev_scorecard.interval"},
		{"empty battery name", `{"batteries":[""]}`, "battery name is empty"},
		{"repeated battery", `{"batteries":["attention","attention"]}`, "listed twice"},
		{"unknown key", `{"hours":"04:30"}`, "unknown field"},
	} {
		var cfg DaemonPatrolConfig
		err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"jev_scorecard":`+tc.body+`}}`), &cfg)
		if err == nil {
			t.Errorf("%s: decoded, want an error naming %s", tc.name, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to name %s", tc.name, err, tc.want)
		}
	}
}
