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
