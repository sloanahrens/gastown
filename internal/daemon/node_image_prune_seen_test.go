package daemon

import (
	"strconv"
	"sync"
	"testing"
)

// On the zero value, a remembered id is seen, an unseen id is not, and the
// empty id is never remembered and never seen.
func TestNodePruneSeenRemembersIDs(t *testing.T) {
	t.Parallel()

	var seen nodePruneSeen
	seen.Remember("run-1")
	if !seen.Seen("run-1") || seen.Seen("run-2") {
		t.Fatal("after Remember(run-1), want run-1 seen and run-2 unseen")
	}
	seen.Remember("")
	if seen.Seen("") {
		t.Fatal(`after Remember(""), Seen("") = true, want false`)
	}
}

// Remember and Seen run from many goroutines at once, and each remembered id
// is seen once its Remember has returned.
func TestNodePruneSeenConcurrentUse(t *testing.T) {
	t.Parallel()

	var seen nodePruneSeen
	var wg sync.WaitGroup
	for i := range 16 {
		id := "run-" + strconv.Itoa(i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			seen.Remember(id)
		}()
		go func() {
			defer wg.Done()
			seen.Seen(id)
		}()
	}
	wg.Wait()

	for i := range 16 {
		if id := "run-" + strconv.Itoa(i); !seen.Seen(id) {
			t.Errorf("Seen(%q) = false after its Remember returned", id)
		}
	}
}
