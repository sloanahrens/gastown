package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDaemonEnv_MissingFileReturnsEmptyMap(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	env, err := LoadDaemonEnv(townRoot)
	if err != nil {
		t.Fatalf("LoadDaemonEnv() error = %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("LoadDaemonEnv() = %v, want empty map", env)
	}
}

func TestLoadDaemonEnv_ParsesKeyValuePairs(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	content := "# comment line\n" +
		"\n" +
		"CMUX_CLAUDE_HOOKS_DISABLED=1\n" +
		"SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk\n" +
		"DEVELOPER_DIR=/Library/Developer/CommandLineTools\n"
	if err := os.WriteFile(filepath.Join(settingsDir, "daemon.env"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	env, err := LoadDaemonEnv(townRoot)
	if err != nil {
		t.Fatalf("LoadDaemonEnv() error = %v", err)
	}

	want := map[string]string{
		"CMUX_CLAUDE_HOOKS_DISABLED": "1",
		"SDKROOT":                    "/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk",
		"DEVELOPER_DIR":              "/Library/Developer/CommandLineTools",
	}
	if len(env) != len(want) {
		t.Fatalf("LoadDaemonEnv() = %v, want %v", env, want)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("LoadDaemonEnv()[%q] = %q, want %q", k, env[k], v)
		}
	}
}

func TestLoadDaemonEnv_MissingEqualsIsError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "daemon.env"), []byte("NOT_A_PAIR\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := LoadDaemonEnv(townRoot); err == nil {
		t.Fatal("LoadDaemonEnv() error = nil, want error for malformed line")
	}
}

func TestDaemonEnvPath(t *testing.T) {
	t.Parallel()
	got := DaemonEnvPath("/town")
	want := filepath.Join("/town", "settings", "daemon.env")
	if got != want {
		t.Errorf("DaemonEnvPath() = %q, want %q", got, want)
	}
}

// writeDaemonEnv writes townRoot/settings/daemon.env.
func writeDaemonEnv(t *testing.T, townRoot, body string) {
	t.Helper()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveDoltPassword(t *testing.T) {
	t.Parallel()
	ref := func(v string) *DoltThresholds { return &DoltThresholds{Password: &v} }
	noEnv := func(string) string { return "" }

	t.Run("unset yields no password", func(t *testing.T) {
		t.Parallel()
		if got := resolveDoltPassword(t.TempDir(), &DoltThresholds{}, noEnv); got != "" {
			t.Errorf("got %q, want empty", got)
		}
		if got := resolveDoltPassword(t.TempDir(), ref(""), noEnv); got != "" {
			t.Errorf("an empty setting got %q, want empty", got)
		}
	})

	t.Run("reference reads daemon.env", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonEnv(t, townRoot, "GT_DOLT_PASSWORD=from-file\n")
		if got := resolveDoltPassword(townRoot, ref("${GT_DOLT_PASSWORD}"), noEnv); got != "from-file" {
			t.Errorf("got %q, want from-file", got)
		}
	})

	t.Run("name daemon.env lacks falls back to the process env", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonEnv(t, townRoot, "OTHER=x\n")
		getenv := func(name string) string {
			if name == "GT_DOLT_PASSWORD" {
				return "from-process"
			}
			return ""
		}
		if got := resolveDoltPassword(townRoot, ref("${GT_DOLT_PASSWORD}"), getenv); got != "from-process" {
			t.Errorf("got %q, want from-process", got)
		}
	})

	t.Run("literal is used as it stands", func(t *testing.T) {
		t.Parallel()
		if got := resolveDoltPassword(t.TempDir(), ref("hunter2"), noEnv); got != "hunter2" {
			t.Errorf("got %q, want hunter2", got)
		}
	})
}
