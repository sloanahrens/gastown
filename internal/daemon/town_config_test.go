package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

const brokenDaemonJSON = "{\n  \"type\": \"daemon-patrol-config\",\n  \"patrols\": {\"witness\": {\"enabled\": false},}\n}\n"

func writeTownFile(t *testing.T, townRoot, rel, body string) string {
	t.Helper()
	path := filepath.Join(townRoot, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireUnchanged(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s was rewritten:\n%s", path, got)
	}
}

// TestEnsureLifecycleConfigFileNeverRewritesAnUnparseableFile is G3-03: a
// trailing comma used to get the operator's daemon.json replaced by
// DefaultLifecycleConfig, re-enabling every patrol they had turned off.
func TestEnsureLifecycleConfigFileNeverRewritesAnUnparseableFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTownFile(t, town, "mayor/daemon.json", brokenDaemonJSON)
	err := EnsureLifecycleConfigFile(town)
	if !errors.Is(err, config.ErrUnparseable) || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("EnsureLifecycleConfigFile = %v, want ErrUnparseable naming %s and the offset", err, path)
	}
	requireUnchanged(t, path, brokenDaemonJSON)
}

func TestSavePatrolConfigRefusesToReplaceAnUnparseableFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTownFile(t, town, "mayor/daemon.json", brokenDaemonJSON)
	if err := SavePatrolConfig(town, DefaultLifecycleConfig()); !errors.Is(err, config.ErrUnparseable) {
		t.Fatalf("SavePatrolConfig = %v, want ErrUnparseable", err)
	}
	requireUnchanged(t, path, brokenDaemonJSON)
}

func TestReadPatrolConfigSeparatesAbsentFromBroken(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if cfg, err := ReadPatrolConfig(town); cfg != nil || err != nil {
		t.Fatalf("absent = %v, %v; want nil, nil", cfg, err)
	}
	writeTownFile(t, town, "mayor/daemon.json", brokenDaemonJSON)
	if cfg, err := ReadPatrolConfig(town); cfg != nil || !errors.Is(err, config.ErrUnparseable) {
		t.Fatalf("broken = %v, %v; want nil, ErrUnparseable", cfg, err)
	}
}

func TestCheckTownConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := CheckTownConfig(town); err != nil {
		t.Fatalf("absent files = %v, want nil (first run creates them)", err)
	}
	writeTownFile(t, town, "mayor/daemon.json", `{"type":"daemon-patrol-config","version":1}`)
	writeTownFile(t, town, "settings/config.json", `{"type":"town-settings","version":1}`)
	if err := CheckTownConfig(town); err != nil {
		t.Fatalf("valid files = %v", err)
	}

	settings := writeTownFile(t, town, "settings/config.json", "{\"role_agents\": {\"polecat\": \"deepseek\",}}")
	err := CheckTownConfig(town)
	if !errors.Is(err, config.ErrUnparseable) || !strings.Contains(err.Error(), settings) {
		t.Fatalf("broken settings = %v, want ErrUnparseable naming %s", err, settings)
	}

	daemonJSON := writeTownFile(t, town, "mayor/daemon.json", brokenDaemonJSON)
	err = CheckTownConfig(town)
	if !strings.Contains(err.Error(), settings) || !strings.Contains(err.Error(), daemonJSON) {
		t.Fatalf("both broken = %v, want both files named", err)
	}

	// A field of the wrong type is as unparseable as a syntax error.
	writeTownFile(t, town, "settings/config.json", `{"type":"town-settings","version":1}`)
	writeTownFile(t, town, "mayor/daemon.json", `{"patrols": {"witness": {"enabled": "no"}}}`)
	if err := CheckTownConfig(town); !errors.Is(err, config.ErrUnparseable) {
		t.Fatalf("wrong field type = %v, want ErrUnparseable", err)
	}
}

// TestNewRefusesAnUnparseableTownConfig: the daemon does not start on a
// config it cannot read, and refuses before touching tmux or the file.
func TestNewRefusesAnUnparseableTownConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeTownFile(t, town, "mayor/daemon.json", brokenDaemonJSON)
	d, err := New(&Config{TownRoot: town, LogFile: filepath.Join(town, "daemon", "daemon.log"), PidFile: filepath.Join(town, "daemon", "daemon.pid")})
	if d != nil || !errors.Is(err, config.ErrUnparseable) {
		t.Fatalf("New = %v, %v; want a refusal", d, err)
	}
	requireUnchanged(t, path, brokenDaemonJSON)
}
