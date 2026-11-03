package specdispatch

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const filesDescription = `## Goal
A thing exists.

## Files
create: internal/a.go
create: internal/b.go
modify: internal/specdispatch/lint.go
read: docs/guides/spec-dispatcher.md
modify: internal/c.go
`

func TestFilesParseVerbs(t *testing.T) {
	block, present, err := ParseFiles(filesDescription)
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	if !present {
		t.Fatal("present = false, want true")
	}
	want := []FileEntry{
		{Verb: FileCreate, Path: "internal/a.go", Line: 1},
		{Verb: FileCreate, Path: "internal/b.go", Line: 2},
		{Verb: FileModify, Path: "internal/specdispatch/lint.go", Line: 3},
		{Verb: FileRead, Path: "docs/guides/spec-dispatcher.md", Line: 4},
		{Verb: FileModify, Path: "internal/c.go", Line: 5},
	}
	if !slices.Equal(block.Entries, want) {
		t.Errorf("entries = %+v, want %+v", block.Entries, want)
	}
}

func TestFilesParseRefusesBadLine(t *testing.T) {
	tests := []struct {
		name  string
		files string
		line  int
		text  string
	}{
		{
			name:  "upper-case verb",
			files: "create: ok.go\nCreate: x.go\n",
			line:  2,
			text:  "Create: x.go",
		},
		{
			name:  "no verb or colon",
			files: "create: ok.go\njust text\n",
			line:  2,
			text:  "just text",
		},
		{
			name:  "empty path",
			files: "create: ok.go\nmodify:\n",
			line:  2,
			text:  "modify:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, present, err := ParseFiles("## Files\n" + tt.files)
			if err == nil {
				t.Fatal("ParseFiles succeeded, want an error")
			}
			if !present {
				t.Error("present = false, want true: the section exists even when a line is refused")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("line %d", tt.line)) {
				t.Errorf("error %q does not name line %d", err, tt.line)
			}
			if !strings.Contains(err.Error(), tt.text) {
				t.Errorf("error %q does not name the line text %q", err, tt.text)
			}
		})
	}
}

func TestFilesParseAbsentSection(t *testing.T) {
	block, present, err := ParseFiles("## Goal\nA thing exists.\n")
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	if present {
		t.Error("present = true, want false: the description has no Files section")
	}
	if len(block.Entries) != 0 {
		t.Errorf("entries = %+v, want none", block.Entries)
	}
}

func TestFilesParseStripsBulletAndBackticks(t *testing.T) {
	block, present, err := ParseFiles("## Files\n- create: `internal/x.go`\n")
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	if !present {
		t.Fatal("present = false, want true")
	}
	want := []FileEntry{{Verb: FileCreate, Path: "internal/x.go", Line: 1}}
	if !slices.Equal(block.Entries, want) {
		t.Errorf("entries = %+v, want %+v", block.Entries, want)
	}
}

func TestFilesPathsDedupesInOrder(t *testing.T) {
	block, _, err := ParseFiles("## Files\ncreate: a.go\nmodify: m.go\ncreate: b.go\ncreate: a.go\n")
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	if got, want := block.Paths(FileCreate), []string{"a.go", "b.go"}; !slices.Equal(got, want) {
		t.Errorf("Paths(create) = %v, want %v", got, want)
	}
	if got := block.Paths(FileRead); len(got) != 0 {
		t.Errorf("Paths(read) = %v, want none", got)
	}
}

func TestFilesParseIgnoresBlankLines(t *testing.T) {
	block, present, err := ParseFiles("## Goal\nx\n\n## Files\n\ncreate: a.go\n\nmodify: b.go\n\n## Out of scope\n")
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	if !present {
		t.Fatal("present = false, want true")
	}
	want := []FileEntry{
		{Verb: FileCreate, Path: "a.go", Line: 1},
		{Verb: FileModify, Path: "b.go", Line: 3},
	}
	if !slices.Equal(block.Entries, want) {
		t.Errorf("entries = %+v, want %+v: blank lines are ignored but still count as lines", block.Entries, want)
	}
}

func TestFilesParseIgnoresHeadings(t *testing.T) {
	block, _, err := ParseFiles("## Files\ncreate: a.go\n### Example\nmodify: b.go\n")
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	want := []FileEntry{
		{Verb: FileCreate, Path: "a.go", Line: 1},
		{Verb: FileModify, Path: "b.go", Line: 3},
	}
	if !slices.Equal(block.Entries, want) {
		t.Errorf("entries = %+v, want %+v: a deeper heading is structure, not a path line", block.Entries, want)
	}
}

func TestFilesParseEmptySection(t *testing.T) {
	block, present, err := ParseFiles("## Files\n\n## Goal\nx\n")
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	if !present {
		t.Error("present = false, want true: an empty section is still a section")
	}
	if len(block.Entries) != 0 {
		t.Errorf("entries = %+v, want none", block.Entries)
	}
}
