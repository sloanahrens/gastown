package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

const formulasDir, formulaSuffix = ".beads/formulas", ".formula.toml"

// skeletonFormulas returns a scrubbed entry for each regular <name>.formula.toml
// in .beads/formulas, skipping symlinks and subdirectories as addTree does.
// An absent directory holds none; an unreadable or non-directory path is an error.
func skeletonFormulas(town townReader) ([]skeletonEntry, error) {
	info, err := town.root.Lstat(filepath.FromSlash(formulasDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("sandbox: read %s: %w", formulasDir, err)
	case !info.IsDir():
		return nil, fmt.Errorf("sandbox: %s is not a directory", formulasDir)
	}
	listed, err := fs.ReadDir(town.root.FS(), formulasDir)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read %s: %w", formulasDir, err)
	}
	var entries []skeletonEntry
	for _, d := range listed {
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), formulaSuffix) {
			continue
		}
		data, err := readFormula(town, path.Join(formulasDir, d.Name()))
		if err != nil {
			return nil, err
		}
		entries = append(entries, skeletonEntry{name: path.Join(skeletonRoot, formulasDir, d.Name()), data: data})
	}
	return entries, nil
}

func readFormula(town townReader, rel string) ([]byte, error) {
	raw, err := town.read(rel)
	if err != nil {
		return nil, err
	}
	return sanitizeFile(rel, raw)
}
