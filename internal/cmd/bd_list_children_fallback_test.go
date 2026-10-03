package cmd

import (
	"testing"
)

// TestBdListChildren_FallsBackToDepsTable is the regression test for GH #3700:
// the children read returned no rows even though parent-child links existed.
// The deps table is authoritative, so an empty primary result consults it.
func TestBdListChildren_FallsBackToDepsTable(t *testing.T) {
	t.Parallel()
	var asked string
	viaDeps := func(parentID string) ([]bdShowResult, error) {
		asked = parentID
		return []bdShowResult{{ID: "ha-c1"}, {ID: "ha-c2"}}, nil
	}
	got, err := childrenOrDepsFallback(nil, "ha-epic", viaDeps)
	if err != nil {
		t.Fatalf("childrenOrDepsFallback: %v", err)
	}
	if asked != "ha-epic" || len(got) != 2 || got[0].ID != "ha-c1" || got[1].ID != "ha-c2" {
		t.Errorf("childrenOrDepsFallback = %+v (deps asked for %q), want ha-c1 and ha-c2 from the deps table", got, asked)
	}
}

// TestBdListChildren_PrimaryPathStillUsed verifies the fallback only kicks in
// when the primary read returns no children.
func TestBdListChildren_PrimaryPathStillUsed(t *testing.T) {
	t.Parallel()
	viaDeps := func(string) ([]bdShowResult, error) {
		t.Fatal("deps fallback consulted although the primary path returned rows")
		return nil, nil
	}
	primary := []bdShowResult{{ID: "ha-direct", Title: "Direct child", Status: "open", IssueType: "task"}}
	got, err := childrenOrDepsFallback(primary, "ha-epic", viaDeps)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "ha-direct" {
		t.Fatalf("expected single ha-direct child from primary path, got %+v", got)
	}
}
