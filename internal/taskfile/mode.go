package taskfile

import (
	"errors"
	"fmt"
	"io/fs"
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
// mode: root holds ModeFile reading ModeFiles. Every other state — no marker,
// an unreadable one, another value — answers false, so a caller that opens the
// bead store on false reads an unreadable marker as dolt mode. A caller whose
// next step the answer chooses wants ReadFileMode, which refuses to guess
// (gt-w1mzol).
func FileMode(root string) bool {
	files, err := ReadFileMode(root)
	return err == nil && files
}

// ReadFileMode is FileMode for a caller whose next step is chosen by the
// answer. A marker that is not there is dolt mode, as FileMode says; a marker
// that is there but cannot be read is an error, because such a seat cannot
// tell whether it has a bead route, and opening the store is a door it may not
// have (gt-w1mzol).
func ReadFileMode(root string) (bool, error) {
	marker := filepath.Join(root, ModeFile)
	data, err := os.ReadFile(marker)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the bead-access marker %s: %w", marker, err)
	}
	return strings.TrimSpace(string(data)) == ModeFiles, nil
}
