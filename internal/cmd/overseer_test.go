package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
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

// A working beat is refused while attention items are unacknowledged or
// escalations are open, and the refusal names them; the heartbeat stays as it
// was. The readers are passed in, so the test needs no beads; the heartbeat it checks is a file
// in a temp town.
func TestOverseerBeatRefusesWorkingWhileItemsAreOpen(t *testing.T) {
	t.Parallel()
	open := func(keys []string, escalations int) overseerOpenWork {
		return overseerOpenWork{
			items:       func(string) ([]string, error) { return keys, nil },
			escalations: func(string) (int, error) { return escalations, nil },
		}
	}

	town := t.TempDir()
	var out bytes.Buffer
	busy := open([]string{"escalation:hq-a", "risk:gt-b:1"}, 1)
	if err := overseerBeatWith(&out, town, overseer.StateWorking, "overseer", overseerNow, busy); err == nil {
		t.Fatal("a working beat with open items was recorded")
	} else {
		for _, want := range []string{"2 unacked attention item(s)", "1 open escalation(s)", "escalation:hq-a", "risk:gt-b:1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal = %q, want it to name %q", err, want)
			}
		}
	}
	if hb, have, _ := overseer.ReadHeartbeat(town); have {
		t.Errorf("a refused working beat wrote a heartbeat: %+v", hb)
	}

	// Other states are not refused: an idle overseer may still record itself.
	if err := overseerBeatWith(&out, town, overseer.StateIdle, "overseer", overseerNow, busy); err != nil {
		t.Errorf("an idle beat with open items was refused: %v", err)
	}

	// Once everything is triaged, working is recorded.
	if err := overseerBeatWith(&out, town, overseer.StateWorking, "overseer", overseerNow, open(nil, 0)); err != nil {
		t.Errorf("a working beat with nothing open was refused: %v", err)
	}

	// A reader that fails refuses the beat instead of recording it.
	broken := overseerOpenWork{
		items:       func(string) ([]string, error) { return nil, errors.New("unreadable") },
		escalations: func(string) (int, error) { return 0, nil },
	}
	if err := overseerBeatWith(&out, t.TempDir(), overseer.StateWorking, "overseer", overseerNow, broken); err == nil || !strings.Contains(err.Error(), "reading attention") {
		t.Errorf("a failing attention reader = %v, want a refusal naming it", err)
	}
}

// TestOverseerBeatLiveReadersRefuseWorkingAgainstATown runs the production readers, the attention
// file and the bead plane's escalation count, against a temp town and an in-memory bead client, so
// the wiring liveOverseerOpenWork builds is tested without a bd process (gt-4tbmv).
func TestOverseerBeatLiveReadersRefuseWorkingAgainstATown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	db := beadsfake.New()
	live := liveOverseerOpenWork(func(string) beads.Client { return db })
	var out bytes.Buffer
	acked := overseerNow

	// An unacknowledged item is refused and named, quoted; an acknowledged one is not counted.
	writeItems := func(items ...attention.Item) {
		t.Helper()
		if err := attention.WriteState(town, attention.State{Updated: overseerNow, Items: items}); err != nil {
			t.Fatalf("writing the attention state: %v", err)
		}
	}
	writeItems(attention.Item{Key: "risk:gt-b:1"}, attention.Item{Key: "red-main:gastown:internal/cmd", AckedAt: &acked})
	err := overseerBeatWith(&out, town, overseer.StateWorking, "overseer", overseerNow, live)
	if err == nil || !strings.Contains(err.Error(), `"risk:gt-b:1"`) || strings.Contains(err.Error(), "red-main") {
		t.Fatalf("working with an unacked item = %v, want a refusal naming only the unacked key", err)
	}

	// A daemon-raised escalation is in the bead plane and as an esc: item: it is named once.
	db.Seed(beads.Issue{ID: "hq-esc1", Status: "open", Labels: []string{"gt:escalation"}})
	writeItems(attention.Item{Key: "esc:hq-esc1"})
	err = overseerBeatWith(&out, town, overseer.StateWorking, "overseer", overseerNow, live)
	if err == nil || !strings.Contains(err.Error(), "0 open escalation(s) with no attention item") || !strings.Contains(err.Error(), `"esc:hq-esc1"`) {
		t.Fatalf("working with an escalation listed as an item = %v, want it named once, as an item", err)
	}

	// Everything triaged and no escalation open: working is recorded.
	writeItems(attention.Item{Key: "esc:hq-esc1", AckedAt: &acked})
	if err := overseerBeatWith(&out, town, overseer.StateWorking, "overseer", overseerNow, liveOverseerOpenWork(func(string) beads.Client { return beadsfake.New() })); err != nil {
		t.Fatalf("working with nothing open = %v, want the heartbeat recorded", err)
	}
	if _, have, _ := overseer.ReadHeartbeat(town); !have {
		t.Error("the working beat with nothing open wrote no heartbeat")
	}
}

// TestOverseerRefusalQuotesEachKey: keys are printed quoted, so a key with a newline or a quote
// cannot break the refusal's lines (gt-4tbmv).
func TestOverseerRefusalQuotesEachKey(t *testing.T) {
	t.Parallel()
	open := overseerOpenWork{
		items:       func(string) ([]string, error) { return []string{"risk:gt-b:1\nforged line"}, nil },
		escalations: func(string) (int, error) { return 0, nil },
	}
	var out bytes.Buffer
	err := overseerBeatWith(&out, t.TempDir(), overseer.StateWorking, "overseer", overseerNow, open)
	if err == nil || strings.Contains(err.Error(), "\nforged line") || !strings.Contains(err.Error(), `"risk:gt-b:1\nforged line"`) {
		t.Fatalf("refusal = %v, want the key quoted so its newline cannot start a line", err)
	}
}
