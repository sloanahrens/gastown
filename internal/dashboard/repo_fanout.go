package dashboard

import "sync"

// forEachRepo runs fn once per name on a crew of at most forgejoMaxParallel
// goroutines and returns after every call ends. It takes no deadline: fn
// carries its own, so a slow name spends only its own slot.
func forEachRepo(names []string, fn func(i int, name string)) {
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(len(names), forgejoMaxParallel); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				fn(i, names[i])
			}
		}()
	}
	for i := range names {
		work <- i
	}
	close(work)
	wg.Wait()
}
