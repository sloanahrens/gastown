// Package patrolstate holds the field-preserving, atomic "zero a counter in
// an agent-managed JSON state file" primitive shared by the witness and
// deacon patrol-counter fixes (gt-oabl, gt-wdv9). Both roles hand-maintain a
// state.json that a Go process never otherwise writes, so a reset must never
// clobber fields it doesn't know about, and must never leave a reader with a
// half-written file. This package exists purely so that logic doesn't have
// to be copied a second time — internal/witness and internal/deacon each own
// their own state-file *paths* and call in here for the shared read/zero/
// write mechanics, which keeps this package a leaf with no risk of an import
// cycle between them.
package patrolstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ResetCounter zeroes the integer field named key in the JSON state file at
// path, preserving every other field verbatim and the file's own mode,
// atomically. Returns whether the file was rewritten.
//
// A missing file is a no-op. An unreadable or unparseable file, or a
// non-numeric value at key, is an error and the file is left untouched — a
// state file we cannot parse is not one to overwrite.
func ResetCounter(path, key string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", path, err)
	}

	// Raw messages, not a struct: this file belongs to the agent that writes
	// it and carries fields Go knows nothing about, which must survive.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}

	raw, ok := fields[key]
	if !ok {
		// No counter to reset — nothing to do, and adding one would be a
		// field the agent never asked for.
		return false, nil
	}

	var current int
	if err := json.Unmarshal(raw, &current); err != nil {
		return false, fmt.Errorf("parsing %s in %s: %w", key, path, err)
	}
	if current == 0 {
		return false, nil // already reset; do not touch the file
	}

	fields[key] = json.RawMessage("0")
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encoding %s: %w", path, err)
	}
	out = append(out, '\n')

	// Preserve the file mode the owning agent gave it.
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	// Write-then-rename: the owning agent may be writing this file
	// concurrently, and a reader must never see a half-written state file.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return false, fmt.Errorf("setting mode on %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("replacing %s: %w", path, err)
	}

	return true, nil
}
