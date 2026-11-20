package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/polecat"
)

// doltRespawnCounted builds one bead's entry as polecat.RespawnCounts reports
// it: at the limit once its count has reached it.
func doltRespawnCounted(bead string, count, limit int) polecat.RespawnCount {
	return polecat.RespawnCount{Bead: bead, Count: count, Limit: limit, AtLimit: count >= limit}
}

// doltRespawnTown writes a town whose respawn state tracks the given counts, so
// the registered read runs polecat.RespawnCounts over a fixture rather than
// the live town.
func doltRespawnTown(t *testing.T, counts map[string]int) string {
	t.Helper()
	town := t.TempDir()
	beads := make(map[string]any, len(counts))
	for bead, count := range counts {
		beads[bead] = map[string]any{"bead_id": bead, "count": count}
	}
	data, err := json.Marshal(map[string]any{"beads": beads})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(town, constants.DirRuntime, "bead-respawn-counts.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// Two beads at the limit and one a count under draw two rows in the at-limit
// group and one in the one-under group, each with its count and the limit.
func TestDoltRespawnSectionSplitsTheGroups(t *testing.T) {
	t.Parallel()

	section := doltRespawnSection([]polecat.RespawnCount{
		doltRespawnCounted("gt-a", 3, 3),
		doltRespawnCounted("gt-b", 3, 3),
		doltRespawnCounted("gt-c", 2, 3),
	})
	if got, want := section.Title, "Beads at the respawn limit"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
	if want := []string{"group", "bead", "count", "limit"}; !reflect.DeepEqual(section.Head, want) {
		t.Errorf("Head = %q, want %q", section.Head, want)
	}
	want := [][]string{
		{"at limit", "gt-a", "3", "3"},
		{"at limit", "gt-b", "3", "3"},
		{"one under", "gt-c", "2", "3"},
	}
	if !reflect.DeepEqual(section.Rows, want) {
		t.Errorf("Rows = %q, want %q", section.Rows, want)
	}
}

// Fourteen beads at the limit list the first ten and say "+4 more" on their own
// row; the four past the tenth are not listed.
func TestDoltRespawnSectionCapsAtTen(t *testing.T) {
	t.Parallel()

	counts := make([]polecat.RespawnCount, 0, 14)
	for i := 1; i <= 14; i++ {
		counts = append(counts, doltRespawnCounted(fmt.Sprintf("gt-%02d", i), 3, 3))
	}

	section := doltRespawnSection(counts)
	if len(section.Rows) != 11 {
		t.Fatalf("Rows = %q, want the ten beads and the +4 more line", section.Rows)
	}
	for i := 1; i <= 10; i++ {
		want := []string{"at limit", fmt.Sprintf("gt-%02d", i), "3", "3"}
		if got := section.Rows[i-1]; !reflect.DeepEqual(got, want) {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
	}
	if want := []string{"at limit", "+4 more", "", ""}; !reflect.DeepEqual(section.Rows[10], want) {
		t.Errorf("the summary row = %q, want %q", section.Rows[10], want)
	}
	for _, row := range section.Rows {
		if row[1] == "gt-11" || row[1] == "gt-12" || row[1] == "gt-13" || row[1] == "gt-14" {
			t.Errorf("the section lists %q past the tenth bead", row[1])
		}
	}
}

// A limit of zero leaves no bead one count under it: a state holding a negative
// count would otherwise draw as "one under" a limit nothing can be under.
func TestDoltRespawnSectionHasNoOneUnderGroupBelowOne(t *testing.T) {
	t.Parallel()

	section := doltRespawnSection([]polecat.RespawnCount{
		doltRespawnCounted("gt-at", 0, 0),
		{Bead: "gt-corrupt", Count: -1, Limit: 0},
	})
	want := [][]string{{"at limit", "gt-at", "0", "0"}}
	if !reflect.DeepEqual(section.Rows, want) {
		t.Errorf("Rows = %q, want %q", section.Rows, want)
	}
}

// A reader that cannot read the state fails the section with an Err and no
// rows: an empty table would read as no bead being at the limit.
func TestDoltRespawnSectionReaderErrorIsItsErr(t *testing.T) {
	t.Parallel()

	boom := fmt.Errorf("reading respawn state: truncated")
	read := func(ctx context.Context, _ time.Time) (dashboard.DoltSection, error) {
		return readDoltRespawnSection(ctx, func(string) ([]polecat.RespawnCount, error) {
			return nil, boom
		})
	}
	got := readDoltSection(doltSectionReader{key: "respawn", read: read}, t.TempDir(), time.Second, time.Now())
	if got.Err == "" {
		t.Fatalf("the failed read drew %+v, want an Err", got)
	}
	if !strings.Contains(got.Err, "truncated") {
		t.Errorf("Err = %q, want it to carry the reader's reason", got.Err)
	}
	if got.Head != nil || got.Rows != nil {
		t.Errorf("the failed section carries a table: Head=%q Rows=%q", got.Head, got.Rows)
	}
}

// The section is registered under `respawn`, refuses a context without a town
// root, and its registered read lists the fixture town's beads through
// polecat.RespawnCounts.
func TestDoltRespawnSectionIsRegistered(t *testing.T) {
	t.Parallel()

	var read doltSectionRead
	for _, r := range doltSections.snapshot() {
		if r.key == "respawn" {
			read = r.read
		}
	}
	if read == nil {
		t.Fatal("no section is registered under the key respawn")
	}
	if _, err := read(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "town root") {
		t.Errorf("the registered read with no town root answered %v, want a refusal naming the missing root", err)
	}

	limit := config.DefaultRecoveryMaxBeadRespawns
	town := doltRespawnTown(t, map[string]int{"gt-limit": limit, "gt-under": limit - 1, "gt-far": 1})
	section, err := read(withDoltTownRoot(context.Background(), town), time.Now())
	if err != nil {
		t.Fatalf("the registered respawn read: %v", err)
	}
	if section.Key != "" {
		t.Errorf("the read filled its own key %q; the registration owns it", section.Key)
	}
	if section.Title != "Beads at the respawn limit" {
		t.Errorf("Title = %q, want Beads at the respawn limit", section.Title)
	}
	want := [][]string{
		{"at limit", "gt-limit", strconv.Itoa(limit), strconv.Itoa(limit)},
		{"one under", "gt-under", strconv.Itoa(limit - 1), strconv.Itoa(limit)},
	}
	if !reflect.DeepEqual(section.Rows, want) {
		t.Errorf("Rows = %q, want %q", section.Rows, want)
	}
}
