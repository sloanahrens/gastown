package specdispatch

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestInterfacesParseDefinesAndUses(t *testing.T) {
	t.Parallel()
	description := "## Goal\nA thing exists.\n\n## Interfaces\n" +
		"- Defines: Store, `Store.Mutate`, ErrNoState\n" +
		"Uses: Clock, Store\n"
	block, present, err := ParseInterfaces(description)
	if err != nil {
		t.Fatalf("ParseInterfaces: %v", err)
	}
	if !present {
		t.Fatal("present = false, want true")
	}
	if got, want := block.Defines, []string{"Store", "Store.Mutate", "ErrNoState"}; !slices.Equal(got, want) {
		t.Errorf("Defines = %v, want %v", got, want)
	}
	if got, want := block.Uses, []string{"Clock", "Store"}; !slices.Equal(got, want) {
		t.Errorf("Uses = %v, want %v", got, want)
	}
}

func TestInterfacesParseRejectsBadLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		lines string
		line  int
		text  string
	}{
		{
			name:  "unknown key",
			lines: "Defines: Store\nProvides: X\n",
			line:  2,
			text:  "Provides: X",
		},
		{
			name:  "no key",
			lines: "Defines: Store\njust text\n",
			line:  2,
			text:  "just text",
		},
		{
			name:  "lower-case key",
			lines: "defines: Store\n",
			line:  1,
			text:  "defines: Store",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, present, err := ParseInterfaces("## Interfaces\n" + tt.lines)
			if err == nil {
				t.Fatal("ParseInterfaces succeeded, want an error")
			}
			if !present {
				t.Error("present = false, want true: the section exists even when a line is refused")
			}
			if want := fmt.Sprintf("line %d", tt.line); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name line %d", err, tt.line)
			}
			if !strings.Contains(err.Error(), tt.text) {
				t.Errorf("error %q does not name the line text %q", err, tt.text)
			}
		})
	}
}

func TestInterfacesParseRejectsNonIdentifier(t *testing.T) {
	t.Parallel()
	for _, item := range []string{"3rd", "a b"} {
		t.Run(item, func(t *testing.T) {
			_, present, err := ParseInterfaces("## Interfaces\nDefines: " + item + "\n")
			if err == nil {
				t.Fatal("ParseInterfaces succeeded, want an error")
			}
			if !present {
				t.Error("present = false, want true")
			}
			if !strings.Contains(err.Error(), item) {
				t.Errorf("error %q does not name the item %q", err, item)
			}
		})
	}
	t.Run("dotted name accepted", func(t *testing.T) {
		block, _, err := ParseInterfaces("## Interfaces\nDefines: Store.Mutate\n")
		if err != nil {
			t.Fatalf("ParseInterfaces: %v", err)
		}
		if got, want := block.Defines, []string{"Store.Mutate"}; !slices.Equal(got, want) {
			t.Errorf("Defines = %v, want %v", got, want)
		}
	})
}

func TestInterfacesParseRefusesEmptyItem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		lines string
		line  int
	}{
		{name: "bare key", lines: "Defines:\n", line: 1},
		{name: "trailing comma", lines: "Defines: Store,\n", line: 1},
		{name: "doubled comma", lines: "Uses: Clock,,Store\n", line: 1},
		{name: "empty item on a later line", lines: "Defines: Store\nUses: \n", line: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, present, err := ParseInterfaces("## Interfaces\n" + tt.lines)
			if err == nil {
				t.Fatal("ParseInterfaces succeeded, want an error for the empty item")
			}
			if !present {
				t.Error("present = false, want true")
			}
			if want := fmt.Sprintf("line %d", tt.line); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name line %d", err, tt.line)
			}
			if !strings.Contains(err.Error(), `"" is not an identifier`) {
				t.Errorf("error %q does not refuse the empty item", err)
			}
		})
	}
}

func TestInterfacesParseAbsentSection(t *testing.T) {
	t.Parallel()
	block, present, err := ParseInterfaces("## Goal\nA thing exists.\n")
	if err != nil {
		t.Fatalf("ParseInterfaces: %v", err)
	}
	if present {
		t.Error("present = true, want false: the description has no Interfaces section")
	}
	if len(block.Defines) != 0 || len(block.Uses) != 0 {
		t.Errorf("block = %+v, want no names", block)
	}
}

func TestInterfacesParseMergesRepeatedLines(t *testing.T) {
	t.Parallel()
	block, present, err := ParseInterfaces("## Interfaces\nDefines: Store, Clock\n\nDefines: Engine, Store\nUses: Clock\n")
	if err != nil {
		t.Fatalf("ParseInterfaces: %v", err)
	}
	if !present {
		t.Fatal("present = false, want true")
	}
	if got, want := block.Defines, []string{"Store", "Clock", "Engine"}; !slices.Equal(got, want) {
		t.Errorf("Defines = %v, want %v", got, want)
	}
	if got, want := block.Uses, []string{"Clock"}; !slices.Equal(got, want) {
		t.Errorf("Uses = %v, want %v", got, want)
	}
}

// wantFinding fails unless f has the kind and bead given and its Detail names
// every string in names.
func wantFinding(t *testing.T, f GraphFinding, kind, bead string, names ...string) {
	t.Helper()
	if f.Kind != kind {
		t.Errorf("Kind = %q, want %q", f.Kind, kind)
	}
	if f.Bead != bead {
		t.Errorf("Bead = %q, want %q", f.Bead, bead)
	}
	for _, name := range names {
		if !strings.Contains(f.Detail, name) {
			t.Errorf("Detail %q does not name %q", f.Detail, name)
		}
	}
}

func TestCheckGraphDuplicateDefine(t *testing.T) {
	t.Parallel()
	got := CheckGraph([]GraphBead{
		{ID: "gt-a", Defines: []string{"Store"}},
		{ID: "gt-b", Defines: []string{"Store", "Clock"}},
	})
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want exactly one duplicate-define", got)
	}
	wantFinding(t, got[0], KindDuplicateDefine, "gt-a", "Store", "gt-a", "gt-b")
}

func TestCheckGraphUnmetUse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		beads []GraphBead
		want  bool
		bead  string // the using bead, when want
		used  string // the unmet name, when want
	}{
		{
			name: "no dependency defines the name",
			beads: []GraphBead{
				{ID: "gt-a", Defines: []string{"Clock"}},
				{ID: "gt-b", Uses: []string{"Clock"}},
			},
			want: true,
			bead: "gt-b",
			used: "Clock",
		},
		{
			name: "direct dependency defines the name",
			beads: []GraphBead{
				{ID: "gt-a", Defines: []string{"Clock"}},
				{ID: "gt-b", DependsOn: []string{"gt-a"}, Uses: []string{"Clock"}},
			},
			want: false,
		},
		{
			name: "transitive dependency defines the name",
			beads: []GraphBead{
				{ID: "gt-base", Defines: []string{"Clock"}},
				{ID: "gt-mid", DependsOn: []string{"gt-base"}},
				{ID: "gt-top", DependsOn: []string{"gt-mid"}, Uses: []string{"Clock"}},
			},
			want: false,
		},
		{
			name: "a bead's own define does not satisfy its use",
			beads: []GraphBead{
				{ID: "gt-a", Defines: []string{"Store"}, Uses: []string{"Store"}},
			},
			want: true,
			bead: "gt-a",
			used: "Store",
		},
		{
			name: "a bead's own define does not satisfy its use through a cycle",
			beads: []GraphBead{
				{ID: "gt-a", DependsOn: []string{"gt-b"}, Defines: []string{"Store"}, Uses: []string{"Store"}},
				{ID: "gt-b", DependsOn: []string{"gt-a"}},
			},
			want: true,
			bead: "gt-a",
			used: "Store",
		},
		{
			name: "unknown dependency id is ignored",
			beads: []GraphBead{
				{ID: "gt-a", DependsOn: []string{"gt-gone"}, Uses: []string{"Clock"}},
			},
			want: true,
			bead: "gt-a",
			used: "Clock",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CheckGraph(tt.beads)
			if !tt.want {
				if len(got) != 0 {
					t.Fatalf("findings = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("findings = %+v, want exactly one unmet-use", got)
			}
			wantFinding(t, got[0], KindUnmetUse, tt.bead, tt.bead, tt.used)
		})
	}
}

func TestCheckGraphSharedModify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		beads []GraphBead
		want  bool
	}{
		{
			name: "independent beads modify one file",
			beads: []GraphBead{
				{ID: "gt-a", Modify: []string{"internal/x.go"}},
				{ID: "gt-b", Modify: []string{"internal/x.go"}},
			},
			want: true,
		},
		{
			name: "one bead depends on the other",
			beads: []GraphBead{
				{ID: "gt-a", Modify: []string{"internal/x.go"}},
				{ID: "gt-b", DependsOn: []string{"gt-a"}, Modify: []string{"internal/x.go"}},
			},
			want: false,
		},
		{
			name: "one bead depends on the other transitively",
			beads: []GraphBead{
				{ID: "gt-base", Modify: []string{"internal/x.go"}},
				{ID: "gt-mid", DependsOn: []string{"gt-base"}},
				{ID: "gt-top", DependsOn: []string{"gt-mid"}, Modify: []string{"internal/x.go"}},
			},
			want: false,
		},
		{
			name: "different files",
			beads: []GraphBead{
				{ID: "gt-a", Modify: []string{"internal/x.go"}},
				{ID: "gt-b", Modify: []string{"internal/y.go"}},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CheckGraph(tt.beads)
			if !tt.want {
				if len(got) != 0 {
					t.Fatalf("findings = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("findings = %+v, want exactly one shared-modify", got)
			}
			wantFinding(t, got[0], KindSharedModify, "gt-a", "gt-a", "gt-b", "internal/x.go")
		})
	}
}

func TestCheckGraphDuplicateTestIsAdvisory(t *testing.T) {
	t.Parallel()
	got := CheckGraph([]GraphBead{
		{ID: "gt-a", TestNames: []string{"TestFoo"}},
		{ID: "gt-b", TestNames: []string{"TestFoo", "TestBar"}},
	})
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want exactly one duplicate-test", got)
	}
	wantFinding(t, got[0], KindDuplicateTest, "gt-a", "TestFoo", "gt-a", "gt-b")
}

func TestCheckGraphCycleTerminatesAndIsStable(t *testing.T) {
	t.Parallel()
	beads := []GraphBead{
		{ID: "gt-a", DependsOn: []string{"gt-c"}, Defines: []string{"Alpha"}, TestNames: []string{"TestSame"}},
		{ID: "gt-b", DependsOn: []string{"gt-a"}, Defines: []string{"Alpha"}, TestNames: []string{"TestSame"}},
		{ID: "gt-c", DependsOn: []string{"gt-b"}, Uses: []string{"Beta"}},
	}
	first := CheckGraph(beads)
	second := CheckGraph(beads)
	if !slices.Equal(first, second) {
		t.Fatalf("two runs over the same input differ:\n%+v\n%+v", first, second)
	}
	var kinds []string
	for _, f := range first {
		kinds = append(kinds, f.Kind)
	}
	if want := []string{KindDuplicateDefine, KindUnmetUse, KindDuplicateTest}; !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v (findings %+v)", kinds, want, first)
	}
}
