package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// doltDispatchTown writes a town whose rig registry names rigs, so the
// section's rows come from the registry rather than from the fixture.
func doltDispatchTown(t *testing.T, rigs ...string) string {
	t.Helper()
	town := t.TempDir()
	cfg := &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	for _, rig := range rigs {
		cfg.Rigs[rig] = config.RigEntry{GitURL: "test://" + rig}
	}
	path := filepath.Join(town, "mayor", "rigs.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRigsConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	return town
}

// doltDispatchIssues returns one bead per id, titled after it, in the order
// given: the order the lister answers in is the order the row lists.
func doltDispatchIssues(ids ...string) []*beads.Issue {
	issues := make([]*beads.Issue, 0, len(ids))
	for _, id := range ids {
		issues = append(issues, &beads.Issue{ID: id, Title: "title of " + id})
	}
	return issues
}

// Three labelled beads over two rigs give one row per rig: its count and each
// bead's id with its title.
func TestDoltDispatchSectionRowsPerRig(t *testing.T) {
	t.Parallel()

	town := doltDispatchTown(t, "alpha", "beta")
	byRig := map[string][]*beads.Issue{
		"alpha": doltDispatchIssues("gt-a", "gt-b"),
		"beta":  doltDispatchIssues("gt-c"),
	}
	list := func(_, rig string) ([]*beads.Issue, error) { return byRig[rig], nil }

	section, err := doltDispatchSection(withDoltTownRoot(context.Background(), town), list)
	if err != nil {
		t.Fatalf("doltDispatchSection: %v", err)
	}
	if got, want := section.Title, "Beads the dispatcher failed on"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
	if section.Note == "" {
		t.Error("the section has no Note naming its source")
	}
	want := [][]string{
		{"alpha", "2", "gt-a: title of gt-a, gt-b: title of gt-b"},
		{"beta", "1", "gt-c: title of gt-c"},
	}
	if !reflect.DeepEqual(section.Rows, want) {
		t.Errorf("rows = %q, want %q", section.Rows, want)
	}
}

// The section the town's panel draws is the one init registers: the registry
// carries a `dispatch` read, and that read fills the title, the note and the
// table for a town with no rigs.
func TestDoltDispatchSectionIsRegistered(t *testing.T) {
	t.Parallel()

	var read doltSectionRead
	for _, r := range doltSections.snapshot() {
		if r.key == "dispatch" {
			read = r.read
		}
	}
	if read == nil {
		t.Fatal("no section is registered under the key dispatch")
	}
	section, err := read(withDoltTownRoot(context.Background(), doltDispatchTown(t)), time.Now())
	if err != nil {
		t.Fatalf("the registered dispatch read: %v", err)
	}
	if section.Key != "" {
		t.Errorf("the read filled its own key %q; the registration owns it", section.Key)
	}
	if section.Title != "Beads the dispatcher failed on" {
		t.Errorf("Title = %q, want Beads the dispatcher failed on", section.Title)
	}
	if section.Note == "" {
		t.Error("the section has no Note naming its source")
	}
	if want := []string{"rig", "open", "beads"}; !reflect.DeepEqual(section.Head, want) {
		t.Errorf("Head = %q, want %q", section.Head, want)
	}
	if len(section.Rows) != 0 {
		t.Errorf("a town with no rigs draws rows %q, want none", section.Rows)
	}
}

// Fourteen labelled beads in one rig list the first ten with their titles, say
// how many more there are, and still count all fourteen.
func TestDoltDispatchSectionCapsTheListAtTen(t *testing.T) {
	t.Parallel()

	town := doltDispatchTown(t, "alpha")
	ids := make([]string, 0, 14)
	for i := 1; i <= 14; i++ {
		ids = append(ids, fmt.Sprintf("gt-%02d", i))
	}
	list := func(_, rig string) ([]*beads.Issue, error) { return doltDispatchIssues(ids...), nil }

	section, err := doltDispatchSection(withDoltTownRoot(context.Background(), town), list)
	if err != nil {
		t.Fatalf("doltDispatchSection: %v", err)
	}
	if len(section.Rows) != 1 {
		t.Fatalf("rows = %q, want one rig's row", section.Rows)
	}
	row := section.Rows[0]
	if row[0] != "alpha" || row[1] != "14" {
		t.Errorf("the row reads %q, count, want the rig and 14", row[:2])
	}
	for i := 1; i <= 10; i++ {
		if want := fmt.Sprintf("gt-%02d: title of gt-%02d", i, i); !strings.Contains(row[2], want) {
			t.Errorf("the row does not list %q: %q", want, row[2])
		}
	}
	for i := 11; i <= 14; i++ {
		if id := fmt.Sprintf("gt-%02d", i); strings.Contains(row[2], id) {
			t.Errorf("the row lists %q past the tenth bead: %q", id, row[2])
		}
	}
	if !strings.Contains(row[2], "+4 more") {
		t.Errorf("the row does not say +4 more: %q", row[2])
	}
}

// One rig whose store cannot be listed reads unreadable in its own row, and the
// rigs around it still draw.
func TestDoltDispatchSectionMarksAnUnreadableRig(t *testing.T) {
	t.Parallel()

	town := doltDispatchTown(t, "alpha", "beta")
	boom := errors.New("bd list: connection refused")
	list := func(_, rig string) ([]*beads.Issue, error) {
		if rig == "alpha" {
			return nil, boom
		}
		return doltDispatchIssues("gt-c"), nil
	}

	section, err := doltDispatchSection(withDoltTownRoot(context.Background(), town), list)
	if err != nil {
		t.Fatalf("one rig's list failed the whole section: %v", err)
	}
	want := [][]string{
		{"alpha", "?", "unreadable: " + boom.Error()},
		{"beta", "1", "gt-c: title of gt-c"},
	}
	if !reflect.DeepEqual(section.Rows, want) {
		t.Errorf("rows = %q, want %q", section.Rows, want)
	}
}

// A registry entry with no name would resolve to the town's own store, so its
// row reads unreadable and no rig is listed in its place.
func TestDoltDispatchSectionRefusesAnUnnamedRig(t *testing.T) {
	t.Parallel()

	town := doltDispatchTown(t, "", "alpha")
	list := func(_, rig string) ([]*beads.Issue, error) {
		if rig == "" {
			t.Error("the read listed a store for an unnamed rig")
		}
		return doltDispatchIssues("gt-a"), nil
	}

	section, err := doltDispatchSection(withDoltTownRoot(context.Background(), town), list)
	if err != nil {
		t.Fatalf("doltDispatchSection: %v", err)
	}
	want := [][]string{
		{"", "?", "unreadable: the registry names no rig"},
		{"alpha", "1", "gt-a: title of gt-a"},
	}
	if !reflect.DeepEqual(section.Rows, want) {
		t.Errorf("rows = %q, want %q", section.Rows, want)
	}
}

// A section read with no town root, or over a registry that cannot be read, is
// an error rather than an empty table: an empty table would read as no bead
// being held out of the queue.
func TestDoltDispatchSectionRefusesWithoutATownRootOrRegistry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "no town root", ctx: context.Background(), want: "town root"},
		{name: "no registry", ctx: withDoltTownRoot(context.Background(), t.TempDir()), want: "rig registry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			list := func(string, string) ([]*beads.Issue, error) {
				t.Error("the read listed a rig it had no registry for")
				return nil, nil
			}
			section, err := doltDispatchSection(tc.ctx, list)
			if err == nil {
				t.Fatalf("the read returned %+v, want an error naming the %s", section, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name the %s", err, tc.want)
			}
		})
	}
}
