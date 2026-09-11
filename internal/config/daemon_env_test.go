package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDaemonEnv_MissingFileReturnsEmptyMap(t *testing.T) {
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
	got := DaemonEnvPath("/town")
	want := filepath.Join("/town", "settings", "daemon.env")
	if got != want {
		t.Errorf("DaemonEnvPath() = %q, want %q", got, want)
	}
}
