package townflight

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// listEntries returns the paths of the .cmd files in townRoot's in-flight
// directory. A missing directory lists nothing; any other read failure is
// returned, because a listing read as absent would let the drain miss a command.
func listEntries(townRoot string) ([]string, error) {
	dir := inflightDir(townRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing inflight directory: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".cmd") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths, nil
}
