package specdispatch

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// filesSection is the "## " heading, lower-cased as Sections keys it, whose
// lines say what the bead does with each repo file (design decision 1,
// docs/plans/2026-11-03-bead-lint-v2-design.md).
const filesSection = "files"

// FileVerb is what a bead does with a path it lists under "## Files".
type FileVerb string

const (
	FileCreate FileVerb = "create"
	FileModify FileVerb = "modify"
	FileRead   FileVerb = "read"
)

// FileEntry is one line of the Files block: the verb, the bare path and the
// 1-based line number in the bead description.
type FileEntry struct {
	Verb FileVerb
	Path string
	Line int
}

// FilesBlock is a bead's parsed "## Files" section. The zero value is no
// entries: an empty section, no section, or a section whose parse failed.
type FilesBlock struct {
	Entries []FileEntry
}

// ParseFiles reads a bead description's optional "## Files" section: one
// "verb: path" per line, verbs create, modify and read in lower case. A line
// may carry a leading "- " bullet and the path may sit in backticks; both are
// stripped. Blank lines and deeper headings are ignored. present is false when
// there is no Files section, which is not an error. Any other line is an error
// naming its 1-based line in the description and its text, so a typo is never
// read as a missing path.
func ParseFiles(description string) (FilesBlock, bool, error) {
	body, present := Sections(description)[filesSection]
	if !present {
		return FilesBlock{}, false, nil
	}
	first := filesBodyLine(strings.Split(description, "\n"))
	var block FilesBlock
	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		text := strings.TrimPrefix(line, "- ")
		verbText, pathText, ok := strings.Cut(text, ":")
		verb := FileVerb(strings.TrimSpace(verbText))
		path := unquotePath(strings.TrimSpace(pathText))
		n := first + i
		if !ok || !verb.known() || path == "" {
			return FilesBlock{}, true, fmt.Errorf("files: line %d: %q is not \"verb: path\" (want create, modify or read)", n, line)
		}
		block.Entries = append(block.Entries, FileEntry{Verb: verb, Path: path, Line: n})
	}
	return block, true, nil
}

// filesBodyLine returns the 1-based description line of the first line of the
// Files body that Sections keeps: the first non-blank line after the heading,
// since Sections trims the blank lines above it. Sections keys a section once a
// prefix of the description reaches its heading, and a longer prefix keeps that
// key, so a binary search for the shortest prefix that has the key lands on the
// heading line.
func filesBodyLine(lines []string) int {
	heading := sort.Search(len(lines), func(i int) bool {
		_, ok := Sections(strings.Join(lines[:i+1], "\n"))[filesSection]
		return ok
	})
	for j := heading + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) != "" {
			return j + 1
		}
	}
	return len(lines) + 1
}

// Paths returns the paths listed with verb, in order and without repeats.
func (b FilesBlock) Paths(verb FileVerb) []string {
	var out []string
	for _, e := range b.Entries {
		if e.Verb == verb && !slices.Contains(out, e.Path) {
			out = append(out, e.Path)
		}
	}
	return out
}

// known reports whether v is one of the three lower-case verbs. A line's verb
// is never case-folded: "Create" is a refusal.
func (v FileVerb) known() bool {
	switch v {
	case FileCreate, FileModify, FileRead:
		return true
	}
	return false
}

// unquotePath strips one pair of backticks around a path, as in `internal/x.go`.
func unquotePath(path string) string {
	if len(path) > 1 && path[0] == '`' && path[len(path)-1] == '`' {
		return strings.TrimSpace(path[1 : len(path)-1])
	}
	return path
}
