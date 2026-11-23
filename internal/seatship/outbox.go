package seatship

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/steveyegge/gastown/internal/outbox"
)

// OutboxFrom returns the outbox files present under dir, keyed by name: a file
// larger than capBytes is an error naming it, an absent file is absent from the
// map, an absent directory gives an empty map, and a negative cap is an error.
// Each entry is read through an os.Root on dir, the untrusted seat's directory,
// so a FIFO, device or directory is refused and a symlink that leaves dir is
// refused.
func OutboxFrom(dir string, capBytes int64) (map[string]string, error) {
	if capBytes < 0 {
		return nil, fmt.Errorf("seatship: outbox cap %d is negative", capBytes)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("seatship: open outbox %s: %w", dir, err)
	}
	defer root.Close()
	files := make(map[string]string, 2)
	for _, name := range []string{outbox.ResultFile, outbox.NotesFile} {
		data, err := readOutboxFile(root, name, capBytes)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		files[name] = string(data)
	}
	return files, nil
}

// readOutboxFile reads one entry under root, at most cap bytes. It opens the
// entry once with O_NOFOLLOW (a symlink out of the outbox is refused) and
// O_NONBLOCK (a FIFO cannot hold the open), checks the type with fstat on that
// descriptor, and reads that same descriptor through a cap+1-byte LimitReader,
// so nothing is reopened and a file grown after the check cannot pass the cap.
func readOutboxFile(root *os.Root, name string, cap int64) ([]byte, error) {
	var data []byte
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			err = errors.New("is not a regular file")
		} else {
			n := cap + 1 // one past the cap separates at-cap from over-cap
			if n <= 0 {  // cap == math.MaxInt64 wraps; no file exceeds it
				n = cap
			}
			data, err = io.ReadAll(io.LimitReader(f, n))
			if err == nil && int64(len(data)) > cap {
				data, err = nil, fmt.Errorf("is larger than the %d-byte cap", cap)
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("seatship: outbox %s: %w", name, err)
	}
	return data, nil
}
