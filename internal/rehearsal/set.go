// Package rehearsal reads the bead set a rehearsal run files.
package rehearsal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// setSize is the number of bead definitions a rehearsal set holds.
const setSize = 16

// Task is one bead definition read from a rehearsal set.
type Task struct {
	// Name is the file name the definition was read from.
	Name string
	// Title is the text after the "# " on the file's first line.
	Title string
	// Body is everything after the title line.
	Body string
}

// LoadSet reads the bead definitions in dir: its *.md files, in name order.
// It refuses a set that is not exactly sixteen files, and a file with no title
// line, an empty body, or no level 2 Acceptance heading. Every error names the
// file or the count.
func LoadSet(dir string) ([]Task, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("rehearsal set %s: %w", dir, err)
	}
	// ReadDir returns entries sorted by file name.
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) != setSize {
		return nil, fmt.Errorf("rehearsal set %s has %d files, want %d", dir, len(names), setSize)
	}
	tasks := make([]Task, 0, len(names))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("rehearsal %s: %w", name, err)
		}
		task, err := parseTask(name, string(raw))
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// parseTask splits one bead definition into its title and body, and refuses it
// when either is missing or the body has no Acceptance heading.
func parseTask(name, raw string) (Task, error) {
	first, rest, _ := strings.Cut(raw, "\n")
	title, ok := strings.CutPrefix(first, "# ")
	if !ok {
		return Task{}, fmt.Errorf("rehearsal %s: first line is not a \"# <title>\" line", name)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return Task{}, fmt.Errorf("rehearsal %s: title is empty", name)
	}
	if strings.TrimSpace(rest) == "" {
		return Task{}, fmt.Errorf("rehearsal %s: body is empty", name)
	}
	if !hasAcceptanceHeading(rest) {
		return Task{}, fmt.Errorf("rehearsal %s: body has no level 2 \"## Acceptance\" heading", name)
	}
	return Task{Name: name, Title: title, Body: rest}, nil
}

// hasAcceptanceHeading reports whether a line of body is exactly "## Acceptance".
func hasAcceptanceHeading(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimRight(line, " \t\r") == "## Acceptance" {
			return true
		}
	}
	return false
}
