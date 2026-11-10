package presubmit

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
)

const (
	// PodEngineDoc is the document whose inventory pins file lines.
	PodEngineDoc = "docs/design/pod-engine.md"
	// PodEngineDir is the package whose inventory test fails when a pinned line moves.
	PodEngineDir = "internal/podengine"
	// podEngineSection starts the inventory table; its File column is the pinned set.
	podEngineSection = "## The inventory"
	// podEngineCells is the cell count of an inventory row (File through Evidence).
	podEngineCells = 7
	// PodEngineRecovery is the one-line recovery printed when the pins moved.
	PodEngineRecovery = "presubmit: a file docs/design/pod-engine.md pins moved; if ./internal/podengine failed, run `go test ./internal/podengine -update-lines`, then renumber the prose line refs by hand, then re-run"
)

// PinnedFiles returns the repo-relative files the inventory table in doc pins:
// the File column of each row under the inventory heading, sorted and without
// duplicates. It fails on a row that is not podEngineCells wide and on a
// document with no inventory rows, so a parse that found nothing never reads
// as a set with nothing pinned.
func PinnedFiles(doc string) ([]string, error) {
	seen := map[string]bool{}
	section := ""
	for i, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			section = trimmed
			continue
		}
		if !strings.HasPrefix(section, podEngineSection) || !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := inventoryCells(trimmed)
		if cells[0] == "File" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		if len(cells) != podEngineCells {
			return nil, fmt.Errorf("%s:%d: the row has %d cells, want %d", PodEngineDoc, i+1, len(cells), podEngineCells)
		}
		if cells[0] == "" {
			return nil, fmt.Errorf("%s:%d: the row names no file", PodEngineDoc, i+1)
		}
		seen[path.Clean(cells[0])] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("%s: no rows under %q", PodEngineDoc, podEngineSection)
	}
	pinned := make([]string, 0, len(seen))
	for f := range seen {
		pinned = append(pinned, f)
	}
	slices.Sort(pinned)
	return pinned, nil
}

// inventoryCells splits a markdown table row on its unescaped pipes and strips
// the backticks around each cell.
func inventoryCells(line string) []string {
	const escaped = "\x00"
	body := strings.ReplaceAll(strings.Trim(line, "|"), `\|`, escaped)
	parts := strings.Split(body, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.Trim(strings.TrimSpace(strings.ReplaceAll(p, escaped, "|")), "`"))
	}
	return cells
}

// PodEngineTouched reports whether the change nameStatus (git diff
// --name-status -z output) touches a file the inventory in doc pins. A rename
// counts on either side, and a deleted pinned file counts too.
func PodEngineTouched(nameStatus, doc string) (bool, error) {
	pinned, err := PinnedFiles(doc)
	if err != nil {
		return false, err
	}
	changed, err := land.ChangedPaths(nameStatus)
	if err != nil {
		return false, err
	}
	for _, p := range changed {
		if slices.Contains(pinned, p) {
			return true, nil
		}
	}
	return false, nil
}

// PodEngineDirs returns dirs with PodEngineDir added when the change touches a
// pinned file, sorted and without duplicates. dirs itself is not modified.
func PodEngineDirs(dirs []string, nameStatus, doc string) ([]string, error) {
	touched, err := PodEngineTouched(nameStatus, doc)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(dirs)
	if touched && !slices.Contains(out, PodEngineDir) {
		out = append(out, PodEngineDir)
	}
	slices.Sort(out)
	return out, nil
}

// RecoveryLine returns PodEngineRecovery when the change touches a pinned file,
// and "" otherwise. The line holds no newline, so a caller that prints it once
// on a failure prints one line.
func RecoveryLine(nameStatus, doc string) (string, error) {
	touched, err := PodEngineTouched(nameStatus, doc)
	if err != nil || !touched {
		return "", err
	}
	return PodEngineRecovery, nil
}
