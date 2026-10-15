package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/overseer"
)

var overseerNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func TestOverseerLogRefusesAnUnknownKindAndListsTheAllowedOnes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out bytes.Buffer

	err := overseerLog(&out, town, "tinker", "did a thing", "", overseerNow)
	if err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	for _, k := range overseer.Kinds() {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("the refusal %q does not list %q", err, k)
		}
	}
	if _, statErr := os.Stat(overseer.JournalPath(town)); statErr == nil {
		t.Error("a refused kind wrote the journal")
	}
	if out.Len() != 0 {
		t.Errorf("a refused kind printed %q", out.String())
	}
}

func TestOverseerLogWritesOneJSONLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out bytes.Buffer

	if err := overseerLog(&out, town, "hold", "parked gt-abc\nuntil the question closes", "gt-abc", overseerNow); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(overseer.JournalPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("journal = %q, want exactly one line", raw)
	}
	var line map[string]string
	if err := json.Unmarshal(raw, &line); err != nil {
		t.Fatalf("the line is not JSON: %v", err)
	}
	want := map[string]string{
		"ts": "2026-10-15T12:00:00Z", "kind": "hold",
		"text": "parked gt-abc until the question closes", "bead": "gt-abc",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("line[%q] = %q, want %q", k, line[k], v)
		}
	}
}

func TestOverseerBeatWritesTheHeartbeat(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out bytes.Buffer

	if err := overseerBeat(&out, town, "idle", "overseer-1", overseerNow); err != nil {
		t.Fatal(err)
	}
	hb, have, err := overseer.ReadHeartbeat(town)
	if err != nil || !have {
		t.Fatalf("ReadHeartbeat = %+v, %v, %v", hb, have, err)
	}
	if !hb.TS.Equal(overseerNow) || hb.State != "idle" || hb.Session != "overseer-1" {
		t.Errorf("heartbeat = %+v", hb)
	}

	if err := overseerBeat(&out, town, "asleep", "", overseerNow); err == nil || !strings.Contains(err.Error(), "working") {
		t.Errorf("an unknown state = %v, want a refusal listing the states", err)
	}
	if after, _, _ := overseer.ReadHeartbeat(town); after.State != "idle" {
		t.Errorf("a refused beat replaced the heartbeat: %+v", after)
	}
}

// The commands are on the tree under gt overseer, and the flags the docs name
// are there.
func TestOverseerCommandsAreRegistered(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"beat", "log"} {
		c, _, err := rootCmd.Find([]string{"overseer", name})
		if err != nil || c == nil || c.Name() != name {
			t.Errorf("gt overseer %s is not registered: %v", name, err)
		}
	}
	if overseerBeatCmd.Flags().Lookup("state") == nil || overseerLogCmd.Flags().Lookup("bead") == nil {
		t.Error("gt overseer beat --state or gt overseer log --bead is missing")
	}
}
