package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
	"gopkg.in/yaml.v3"
)

func TestNewDoltServerManagerNormalizesManagedEndpointFromTownConfig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeManagedDoltConfig(t, townRoot, "listener:\n  host: 127.0.0.2\n  port: 5507\n")

	cfg := &DoltServerConfig{
		Enabled:              true,
		External:             true,
		Host:                 "stale-daemon-host",
		Port:                 9999,
		User:                 "root",
		Password:             "secret",
		DataDir:              filepath.Join(townRoot, "custom-data"),
		LogFile:              filepath.Join(townRoot, "custom.log"),
		AutoRestart:          true,
		RestartDelay:         2 * time.Second,
		MaxRestartDelay:      3 * time.Second,
		MaxRestartsInWindow:  4,
		RestartWindow:        5 * time.Second,
		HealthyResetInterval: 6 * time.Second,
		HealthCheckInterval:  7 * time.Second,
	}

	m := NewDoltServerManager(townRoot, cfg, func(string, ...interface{}) {})
	if got := m.config.Host; got != "127.0.0.2" {
		t.Fatalf("manager host = %q, want managed host", got)
	}
	if got := m.config.Port; got != 5507 {
		t.Fatalf("manager port = %d, want managed port", got)
	}
	if cfg.Host != "stale-daemon-host" || cfg.Port != 9999 {
		t.Fatalf("input config was mutated: host=%q port=%d", cfg.Host, cfg.Port)
	}
	if !m.config.Enabled || !m.config.External || m.config.User != "root" || m.config.Password != "secret" {
		t.Fatalf("non-endpoint fields were not preserved: %#v", m.config)
	}
	if m.config.DataDir != cfg.DataDir || m.config.LogFile != cfg.LogFile {
		t.Fatalf("paths were not preserved: %#v", m.config)
	}
	if m.config.RestartDelay != cfg.RestartDelay || m.config.MaxRestartDelay != cfg.MaxRestartDelay || m.config.MaxRestartsInWindow != cfg.MaxRestartsInWindow || m.config.RestartWindow != cfg.RestartWindow || m.config.HealthyResetInterval != cfg.HealthyResetInterval || m.config.HealthCheckInterval != cfg.HealthCheckInterval {
		t.Fatalf("restart/health settings were not preserved: %#v", m.config)
	}
}

func TestNewDoltServerManagerPortOnlyManagedConfigClearsStaleHost(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeManagedDoltConfig(t, townRoot, "listener:\n  port: 5507\n")

	m := NewDoltServerManager(townRoot, &DoltServerConfig{Enabled: true, Host: "stale-daemon-host", Port: 9999}, func(string, ...interface{}) {})
	if got := m.config.Host; got != "" {
		t.Fatalf("manager host = %q, want cleared", got)
	}
	if got := m.config.Port; got != 5507 {
		t.Fatalf("manager port = %d, want managed port", got)
	}
}

// daemon.json's patrols.dolt_server port and host still load, so a file
// written before gt-y3pgh.9 keeps the daemon starting, but they are ignored:
// a town with no endpoint gives the manager none.
func TestDaemonJSONDoltServerEndpointIsAcceptedAndIgnored(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	daemonJSON := `{"type":"daemon-patrol-config","version":1,"patrols":{"dolt_server":{"enabled":true,"port":9999,"host":"stale-daemon-host"}}}`
	if err := os.WriteFile(PatrolConfigFile(townRoot), []byte(daemonJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(townRoot)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	m := NewDoltServerManager(townRoot, cfg.Patrols.DoltServer, func(string, ...interface{}) {})
	if m.config.Port != 0 || m.config.Host != "" {
		t.Fatalf("manager endpoint = %q:%d, want none (daemon.json values ignored)", m.config.Host, m.config.Port)
	}
}

func TestWriteDaemonDoltConfigAutoGC(t *testing.T) {
	t.Parallel()
	t.Run("default enabled", func(t *testing.T) {
		t.Parallel()
		got := writeAndReadDaemonAutoGC(t, "")
		if !got.Enable {
			t.Fatalf("auto_gc_behavior.enable = false, want true")
		}
		if got.ArchiveLevel != 1 {
			t.Fatalf("auto_gc_behavior.archive_level = %d, want 1", got.ArchiveLevel)
		}
	})

	t.Run("kill switch disabled", func(t *testing.T) {
		t.Parallel()
		got := writeAndReadDaemonAutoGC(t, `{"auto_gc":"disabled"}`)
		if got.Enable {
			t.Fatalf("operational.dolt.auto_gc=disabled: auto_gc_behavior.enable = true, want false")
		}
		if got.ArchiveLevel != 0 {
			t.Fatalf("operational.dolt.auto_gc=disabled: auto_gc_behavior.archive_level = %d, want 0", got.ArchiveLevel)
		}
	})
}

// writeTownDoltSettings writes operational.dolt into townRoot's
// settings/config.json (gt-y3pgh.2.3). doltJSON is the dolt object's body.
func writeTownDoltSettings(t *testing.T, townRoot, doltJSON string) {
	t.Helper()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"town-settings","version":1,"operational":{"dolt":` + doltJSON + `}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeManagedDoltConfig(t *testing.T, townRoot, content string) {
	t.Helper()
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeAndReadDaemonAutoGC(t *testing.T, doltSettings string) struct {
	Enable       bool `yaml:"enable"`
	ArchiveLevel int  `yaml:"archive_level"`
} {
	t.Helper()

	dir := t.TempDir()
	if doltSettings != "" {
		writeTownDoltSettings(t, dir, doltSettings)
	}
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &DoltServerConfig{Port: 3307, DataDir: dir}
	// The password comes from the town's settings reference, not the
	// daemon's env (gt-y3pgh.2.4); the auto-GC knob comes from town settings.
	if err := writeDaemonDoltConfig(cfg, configPath, doltserver.DefaultConfig(dir)); err != nil {
		t.Fatalf("writeDaemonDoltConfig: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading daemon Dolt config: %v", err)
	}

	var parsed struct {
		Behavior struct {
			AutoGCBehavior struct {
				Enable       bool `yaml:"enable"`
				ArchiveLevel int  `yaml:"archive_level"`
			} `yaml:"auto_gc_behavior"`
		} `yaml:"behavior"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("generated daemon config is invalid YAML: %v\n%s", err, data)
	}
	return parsed.Behavior.AutoGCBehavior
}

// TestWriteDaemonDoltConfig_LogLevel covers operational.dolt.log_level on the
// daemon-managed server: the same setting the CLI path already honors. The
// daemon keeps its own info default, where the CLI default is warning.
func TestWriteDaemonDoltConfig_LogLevel(t *testing.T) {
	t.Parallel()

	t.Run("unset keeps the daemon default", func(t *testing.T) {
		t.Parallel()
		if got := writeAndReadDaemonLogLevel(t, `{"auto_gc":"off"}`); got != "info" {
			t.Errorf("operational.dolt.log_level unset: log_level = %q, want info", got)
		}
	})

	t.Run("no settings file keeps the daemon default", func(t *testing.T) {
		t.Parallel()
		if got := writeAndReadDaemonLogLevel(t, ""); got != "info" {
			t.Errorf("no town settings: log_level = %q, want info", got)
		}
	})

	t.Run("configured level is emitted", func(t *testing.T) {
		t.Parallel()
		if got := writeAndReadDaemonLogLevel(t, `{"log_level":"debug"}`); got != "debug" {
			t.Errorf("operational.dolt.log_level=debug: log_level = %q, want debug", got)
		}
	})

	t.Run("invalid level is written through like the CLI path", func(t *testing.T) {
		t.Parallel()
		// Neither path validates: writeServerConfig writes the string as given
		// and Dolt rejects it at start. The daemon must not diverge by
		// swallowing or correcting it.
		if got := writeAndReadDaemonLogLevel(t, `{"log_level":"verbose"}`); got != "verbose" {
			t.Errorf("operational.dolt.log_level=verbose: log_level = %q, want verbose", got)
		}
	})
}

// writeAndReadDaemonLogLevel writes the daemon's Dolt config for a town whose
// operational.dolt is doltJSON and returns the log_level it contains. An empty
// doltJSON means the town has no settings file.
func writeAndReadDaemonLogLevel(t *testing.T, doltJSON string) string {
	t.Helper()

	dir := t.TempDir()
	if doltJSON != "" {
		writeTownDoltSettings(t, dir, doltJSON)
	}
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &DoltServerConfig{Port: 3307, DataDir: dir}
	// The log level comes from the same knobs the CLI path reads (gt-gbpvx);
	// DefaultConfig resolves them from the town's settings.
	if err := writeDaemonDoltConfig(cfg, configPath, doltserver.DefaultConfig(dir)); err != nil {
		t.Fatalf("writeDaemonDoltConfig: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading daemon Dolt config: %v", err)
	}

	var parsed struct {
		LogLevel string `yaml:"log_level"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("generated daemon config is invalid YAML: %v\n%s", err, data)
	}
	return parsed.LogLevel
}
