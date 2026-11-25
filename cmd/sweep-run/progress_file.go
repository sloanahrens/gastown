package main

import (
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// progressFileName is the file writeProgress writes the Progress record to.
// The daemon and the dashboard read it through the shared volume, so it is
// written world-readable.
const progressFileName = "progress.json"

// progressFileMode is the mode of progress.json. os.CreateTemp makes the temp
// file 0600; atomicfile.WriteFile sets this mode before the rename publishes it.
const progressFileMode = 0o644

// writeProgress writes p's JSON to dir/progress.json. The bytes go to a temp
// file in dir and are renamed over the target, so a reader never sees a
// half-written record and no temp file is left behind on success.
func writeProgress(dir string, p Progress) error {
	path := filepath.Join(dir, progressFileName)
	data, err := p.JSON()
	if err != nil {
		return fmt.Errorf("sweep-run: encode progress %s: %w", path, err)
	}
	if err := atomicfile.WriteFile(path, data, progressFileMode); err != nil {
		return fmt.Errorf("sweep-run: write progress %s: %w", path, err)
	}
	return nil
}
