package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// journalTown writes a town whose rigs registry names rigs, so the section's
// store list and its order come from the section rather than the fixture.
func journalTown(t *testing.T, rigs ...string) string {
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

// Each store's value becomes its own row, read from the store's config: hq
// first, then the rigs in registry order, and the value read the way bd reads
// it, so an unset store is drawn as unset and off.
func TestDoltJournalSectionStates(t *testing.T) {
	t.Parallel()

	town := journalTown(t, "beta", "alpha")
	values := map[string]string{"hq": "true", "alpha": "false", "beta": ""}
	section, err := doltJournalSection(town, func(store string) (string, error) {
		return values[store], nil
	})
	if err != nil {
		t.Fatalf("doltJournalSection: %v", err)
	}
	if section.Err != "" {
		t.Errorf("the section carries Err %q, want none", section.Err)
	}
	if got, want := section.Head, []string{"store", "value", "state"}; !slices.Equal(got, want) {
		t.Errorf("Head = %v, want %v", got, want)
	}
	want := [][]string{
		{"hq", "true", "on"},
		{"alpha", "false", "off"},
		{"beta", "", "unset (off)"},
	}
	if !slices.EqualFunc(section.Rows, want, slices.Equal) {
		t.Errorf("Rows = %v, want %v", section.Rows, want)
	}
}

// One store that cannot answer keeps its own row with the reason and leaves
// every other store's row as it was: a store the read cannot reach is not a
// store with no setting, and it never costs the panel another row.
func TestDoltJournalSectionStoreErrorIsItsRow(t *testing.T) {
	t.Parallel()

	town := journalTown(t, "alpha", "beta")
	boom := errors.New("config read failed")
	section, err := doltJournalSection(town, func(store string) (string, error) {
		if store == "alpha" {
			return "", boom
		}
		return "true", nil
	})
	if err != nil {
		t.Fatalf("one store's failure failed the whole section: %v", err)
	}
	want := [][]string{
		{"hq", "true", "on"},
		{"alpha", "", "unreadable: " + boom.Error()},
		{"beta", "true", "on"},
	}
	if !slices.EqualFunc(section.Rows, want, slices.Equal) {
		t.Errorf("Rows = %v, want %v", section.Rows, want)
	}
}

// A registry the section cannot read fails the section rather than drawing hq
// alone, as a town of one store reads the same as a town whose rigs all fell
// away.
func TestDoltJournalSectionRefusesAnUnreadableRegistry(t *testing.T) {
	t.Parallel()

	read := false
	section, err := doltJournalSection(t.TempDir(), func(string) (string, error) {
		read = true
		return "true", nil
	})
	if err == nil {
		t.Fatal("a town with no rig registry read as a filled section")
	}
	if section.Head != nil || section.Rows != nil {
		t.Errorf("the failed section carries a table: %+v", section)
	}
	if read {
		t.Error("a store was read before the registry")
	}
}

// A store with no beads directory is refused for its row. `bd` run from a store
// with an unset directory would answer from whichever store it finds from the
// town directory, so the row would name one store and show another's setting.
func TestJournalConfigGetRefusesAStoreWithNoDirectory(t *testing.T) {
	t.Parallel()

	_, err := journalConfigGet(t.TempDir())("")
	if err == nil || !strings.Contains(err.Error(), "no beads directory") {
		t.Errorf("a store with no beads directory read as %v, want the refusal", err)
	}
}

// The section is registered under `journal` by its own file, its note says the
// value is read live, and the registered read refuses a context without a town
// root rather than reading the empty one.
func TestDoltJournalSectionRegisters(t *testing.T) {
	t.Parallel()

	var registered *doltSectionReader
	for _, r := range doltSections.snapshot() {
		if r.key == "journal" {
			registered = &r
		}
	}
	if registered == nil {
		t.Fatal("the Dolt panel has no journal section registered")
	}

	// The registered read reaches the town's stores: a town whose registry is
	// missing fails the section, so a read that answered here would have drawn
	// a panel without asking which stores exist.
	ctx := withDoltTownRoot(context.Background(), t.TempDir())
	if section, err := registered.read(ctx, time.Now()); err == nil {
		t.Fatalf("the registered read answered %+v for a town with no registry", section)
	}
	if _, err := registered.read(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "no town root") {
		t.Errorf("the registered read with no town root answered %v, want a refusal naming the missing root", err)
	}

	section, err := doltJournalSection(journalTown(t), func(string) (string, error) {
		return "true", nil
	})
	if err != nil {
		t.Fatalf("doltJournalSection: %v", err)
	}
	if got, want := section.Title, "Events journal"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
	if !strings.Contains(section.Note, "live") {
		t.Errorf("Note = %q, want it to say the value is read live", section.Note)
	}
}
