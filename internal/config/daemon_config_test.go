package config

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
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

// hookless_agent and max_hookless are retired with the hookless seat class
// (gt-4k3fj.8.7): a spec_dispatch block that still carries them decodes and
// keeps them on a rewrite.
func TestDaemonPatrolConfigDecodesRetiredHooklessKeys(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"spec_dispatch":{"enabled":true,"hooked_agent":"claude-sonnet","hookless_agent":"local-coder","max_hookless":2}}}`
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
