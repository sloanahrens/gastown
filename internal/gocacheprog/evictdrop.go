package gocacheprog

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
)

// dropMissing deletes from idx each chosen action not in keep whose index file or
// body is absent under dir, at the paths Disk uses. An action idx lacks is skipped,
// and a stat failure other than absence returns its error with idx unchanged.
func dropMissing(dir string, idx Index, chosen, keep [][]byte) error {
	d := &Disk{dir: dir}
	drop := map[string]bool{}
	for _, id := range chosen {
		e, ok := idx[string(id)]
		if !ok || slices.ContainsFunc(keep, func(k []byte) bool { return bytes.Equal(k, id) }) {
			continue
		}
		if !validID(id) || !validID([]byte(e.OutputID)) {
			return fmt.Errorf("gocacheprog: evict drop: action %q or output %q id is not a non-empty hex string", id, e.OutputID)
		}
		for _, p := range []string{d.file(id, "-a"), d.file([]byte(e.OutputID), "-d")} {
			switch _, err := os.Stat(p); {
			case errors.Is(err, os.ErrNotExist):
				drop[string(id)] = true
			case err != nil:
				return fmt.Errorf("gocacheprog: evict drop: stat %s: %w", p, err)
			}
		}
	}
	for k := range drop {
		delete(idx, k)
	}
	return nil
}
