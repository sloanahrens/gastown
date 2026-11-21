package daemon

import "sync"

// overrunReadPoolSize is how many per-seat reads of one overrun pass run at once: each read
// crosses the sandbox transport, so a few in flight cut the pass time without flooding it.
const overrunReadPoolSize = 4

// overrunReadPool calls read for every i in 0..n-1 with at most limit calls in flight at
// once, and returns when all have returned. A limit below one runs one read at a time; an n
// of zero or less calls nothing. Reads start in index order and finish in any order. The pool
// returns nothing: a caller keeps each read's result in a slot of its own, by index. The pool
// adds no deadline, so a read that never returns holds its caller (gt-jhthh0 bounds a read).
// A read that panics is recovered in its own goroutine; once every read has returned, the
// first recovered panic is raised again in the caller, so the caller's recover sees it.
func overrunReadPool(n, limit int, read func(i int)) {
	if limit < 1 {
		limit = 1
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var panicked bool
	var first any
	slots := make(chan struct{}, limit)
	for i := 0; i < n; i++ {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					if !panicked {
						panicked, first = true, r
					}
					mu.Unlock()
				}
			}()
			read(i)
		}()
	}
	wg.Wait()
	if panicked {
		panic(first)
	}
}
