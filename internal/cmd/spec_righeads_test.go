package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// TestTickEffRigDatabases: a rig's database is the dolt_database its metadata
// file records, not its name; a rig whose metadata names none, does not parse,
// or is absent is left out.
func TestTickEffRigDatabases(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeMetadata := func(rig, content string) {
		t.Helper()
		dir := filepath.Join(town, rig, ".beads")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata("gastown", `{"dolt_database":"gt"}`)
	writeMetadata("beads", `{"dolt_database":"be"}`)
	writeMetadata("third", `{}`)
	writeMetadata("broken", `{"dolt_database":`)

	got := rigDatabases(town, []string{"gastown", "beads", "third", "broken", "absent"})
	want := map[string]string{"gastown": "gt", "beads": "be"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rigDatabases = %v, want %v", got, want)
	}
}

// TestTickEffRigHeadsQuery: the statement is beadsql.RigHeads' for the
// databases in the order given, and a list RigHeads refuses keeps its refusal.
func TestTickEffRigHeadsQuery(t *testing.T) {
	t.Parallel()
	got, err := rigHeadsQuery([]string{"om", "gt"}).Inline()
	if err != nil {
		t.Fatalf("rigHeadsQuery: %v", err)
	}
	want, err := beadsql.RigHeads([]string{"om", "gt"}).Inline()
	if err != nil {
		t.Fatalf("beadsql.RigHeads: %v", err)
	}
	if got != want {
		t.Errorf("rigHeadsQuery = %q, want beadsql.RigHeads' %q", got, want)
	}
	if om, gt := strings.Index(got, "om_head"), strings.Index(got, "gt_head"); om < 0 || gt < 0 || om > gt {
		t.Errorf("rigHeadsQuery names om at %d and gt at %d, want om first:\n%s", om, gt, got)
	}
	bad := []struct {
		name string
		dbs  []string
	}{
		{"unsafe name", []string{"gt", "x`; DROP TABLE issues"}},
		{"no database", nil},
	}
	for _, tc := range bad {
		if _, err := rigHeadsQuery(tc.dbs).Inline(); err == nil {
			t.Errorf("%s: rigHeadsQuery = <nil>, want beadsql's refusal", tc.name)
		}
	}
}
