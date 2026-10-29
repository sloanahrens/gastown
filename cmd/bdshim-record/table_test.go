package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestGoldenNameIsStableAndPathSafe: a golden's file stem comes from the verb and flags alone, lower
// case, with no path separator or space, so it is a single safe file name.
func TestGoldenNameIsStableAndPathSafe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		row  Row
		want string
	}{
		{"verb and one flag", Row{Verb: "show", Flags: []string{"--json"}}, "show-json"},
		{"flag value is kept", Row{Verb: "list", Flags: []string{"--status", "open", "--json"}}, "list-status-open-json"},
		{"two-word verb", Row{Verb: "dep add"}, "dep-add"},
		{"no flags", Row{Verb: "ready"}, "ready"},
		{"underscore and colon are dashes", Row{Verb: "list", Flags: []string{"--label", "gt:agent", "--status", "in_progress"}}, "list-label-gt-agent-status-in-progress"},
		{"path separators and spaces are dashes", Row{Verb: "create", Flags: []string{"--title", "a/b c\\d"}}, "create-title-a-b-c-d"},
		{"repeated separators collapse", Row{Verb: "count", Flags: []string{"--by--status"}}, "count-by-status"},
		{"leading and trailing separators drop", Row{Verb: "count", Flags: []string{"---x---"}}, "count-x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := GoldenName(tc.row)
			if got != tc.want {
				t.Errorf("GoldenName(%+v) = %q, want %q", tc.row, got, tc.want)
			}
			if strings.ContainsAny(got, "/\\ ") || got == "" {
				t.Errorf("GoldenName(%+v) = %q, want a non-empty name with no separator or space", tc.row, got)
			}
		})
	}
}

// TestGoldenNameIgnoresOperands: two rows that differ only in an operand share a name, which is why
// Capture refuses a table that repeats one.
func TestGoldenNameIgnoresOperands(t *testing.T) {
	t.Parallel()
	a := Row{Verb: "show", Flags: []string{"--json"}, Operands: []string{"fx-1"}}
	b := Row{Verb: "show", Flags: []string{"--json"}, Operands: []string{"fx-2"}}
	if GoldenName(a) != GoldenName(b) {
		t.Errorf("GoldenName differs by operand: %q vs %q", GoldenName(a), GoldenName(b))
	}
}

// TestRowArgsPutsVerbBeforeFlagsBeforeOperands: bd takes the verb words first, then the flags, then
// the operands.
func TestRowArgsPutsVerbBeforeFlagsBeforeOperands(t *testing.T) {
	t.Parallel()
	row := Row{Verb: "dep add", Flags: []string{"--json"}, Operands: []string{"fx-1", "fx-2"}}
	want := []string{"dep", "add", "--json", "fx-1", "fx-2"}
	if got := row.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("Args() = %v, want %v", got, want)
	}
}

// TestTheTableNamesEveryFixtureBead: the beads the preflight asks bd for are the ones the rows use, so
// the fixture a recording is made from is checked against the table that names it.
func TestTheTableNamesEveryFixtureBead(t *testing.T) {
	t.Parallel()
	for _, id := range fixtureIDs {
		named := false
		for _, row := range Table() {
			for _, arg := range row.Args() {
				if arg == id {
					named = true
				}
			}
		}
		if !named {
			t.Errorf("fixture bead %s is named by no row, so checking for it proves nothing", id)
		}
	}
}

// TestTableGoldenNamesAreUniqueAndPathSafe: the shipped table writes distinct goldens, each with a
// safe name, so a recording run never overwrites one row with another.
func TestTableGoldenNamesAreUniqueAndPathSafe(t *testing.T) {
	t.Parallel()
	rows := Table()
	if err := checkUnique(rows); err != nil {
		t.Fatalf("the shipped table repeats a golden: %v", err)
	}
	for _, row := range rows {
		if name := GoldenName(row); name == "" || strings.ContainsAny(name, "/\\ ") {
			t.Errorf("row %v has golden name %q, want a safe non-empty name", row.Args(), name)
		}
	}
}
