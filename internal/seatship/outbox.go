package seatship

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/outbox"
)

// OutboxFrom returns the outbox files present under dir, keyed by name: a file
// larger than capBytes is an error naming it, an absent file is absent from the
// map, an absent directory is an empty map, and a negative cap is an error.
func OutboxFrom(dir string, capBytes int64) (map[string]string, error) {
	if capBytes < 0 {
		return nil, fmt.Errorf("seatship: outbox cap %d is negative", capBytes)
	}
	files := make(map[string]string, 2)
	for _, name := range []string{outbox.ResultFile, outbox.NotesFile} {
		data, err := readOutboxFile(filepath.Join(dir, name), capBytes)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		files[name] = string(data)
	}
	return files, nil
}

// readOutboxFile returns path's content when it is a regular file no larger
// than cap. The stat is an Lstat, so a symlink, FIFO, socket or device is
// refused before it is opened, since a read-only open of a FIFO with no writer
// blocks, and the size check before the read bounds it.
func readOutboxFile(path string, cap int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("seatship: %s is not a regular file", path)
	}
	if info.Size() > cap {
		return nil, fmt.Errorf("seatship: %s is larger than the %d-byte cap", path, cap)
	}
	return os.ReadFile(path)
}
