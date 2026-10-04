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

// watchBinary calls changed once, when stamp reports something different from
// what it reported first and keeps reporting that new value on the next tick,
// so a binary still being written is not picked up half-copied. A stamp error
// (the instant between rename and create) is not a change.
func watchBinary(ctx context.Context, stamp func() (binaryStamp, error), ticks <-chan time.Time, changed func()) {
	base, err := stamp()
	if err != nil {
		return
	}
	var pending *binaryStamp
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		cur, err := stamp()
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

// watchBinaryFile is watchBinary on the executable at path, polled every
// interval.
func watchBinaryFile(ctx context.Context, path string, every time.Duration, changed func()) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	watchBinary(ctx, func() (binaryStamp, error) { return stampBinary(path) }, tick.C, changed)
}
