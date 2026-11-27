package dashboard

import (
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The helper calls fn once per name, at that name's index, and returns after
// every call has ended.
func TestRepoFanoutCallsEveryNameOnceWithItsIndex(t *testing.T) {
	t.Parallel()
	names := []string{"owner/a", "owner/b", "owner/c", "owner/d", "owner/e"}
	got := make([]string, len(names))
	ended := make(chan struct{}, len(names))
	forEachRepo(names, func(i int, name string) {
		got[i] = name
		ended <- struct{}{}
	})
	assert.Len(t, ended, len(names), "forEachRepo returns after every call ends")
	assert.Equal(t, names, got, "every name arrives at its own index")
}

// More names than the crew: the whole crew runs at once and no more. Each call
// parks until the test releases it, so the calls that entered are the calls
// that ran at once; the bounded yield lets a call past the crew start before
// the count is read.
func TestRepoFanoutRunsAtMostTheCrewAtOnce(t *testing.T) {
	t.Parallel()
	names := make([]string, forgejoMaxParallel+4)
	var entered atomic.Int32
	started := make(chan struct{}, len(names))
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		forEachRepo(names, func(int, string) {
			entered.Add(1)
			started <- struct{}{}
			holdUntil(release)
		})
	}()
	for n := 0; n < forgejoMaxParallel; n++ {
		wantSignal(t, started, "fewer than the whole crew started at once")
	}
	for n := 0; n < 100; n++ {
		runtime.Gosched()
	}
	assert.Equal(t, int32(forgejoMaxParallel), entered.Load(), "the whole crew runs at once and the rest wait for a slot")
	close(release)
	wantSignal(t, done, "forEachRepo returns once every call is released")
}
