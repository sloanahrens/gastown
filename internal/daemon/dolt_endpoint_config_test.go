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

// mapEnv is an in-memory environment: a lookup for the config writer.
type mapEnv map[string]string

func (e mapEnv) lookup(key string) (string, bool) {
	v, ok := e[key]
	return v, ok
}
