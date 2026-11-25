package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// runProgressName is the record a tier sweep run's runner rewrites after each
// tier, and the poll copies out of the run's Job.
const runProgressName = "progress.json"

// progressTemp is the part of the temp file writeRunProgress writes through.
type progressTemp interface {
	Name() string
	Write(p []byte) (int, error)
	Sync() error
	Close() error
}

// progressFS is the filesystem calls writeRunProgress makes before the rename.
// A test swaps one for a failing call to reach a branch a real file never hits.
type progressFS struct {
	createTemp func(dir, pattern string) (progressTemp, error)
	chmod      func(name string, mode os.FileMode) error
}

// writeRunProgress stores data as progress.json in outDir. data must be a JSON
// object. The bytes go to a temp file in outDir that is renamed over the
// record, so a reader sees the old record or the new one, never a part-written
// file.
func writeRunProgress(outDir string, data []byte) error {
	return writeRunProgressFS(progressFS{
		createTemp: func(dir, pattern string) (progressTemp, error) { return os.CreateTemp(dir, pattern) },
		chmod:      os.Chmod,
	}, outDir, data)
}

// writeRunProgressFS is writeRunProgress over the given filesystem calls.
func writeRunProgressFS(fs progressFS, outDir string, data []byte) error {
	var object map[string]json.RawMessage
	// A JSON null unmarshals to a nil map with no error, so it is refused here.
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return fmt.Errorf("progress for %s is not a JSON object", outDir)
	}
	tmp, err := fs.createTemp(outDir, runProgressName+".tmp.*")
	if err != nil {
		return fmt.Errorf("writing progress to %s: %w", outDir, err)
	}
	tmpName := tmp.Name()
	// The rename moves the temp file away on success; this removes it on any
	// failure before that.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing progress to %s: %w", outDir, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing progress to %s: %w", outDir, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing progress to %s: %w", outDir, err)
	}
	// CreateTemp opens 0600; progress.json is a plain record other processes read.
	if err := fs.chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("writing progress to %s: %w", outDir, err)
	}
	if err := os.Rename(tmpName, filepath.Join(outDir, runProgressName)); err != nil {
		return fmt.Errorf("writing progress to %s: %w", outDir, err)
	}
	return nil
}
