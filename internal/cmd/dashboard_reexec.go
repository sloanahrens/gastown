package cmd

import (
	"context"
	"os"
	"time"
)

const dashboardBinaryPoll = 3 * time.Second

// binaryStamp identifies one build of the executable on disk. make install
// replaces the file, so a new inode, size or mtime means a new binary.
type binaryStamp struct {
	size int64
	mod  time.Time
	ino  uint64
}

func stampBinary(path string) (binaryStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return binaryStamp{}, err
	}
	return binaryStamp{size: fi.Size(), mod: fi.ModTime(), ino: fileInode(fi)}, nil
}

// watchBinary calls changed once, when the file at path has a different stamp
// from the one it started with and has held that new stamp for two polls in a
// row, so a binary still being written is not picked up half-copied. A missing
// file (the instant between rename and create) is not a change.
func watchBinary(ctx context.Context, path string, every time.Duration, changed func()) {
	base, err := stampBinary(path)
	if err != nil {
		return
	}
	var pending *binaryStamp
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cur, err := stampBinary(path)
		if err != nil || cur == base {
			pending = nil
			continue
		}
		if pending != nil && *pending == cur {
			changed()
			return
		}
		pending = &cur
	}
}
