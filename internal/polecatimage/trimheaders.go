package polecatimage

import (
	"fmt"
	"io"
	"sync"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// readBuildOutputs sets the Output of every build-cache entry in entries, reading headers through
// open from workers goroutines: one read at a time overran the trim Job's deadline on a large claim
// (gt-fp66p0). Entries keep the serial read's order and a module entry is never opened; a failed
// read names its file, admits no new read, and leaves no worker running when the call returns.
func readBuildOutputs(entries []cachevol.Entry, open func(entryPath string) (io.ReadCloser, error), workers int) error {
	if workers < 1 {
		return fmt.Errorf("reading build-cache headers needs at least one worker, %d given", workers)
	}
	var mu sync.Mutex
	var next int
	var firstErr error
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if firstErr != nil || next >= len(entries) {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				if entries[i].Cache != cachevol.BuildCache {
					continue
				}
				out, err := cachevol.BuildActionOutputFrom(entries[i].Path, func() (io.ReadCloser, error) {
					return open(entries[i].Path)
				})
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("reading the build-cache header of %s: %w", entries[i].Path, err)
					}
					mu.Unlock()
					return
				}
				entries[i].Output = out
			}
		}()
	}
	wg.Wait()
	return firstErr
}
