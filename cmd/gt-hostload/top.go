package main

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/hostload"
)

// topTimeout bounds the top read: -l 2 samples twice, a second apart, and a
// busy host stretches that (gt-efoycy).
const topTimeout = 30 * time.Second

// readTop returns the Mac's idle CPU share and its used and compressor memory
// in bytes, from `top -l 2 -n 0 -s 1` through read.
func readTop(read func(timeout time.Duration, name string, args ...string) (string, error)) (idle float64, used, compressor uint64, err error) {
	text, err := read(topTimeout, "top", "-l", "2", "-n", "0", "-s", "1")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("top: %w", err)
	}
	idle, used, compressor, err = hostload.ParseTop(text)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("top: %w", err)
	}
	return idle, used, compressor, nil
}
