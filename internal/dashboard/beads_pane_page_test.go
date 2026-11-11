package dashboard

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var pageColumnSectionRE = regexp.MustCompile(`<section\b[^>]*\bdata-fold="([^"]+)"`)

// pageColumnKeys is the fold key of every section in each column of the grid,
// in the order index.html declares them. The read stops at the comment above
// the full-width Landings block, so the Landings and Polecats sections are not
// in either column.
func pageColumnKeys(t *testing.T) (left, right []string) {
	t.Helper()

	page := string(indexHTML)
	end := strings.Index(page, "<!-- Below the grid")
	if end < 0 {
		t.Fatal("index.html has no comment marking the end of the grid")
	}
	cols := strings.Split(page[:end], `<div class="col">`)
	if len(cols) != 3 {
		t.Fatalf("index.html has %d columns, want 2", len(cols)-1)
	}
	keys := func(col string) []string {
		var out []string
		for _, m := range pageColumnSectionRE.FindAllStringSubmatch(col, -1) {
			out = append(out, m[1])
		}
		return out
	}
	return keys(cols[1]), keys(cols[2])
}

// The Beads pane sits in the left column directly under Tier sweeps, the
// operator's layout for the dashboard (gt-70ortp). The check reads the
// embedded markup's column order, not the source text around one id.
func TestBeadsPaneSitsDirectlyUnderTierSweeps(t *testing.T) {
	t.Parallel()

	left, right := pageColumnKeys(t)
	at := -1
	for i, key := range left {
		if key == "beads" {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("the left column holds %v, want the beads pane in it", left)
	}
	if left[at-1] != "tiersweep" {
		t.Errorf("the pane above beads in the left column is %q, want tiersweep", left[at-1])
	}
	for _, key := range right {
		if key == "beads" {
			t.Error("the beads pane is also in the right column")
		}
	}
}

// Moving the Beads pane leaves the other panes in their columns and in their
// order: the left column ends with Beads after Tier sweeps, and the right
// column holds the same panes it did, without Beads.
func TestOtherPanesKeepTheirColumnsAndOrder(t *testing.T) {
	t.Parallel()

	left, right := pageColumnKeys(t)
	wantLeft := []string{"birds", "attention", "events", "cost", "rigs", "tiersweep", "beads"}
	wantRight := []string{"news", "machine", "cloud", "forgejo", "om", "jev", "mango"}
	if !reflect.DeepEqual(left, wantLeft) {
		t.Errorf("the left column holds %v, want %v", left, wantLeft)
	}
	if !reflect.DeepEqual(right, wantRight) {
		t.Errorf("the right column holds %v, want %v", right, wantRight)
	}
}

// The page's sections, as the fake DOM folds them, list Beads directly after
// Tier sweeps. The fake DOM lays sections out flat, so this reads document
// order only; TestBeadsPaneSitsDirectlyUnderTierSweeps checks the column.
func TestBeadsFoldFollowsTierSweepsOnThePage(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	keys := dom.foldKeys(t)
	at := -1
	for i, key := range keys {
		if key == "beads" {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("the page folds sections %v, want a beads section", keys)
	}
	if keys[at-1] != "tiersweep" {
		t.Errorf("the page folds %q directly before beads, want tiersweep", keys[at-1])
	}
}
