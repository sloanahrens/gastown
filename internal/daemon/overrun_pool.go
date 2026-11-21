package daemon

import "sync"

// overrunReadPoolSize is how many per-seat reads of one overrun pass run at once: each read
// crosses the sandbox transport, so a few in flight cut the pass time without flooding it.
const overrunReadPoolSize = 4

// overrunReadPool calls read for every i in 0..n-1 with at most limit calls in flight at
// once, and returns when all have returned.
func overrunReadPool(n, limit int, read func(i int)) {
	if limit < 1 {
		limit = 1
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, limit)
	for i := 0; i < n; i++ {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			read(i)
		}()
	}
	wg.Wait()
}
