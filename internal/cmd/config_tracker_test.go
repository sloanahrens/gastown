package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// loadTestTown reads the town.json setupTestTownForConfig wrote.
func loadTestTown(t *testing.T, townRoot string) *config.TownConfig {
	t.Helper()
	town, err := config.LoadTownConfig(filepath.Join(townRoot, "mayor", "town.json"))
	if err != nil {
		t.Fatalf("load town.json: %v", err)
	}
	return town
}

func TestConfigSetTrackerPortWritesTheTownEndpoint(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)

	if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"tracker.port", "8000"}); err != nil {
		t.Fatalf("config set tracker.port: %v", err)
	}
	town := loadTestTown(t, townRoot)
	if town.Tracker == nil || town.Tracker.Port != 8000 {
		t.Fatalf("town.json tracker = %+v, want port 8000", town.Tracker)
	}

	var out bytes.Buffer
	if err := configGet(townConfigCmdEnv(townRoot, &out), []string{"tracker.port"}); err != nil {
		t.Fatalf("config get tracker.port: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "8000" {
		t.Errorf("config get tracker.port = %q, want 8000", got)
	}
}

func TestConfigSetTrackerHostKeepsThePort(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)

	if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"tracker.port", "8000"}); err != nil {
		t.Fatalf("config set tracker.port: %v", err)
	}
	if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"tracker.host", "tracker.internal"}); err != nil {
		t.Fatalf("config set tracker.host: %v", err)
	}
	town := loadTestTown(t, townRoot)
	want := config.TrackerEndpoint{Host: "tracker.internal", Port: 8000}
	if town.Tracker == nil || *town.Tracker != want {
		t.Fatalf("town.json tracker = %+v, want %+v", town.Tracker, want)
	}

	var out bytes.Buffer
	if err := configGet(townConfigCmdEnv(townRoot, &out), []string{"tracker.host"}); err != nil {
		t.Fatalf("config get tracker.host: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "tracker.internal" {
		t.Errorf("config get tracker.host = %q, want tracker.internal", got)
	}
}

// A town gt install has not stamped reads as trackerd's own defaults.
func TestConfigGetTrackerDefaults(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)

	for _, tt := range []struct{ key, want string }{
		{"tracker.port", "7433"},
		{"tracker.host", "127.0.0.1"},
	} {
		var out bytes.Buffer
		if err := configGet(townConfigCmdEnv(townRoot, &out), []string{tt.key}); err != nil {
			t.Fatalf("config get %s: %v", tt.key, err)
		}
		if got := strings.TrimSpace(out.String()); got != tt.want {
			t.Errorf("config get %s = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestConfigSetTrackerRefusesBadValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, key, value, wantErr string
	}{
		{"port that is not a number", "tracker.port", "seven", "expected port number"},
		{"port below the range", "tracker.port", "80", "expected port number"},
		{"port above the range", "tracker.port", "70000", "expected port number"},
		{"empty host", "tracker.host", " ", "expected a host name"},
		{"host with a path", "tracker.host", "tracker/internal", "expected a host name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := setupTestTownForConfig(t)
			err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{tt.key, tt.value})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("config set %s %q = %v, want an error naming %q", tt.key, tt.value, err, tt.wantErr)
			}
			if town := loadTestTown(t, townRoot); town.Tracker != nil {
				t.Errorf("town.json tracker = %+v after a refused value", town.Tracker)
			}
		})
	}
}

// tornTownJSON is a town.json the loaders and writers both refuse.
const tornTownJSON = `{"type":`

func writeTownJSON(t *testing.T, townRoot, body string) {
	t.Helper()
	path := filepath.Join(townRoot, "mayor", "town.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
}

// A town.json the write cannot complete fails the command: a tracker setting
// that was not saved must never read as set.
func TestConfigSetTrackerFailsOnATownJSONItCannotWrite(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ key, value string }{
		{"tracker.port", "8000"},
		{"tracker.host", "tracker.internal"},
	} {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			townRoot := setupTestTownForConfig(t)
			writeTownJSON(t, townRoot, tornTownJSON)
			err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{tt.key, tt.value})
			if err == nil || !strings.Contains(err.Error(), "saving town.json") {
				t.Fatalf("config set %s over a torn town.json = %v, want a saving town.json error", tt.key, err)
			}
		})
	}
}

// gt config get reports a tracker entry it cannot read rather than falling
// back to the default.
func TestConfigGetTrackerFailsOnAnInvalidPort(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)
	writeTownJSON(t, townRoot, `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","tracker":{"port":70000}}`)
	err := configGet(townConfigCmdEnv(townRoot, io.Discard), []string{"tracker.port"})
	if err == nil || !strings.Contains(err.Error(), "tracker.port") {
		t.Fatalf("config get tracker.port on an invalid port = %v, want a tracker.port error", err)
	}
}
