package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gtevents "github.com/steveyegge/gastown/internal/events"
)

func TestRunLogCrashEmitsFeedSessionDeath(t *testing.T) {
	t.Parallel()
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := logCrash(townRoot, "gastown/polecats/rust", "gt-gastown-rust", 42); err != nil {
		t.Fatalf("runLogCrash: %v", err)
	}

	if _, err := os.Stat(filepath.Join(townRoot, "logs", "town.log")); !os.IsNotExist(err) {
		t.Fatalf("gt log crash still writes logs/town.log: %v", err)
	}

	rawEvents, err := os.ReadFile(filepath.Join(townRoot, gtevents.EventsFile))
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(rawEvents)), "\n")
	if len(lines) != 1 {
		t.Fatalf("event count = %d, want 1: %s", len(lines), rawEvents)
	}

	var event gtevents.Event
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if event.Type != gtevents.TypeSessionDeath {
		t.Fatalf("event type = %q, want %q", event.Type, gtevents.TypeSessionDeath)
	}
	if event.Actor != "gastown/polecats/rust" {
		t.Fatalf("actor = %q", event.Actor)
	}
	if event.Visibility != gtevents.VisibilityFeed {
		t.Fatalf("visibility = %q", event.Visibility)
	}
	assertPayloadString(t, event.Payload, "session", "gt-gastown-rust")
	assertPayloadString(t, event.Payload, "agent", "gastown/polecats/rust")
	assertPayloadString(t, event.Payload, "reason", "crashed with exit code 42")
	assertPayloadString(t, event.Payload, "caller", "gt log crash")
	if got, ok := event.Payload["exit_code"].(float64); !ok || got != 42 {
		t.Fatalf("exit_code = %#v, want 42", event.Payload["exit_code"])
	}
}

func assertPayloadString(t *testing.T, payload map[string]interface{}, key, want string) {
	t.Helper()
	if got, ok := payload[key].(string); !ok || got != want {
		t.Fatalf("payload[%q] = %#v, want %q", key, payload[key], want)
	}
}

func TestRunLogPruneWorktreeEmitsFeedEvent(t *testing.T) {
	t.Parallel()
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := logPruneWorktree(townRoot, "deacon", "dog", "rex", "/Users/rex/gt/deacon/dogs/rex/gastown"); err != nil {
		t.Fatalf("runLogPruneWorktree: %v", err)
	}

	rawEvents, err := os.ReadFile(filepath.Join(townRoot, gtevents.EventsFile))
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(rawEvents)), "\n")
	if len(lines) != 1 {
		t.Fatalf("event count = %d, want 1: %s", len(lines), rawEvents)
	}

	var event gtevents.Event
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if event.Type != gtevents.TypeWorktreePrune {
		t.Fatalf("event type = %q, want %q", event.Type, gtevents.TypeWorktreePrune)
	}
	if event.Visibility != gtevents.VisibilityFeed {
		t.Fatalf("visibility = %q", event.Visibility)
	}
	assertPayloadString(t, event.Payload, "kind", "dog")
	assertPayloadString(t, event.Payload, "owner", "rex")
	assertPayloadString(t, event.Payload, "path", "/Users/rex/gt/deacon/dogs/rex/gastown")
}
