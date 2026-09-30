package git

import (
	"fmt"
	"strings"
)

// StagedChange is one path whose staged content differs from HEAD: Status is
// 'A' (added), 'M' (modified), 'D' (deleted) or 'T' (type changed).
type StagedChange struct {
	Status byte
	Path   string
}

// StagedChanges lists what the index would commit relative to HEAD, one entry
// per path, with rename detection off, so a moved file is a deletion plus an
// addition (git diff --cached --name-status --no-renames -z).
func (g *Git) StagedChanges() ([]StagedChange, error) {
	out, err := g.runOutput("diff", "--cached", "--name-status", "--no-renames", "-z")
	if err != nil {
		return nil, err
	}
	return parseNameStatusZ(out)
}

// parseNameStatusZ reads diff --name-status -z output: a status token, then
// the path, both NUL-terminated.
func parseNameStatusZ(out string) ([]StagedChange, error) {
	fields := strings.Split(out, "\x00")
	var changes []StagedChange
	for i := 0; i+1 < len(fields); i += 2 {
		status := fields[i]
		if status == "" {
			break
		}
		changes = append(changes, StagedChange{Status: status[0], Path: fields[i+1]})
	}
	if len(fields)%2 == 0 && fields[len(fields)-1] != "" {
		return nil, fmt.Errorf("git diff --name-status: unterminated entry in %q", out)
	}
	return changes, nil
}

// WriteTree writes the index as a tree object and returns its id, without
// committing (git write-tree).
func (g *Git) WriteTree() (string, error) {
	return g.run("write-tree")
}
