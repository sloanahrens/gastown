package channelevents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmitToTown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	path, err := EmitToTown(townRoot, "refinery", "dashboard", "MERGE_READY", []string{
		"source=witness",
		"rig=dashboard",
	})
	if err != nil {
		t.Fatalf("EmitToTown failed: %v", err)
	}

	if !strings.HasSuffix(path, ".event") {
		t.Errorf("expected .event suffix, got %q", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading event file: %v", err)
	}

	var event map[string]interface{}
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("unmarshaling event: %v", err)
	}

	if event["type"] != "MERGE_READY" {
		t.Errorf("type = %v, want MERGE_READY", event["type"])
	}
	if event["channel"] != "refinery" {
		t.Errorf("channel = %v, want refinery", event["channel"])
	}
	if event["rig"] != "dashboard" {
		t.Errorf("rig = %v, want dashboard", event["rig"])
	}

	payload, ok := event["payload"].(map[string]interface{})
	if !ok {
		t.Fatal("payload is not a map")
	}
	if payload["source"] != "witness" {
		t.Errorf("payload.source = %v, want witness", payload["source"])
	}
	if payload["rig"] != "dashboard" {
		t.Errorf("payload.rig = %v, want dashboard", payload["rig"])
	}
}

func TestEmitToTown_PerRigChannelScopedDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Per-rig channels land in events/<channel>/<rig>/, so one rig's
	// consumer can never see (or delete) another rig's events (gt-dsj).
	pathA, err := EmitToTown(townRoot, "refinery", "riga", "MQ_SUBMIT", nil)
	if err != nil {
		t.Fatalf("emit for riga failed: %v", err)
	}
	pathB, err := EmitToTown(townRoot, "refinery", "rigb", "MQ_SUBMIT", nil)
	if err != nil {
		t.Fatalf("emit for rigb failed: %v", err)
	}

	wantDirA := filepath.Join(townRoot, "events", "refinery", "riga")
	wantDirB := filepath.Join(townRoot, "events", "refinery", "rigb")
	if filepath.Dir(pathA) != wantDirA {
		t.Errorf("riga event dir = %q, want %q", filepath.Dir(pathA), wantDirA)
	}
	if filepath.Dir(pathB) != wantDirB {
		t.Errorf("rigb event dir = %q, want %q", filepath.Dir(pathB), wantDirB)
	}
}

func TestEmitToTown_PerRigChannelRequiresRig(t *testing.T) {
	t.Parallel()
	for _, channel := range []string{"refinery", "witness"} {
		if _, err := EmitToTown(t.TempDir(), channel, "", "TEST", nil); err == nil {
			t.Errorf("expected error emitting on per-rig channel %q without a rig", channel)
		}
	}
}

func TestEmitToTown_PerRigChannelInvalidRig(t *testing.T) {
	t.Parallel()
	if _, err := EmitToTown(t.TempDir(), "refinery", "../escape", "TEST", nil); err == nil {
		t.Error("expected error for invalid rig name")
	}
}

func TestEmitToTown_GlobalChannelIgnoresRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// Town-global channels (single consumer) stay flat even when the
	// emitter runs in a rig context.
	path, err := EmitToTown(townRoot, "mayor", "gastown", "SLOT_OPEN", nil)
	if err != nil {
		t.Fatalf("EmitToTown failed: %v", err)
	}
	wantDir := filepath.Join(townRoot, "events", "mayor")
	if filepath.Dir(path) != wantDir {
		t.Errorf("event dir = %q, want %q", filepath.Dir(path), wantDir)
	}
}

func TestEmitToTown_InvalidChannel(t *testing.T) {
	t.Parallel()
	_, err := EmitToTown(t.TempDir(), "../escape", "", "TEST", nil)
	if err == nil {
		t.Error("expected error for invalid channel name")
	}
}

func TestEmitToTown_UniqueFilenames(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	seen := make(map[string]bool)

	for i := 0; i < 10; i++ {
		path, err := EmitToTown(townRoot, "test", "", "EVENT", nil)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if seen[path] {
			t.Errorf("duplicate filename: %s", path)
		}
		seen[path] = true
	}
}

func TestValidChannelName(t *testing.T) {
	t.Parallel()
	valid := []string{"refinery", "witness", "my-channel", "test_chan", "abc123"}
	for _, name := range valid {
		if !ValidChannelName.MatchString(name) {
			t.Errorf("%q should be valid", name)
		}
	}

	invalid := []string{"../escape", "has space", "has/slash", "", "has.dot"}
	for _, name := range invalid {
		if ValidChannelName.MatchString(name) {
			t.Errorf("%q should be invalid", name)
		}
	}
}

func TestIsPerRig(t *testing.T) {
	t.Parallel()
	for _, channel := range []string{"refinery", "witness"} {
		if !IsPerRig(channel) {
			t.Errorf("IsPerRig(%q) = false, want true", channel)
		}
	}
	for _, channel := range []string{"mayor", "other"} {
		if IsPerRig(channel) {
			t.Errorf("IsPerRig(%q) = true, want false", channel)
		}
	}
}

func TestDir(t *testing.T) {
	t.Parallel()
	townRoot := "/town"

	if got, want := Dir(townRoot, "refinery", "gastown"), filepath.Join(townRoot, "events", "refinery", "gastown"); got != want {
		t.Errorf("Dir per-rig = %q, want %q", got, want)
	}
	if got, want := Dir(townRoot, "mayor", "gastown"), filepath.Join(townRoot, "events", "mayor"); got != want {
		t.Errorf("Dir global = %q, want %q", got, want)
	}
}

func TestEmitToTown_CreatesDirectory(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	channelDir := filepath.Join(townRoot, "events", "newchannel")

	if _, err := os.Stat(channelDir); !os.IsNotExist(err) {
		t.Fatal("channel dir should not exist yet")
	}

	_, err := EmitToTown(townRoot, "newchannel", "", "TEST", nil)
	if err != nil {
		t.Fatalf("EmitToTown failed: %v", err)
	}

	if _, err := os.Stat(channelDir); err != nil {
		t.Errorf("channel dir should exist after emit: %v", err)
	}
}
