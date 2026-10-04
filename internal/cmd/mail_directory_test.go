package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// fakeDirSource is a town's addressable beads; a nil map fails that listing
// the way bd does in a town with no database.
type fakeDirSource struct {
	agents   map[string]*beads.Issue
	groups   map[string]*beads.GroupFields
	queues   map[string]*beads.Issue
	channels map[string]*beads.ChannelFields
}

var errNoDatabase = errors.New("no beads database found")

func (f fakeDirSource) ListAgentBeads() (map[string]*beads.Issue, error) {
	if f.agents == nil {
		return nil, errNoDatabase
	}
	return f.agents, nil
}

func (f fakeDirSource) ListGroupBeads() (map[string]*beads.GroupFields, error) {
	if f.groups == nil {
		return nil, errNoDatabase
	}
	return f.groups, nil
}

func (f fakeDirSource) ListQueueBeads() (map[string]*beads.Issue, error) {
	if f.queues == nil {
		return nil, errNoDatabase
	}
	return f.queues, nil
}

func (f fakeDirSource) ListChannelBeads() (map[string]*beads.ChannelFields, error) {
	if f.channels == nil {
		return nil, errNoDatabase
	}
	return f.channels, nil
}

func mailDirectoryJSON(t *testing.T, src mailDirectorySource) []DirectoryEntry {
	t.Helper()
	var out bytes.Buffer
	if err := writeMailDirectory(&out, io.Discard, src, true); err != nil {
		t.Fatalf("writeMailDirectory: %v", err)
	}
	var entries []DirectoryEntry
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal JSON output: %v\nraw output:\n%s", err, out.String())
	}
	return entries
}

// TestRunMailDirectory_WellKnownAddresses: with no beads database every
// listing warns, and the well-known and special addresses are still listed
// in the table.
func TestRunMailDirectory_WellKnownAddresses(t *testing.T) {
	t.Parallel()
	var out, warn bytes.Buffer
	if err := writeMailDirectory(&out, &warn, fakeDirSource{}, false); err != nil {
		t.Fatalf("writeMailDirectory: %v", err)
	}
	output := out.String()
	for _, addr := range []string{"--self", "@town", "@crew", "@witnesses", "@overseer", "ADDRESS", "TYPE", "(4 warnings)"} {
		if !strings.Contains(output, addr) {
			t.Errorf("output lacks %q:\n%s", addr, output)
		}
	}
	// gt mail send has no --human flag, so the directory must not offer it.
	if strings.Contains(output, "--human") {
		t.Errorf("output still lists --human:\n%s", output)
	}
	if strings.Count(warn.String(), "warning:") != 4 {
		t.Errorf("warnings:\n%s", warn.String())
	}
}

// TestRunMailDirectory_JSONOutput lists every kind of address as JSON.
func TestRunMailDirectory_JSONOutput(t *testing.T) {
	t.Parallel()
	entries := mailDirectoryJSON(t, fakeDirSource{
		agents:   map[string]*beads.Issue{"gt-gastown-witness": {}},
		groups:   map[string]*beads.GroupFields{"ops": {}},
		queues:   map[string]*beads.Issue{"hq-q1": {Description: "name: work"}, "hq-q2": {Description: ""}},
		channels: map[string]*beads.ChannelFields{"alerts": {}},
	})
	got := map[string]string{}
	for _, e := range entries {
		got[e.Address] = e.Type
	}
	for addr, typ := range map[string]string{"gastown/witness": "agent", "group:ops": "group", "queue:work": "queue", "channel:alerts": "channel", "@town": "special"} {
		if got[addr] != typ {
			t.Errorf("address %q has type %q, want %q (all: %v)", addr, got[addr], typ, got)
		}
	}
	if _, ok := got["queue:"]; ok {
		t.Error("a queue with no name field was listed")
	}
	if _, ok := got["--human"]; ok {
		t.Error("--human listed, but gt mail send rejects it")
	}
}

// TestRunMailDirectory_Deduplication: every address is listed once, however
// many sources name it.
func TestRunMailDirectory_Deduplication(t *testing.T) {
	t.Parallel()
	entries := mailDirectoryJSON(t, fakeDirSource{
		agents:   map[string]*beads.Issue{"gt-gastown-witness": {}, "gt-alpha-witness": {}},
		groups:   map[string]*beads.GroupFields{"ops": {}},
		queues:   map[string]*beads.Issue{"hq-q1": {Description: "name: work"}},
		channels: map[string]*beads.ChannelFields{"alerts": {}},
	})
	seen := map[string]int{}
	for _, e := range entries {
		seen[e.Address]++
	}
	if len(seen) == 0 {
		t.Fatal("no addresses listed")
	}
	for addr, n := range seen {
		if n > 1 {
			t.Errorf("address %q appears %d times", addr, n)
		}
	}
}

// TestRunMailDirectory_SortOrder: entries sort by type, then address.
func TestRunMailDirectory_SortOrder(t *testing.T) {
	t.Parallel()
	entries := mailDirectoryJSON(t, fakeDirSource{
		agents: map[string]*beads.Issue{"gt-gastown-witness": {}, "gt-alpha-witness": {}},
		groups: map[string]*beads.GroupFields{"zeta": {}, "alpha": {}},
	})
	for i := 1; i < len(entries); i++ {
		prev, curr := entries[i-1], entries[i]
		if prev.Type > curr.Type || (prev.Type == curr.Type && prev.Address > curr.Address) {
			t.Errorf("%q (%s) sorts before %q (%s)", prev.Address, prev.Type, curr.Address, curr.Type)
		}
	}
}

func TestDirectoryEntry_JSONTags(t *testing.T) {
	t.Parallel()
	e := DirectoryEntry{Address: "gastown/witness", Type: "agent"}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if raw["address"] != "gastown/witness" {
		t.Errorf("JSON key should be 'address', got: %v", raw)
	}
	if raw["type"] != "agent" {
		t.Errorf("JSON key should be 'type', got: %v", raw)
	}
}
