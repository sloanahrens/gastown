package taskfile

import (
	"os"
	"path/filepath"
	"strings"
)

// The files a sandbox seat reads to learn where its task lives: one marker in
// the task directory /sandbox/.gt/task, beside TASK.md (gt-3qkaws).
const (
	// ModeFile is the marker's name.
	ModeFile = "MODE"
	// ModeFiles is the marker's value for a seat that reads its task from
	// TASK.md rather than from its bead in Dolt.
	ModeFiles = "files"
)

// FileMode reports whether the seat whose task directory is root is in file
// mode: root holds ModeFile reading ModeFiles. Any other state — no marker, an
// unreadable one, or another value — is dolt mode, so a seat reads its bead and
// fails loudly where no route to Dolt exists (gt-3qkaws).
func FileMode(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, ModeFile))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == ModeFiles
}
