package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestApplyDoltServerConfigEnvUsesNormalizedManagerConfig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeManagedDoltConfig(t, townRoot, "listener:\n  host: 127.0.0.2\n  port: 5507\n")
	env := mapEnv{
		"GT_DOLT_HOST": "stale-env-host", "GT_DOLT_PORT": "9999",
		"BEADS_DOLT_SERVER_HOST": "stale-beads-host", "BEADS_DOLT_SERVER_PORT": "9999", "BEADS_DOLT_PORT": "9999",
	}

	m := NewDoltServerManager(townRoot, &DoltServerConfig{Enabled: true, Host: "stale-daemon-host", Port: 9999}, func(string, ...interface{}) {})
	applyDoltServerConfigEnvTo(env, m.config)

	env.assert(t, "GT_DOLT_HOST", "127.0.0.2")
	env.assert(t, "BEADS_DOLT_SERVER_HOST", "127.0.0.2")
	env.assert(t, "GT_DOLT_PORT", "5507")
	env.assert(t, "BEADS_DOLT_SERVER_PORT", "5507")
	env.assert(t, "BEADS_DOLT_PORT", "5507")
}

func TestApplyConfiguredDoltHostEnvClearsManagedConfigWithoutHost(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeManagedDoltConfig(t, townRoot, "listener:\n  port: 5507\n")
	env := mapEnv{"GT_DOLT_HOST": "stale-env-host", "BEADS_DOLT_SERVER_HOST": "stale-beads-host"}

	applyConfiguredDoltHostEnvTo(env, townRoot, nil)

	if got, ok := env["GT_DOLT_HOST"]; ok {
		t.Fatalf("GT_DOLT_HOST = %q, want cleared", got)
	}
	if got, ok := env["BEADS_DOLT_SERVER_HOST"]; ok {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want cleared", got)
	}
}

func TestWriteDaemonDoltConfigAutoGC(t *testing.T) {
	t.Parallel()
	t.Run("default enabled", func(t *testing.T) {
		t.Parallel()
		got := writeAndReadDaemonAutoGC(t, mapEnv{})
		if !got.Enable {
			t.Fatalf("auto_gc_behavior.enable = false, want true")
		}
		if got.ArchiveLevel != 1 {
			t.Fatalf("auto_gc_behavior.archive_level = %d, want 1", got.ArchiveLevel)
		}
	})

	t.Run("kill switch disabled", func(t *testing.T) {
		t.Parallel()
		got := writeAndReadDaemonAutoGC(t, mapEnv{"GT_DOLT_AUTO_GC": "disabled"})
		if got.Enable {
			t.Fatalf("GT_DOLT_AUTO_GC=disabled: auto_gc_behavior.enable = true, want false")
		}
		if got.ArchiveLevel != 0 {
			t.Fatalf("GT_DOLT_AUTO_GC=disabled: auto_gc_behavior.archive_level = %d, want 0", got.ArchiveLevel)
		}
	})
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

func writeAndReadDaemonAutoGC(t *testing.T, env mapEnv) struct {
	Enable       bool `yaml:"enable"`
	ArchiveLevel int  `yaml:"archive_level"`
} {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &DoltServerConfig{Port: 3307, DataDir: dir}
	if err := writeDaemonDoltConfig(cfg, configPath, env.lookup); err != nil {
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

// mapEnv is an in-memory environment: an envWriter for the apply functions
// and a lookup for the config writer.
type mapEnv map[string]string

func (e mapEnv) Setenv(key, value string) { e[key] = value }
func (e mapEnv) Unsetenv(key string)      { delete(e, key) }

func (e mapEnv) lookup(key string) (string, bool) {
	v, ok := e[key]
	return v, ok
}

func (e mapEnv) assert(t *testing.T, key, want string) {
	t.Helper()
	if got := e[key]; got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}
