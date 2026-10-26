package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// polecatPromptRetention is how long a stored polecat prompt is kept after it
// was last written. A file is kept while its modification time is within this
// window, so the prompt a landing's author had is still on disk when rejections
// are read (gt-js6di; Sloan 2026-10-26). Prompts are kept by age, not by a
// reference count from bead comments, which is not cheap to compute.
const polecatPromptRetention = 30 * 24 * time.Hour

// polecatPromptDir is the store under the town root, one file per prompt.
const polecatPromptDir = ".runtime/polecat-prompts"

// storePolecatPrompt writes data once under its sha256 name in the town's prompt
// store and returns the hex digest and whether this call wrote the file. A file
// with that name already present is left as it is. The write is atomic: a
// temporary file in the same directory is synced, then renamed into place, so a
// reader never sees a partial prompt.
func storePolecatPrompt(townRoot string, data []byte) (string, bool, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:])
	dir := filepath.Join(townRoot, polecatPromptDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return name, false, err
	}
	final := filepath.Join(dir, name+".md")
	if _, err := os.Stat(final); err == nil {
		return name, false, nil
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+name+"-*")
	if err != nil {
		return name, false, err
	}
	// After a successful rename the temporary name no longer exists; the removal
	// only cleans up when a step below fails.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return name, false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return name, false, err
	}
	if err := tmp.Close(); err != nil {
		return name, false, err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return name, false, err
	}
	return name, true, nil
}

// prunePolecatPrompts deletes stored prompts whose modification time is older
// than polecatPromptRetention before now. The file named keep is never deleted,
// so the prompt just written survives its own prune. A store that does not exist
// yet has nothing to prune.
func prunePolecatPrompts(townRoot string, now time.Time, keep string) error {
	dir := filepath.Join(townRoot, polecatPromptDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	cutoff := now.Add(-polecatPromptRetention)
	for _, e := range entries {
		if e.IsDir() || e.Name() == keep+".md" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
