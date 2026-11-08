package cloudcheck

import "github.com/steveyegge/gastown/internal/atomicfile"

// Write writes r to path atomically: a temporary file in the same directory is
// renamed over path, so a reader sees the previous status or this one and never
// a partial file. The directory must exist.
func Write(path string, r Report) error {
	return atomicfile.WriteJSON(path, r)
}
