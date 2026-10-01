package cmd

import (
	"testing"
)

// TestBdListChildren_FallsBackToDepsTable is the regression test for GH #3700:
// `bd list --parent=<epic>` returned `[]` even though parent-child links
// existed. The deps table is authoritative, so an empty primary result
// consults it.
func TestBdListChildren_FallsBackToDepsTable(t *testing.T) {
	t.Parallel()
	for _, out := range []string{"[]\n", "", "  "} {
		var asked string
		viaDeps := func(parentID string) ([]bdShowResult, error) {
			asked = parentID
			return []bdShowResult{{ID: "ha-c1"}, {ID: "ha-c2"}}, nil
		}
		got, err := childrenOrDepsFallback("ha-epic", []byte(out), viaDeps)
		if err != nil {
			t.Fatalf("childrenOrDepsFallback(%q): %v", out, err)
		}
		if asked != "ha-epic" || len(got) != 2 || got[0].ID != "ha-c1" || got[1].ID != "ha-c2" {
			t.Errorf("childrenOrDepsFallback(%q) = %+v (deps asked for %q), want ha-c1 and ha-c2 from the deps table", out, got, asked)
		}
	}
}

// TestBdListChildren_PrimaryPathStillUsed verifies the fallback only kicks in
// when the primary `bd list --parent` path returns empty.
func TestBdListChildren_PrimaryPathStillUsed(t *testing.T) {
	t.Parallel()
	viaDeps := func(string) ([]bdShowResult, error) {
		t.Fatal("deps fallback consulted although the primary path returned rows")
		return nil, nil
	}
	out := `[{"id":"ha-direct","title":"Direct child","status":"open","issue_type":"task"}]`
	got, err := childrenOrDepsFallback("ha-epic", []byte(out), viaDeps)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ha-direct" {
		t.Fatalf("expected single ha-direct child from primary path, got %+v", got)
	}
}
