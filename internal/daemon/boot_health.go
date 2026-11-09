package daemon

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// bootProbe is one named check that a piece of infrastructure is answering.
// Check returns nil once the thing answers. An error, a panic, or a run that
// outlasts the health's timeout all count as not answering.
type bootProbe struct {
	Name  string
	Check func(ctx context.Context) error
}

// bootHealth says which probes are not answering yet. It latches: after one
// call in which every probe answered, Unhealthy returns "" for the life of the
// value and runs no probe again, so an outage after boot is not treated as
// boot. The probe set is the caller's, and an empty set is healthy at once. A
// pod has no container engine, so its caller builds the set without a Docker
// probe.
type bootHealth struct {
	probes  []bootProbe
	timeout time.Duration

	mu      sync.Mutex
	healthy bool
}

// newBootHealth returns a bootHealth over probes, each given timeout to answer.
// The timeout must be positive: the caller sets it, and a zero one would race
// each probe against the timer.
func newBootHealth(probes []bootProbe, timeout time.Duration) *bootHealth {
	return &bootHealth{probes: probes, timeout: timeout}
}

// Unhealthy returns "" once every probe has answered. Otherwise it returns
// "waiting for " followed by the names of the probes that have not, sorted and
// comma-joined. The probes run in parallel, so one call waits at most one
// timeout. The lock is held across the probes, so concurrent callers see one
// sequence: waiting answers, then "" for good, never "" followed by a waiting
// answer.
func (b *bootHealth) Unhealthy(ctx context.Context) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.healthy {
		return ""
	}
	answering := make([]bool, len(b.probes))
	var wg sync.WaitGroup
	for i, p := range b.probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answering[i] = b.answers(ctx, p)
		}()
	}
	wg.Wait()
	var waiting []string
	for i, p := range b.probes {
		if !answering[i] {
			waiting = append(waiting, p.Name)
		}
	}
	if len(waiting) == 0 {
		b.healthy = true
		return ""
	}
	slices.Sort(waiting)
	return "waiting for " + strings.Join(waiting, ", ")
}

// answers runs one probe and reports whether it answered within the timeout.
// The probe runs in its own goroutine, so a probe that never returns holds only
// that goroutine. The channel is buffered, so the goroutine exits when the probe
// does, even after the caller has stopped waiting. The probe gets ctx, and one
// that honors it returns early; the timeout bounds the rest.
func (b *bootHealth) answers(ctx context.Context, p bootProbe) bool {
	done := make(chan bool, 1)
	go func() {
		answered := false
		defer func() {
			// A panic leaves answered false: a probe that panics is not answering.
			if recover() != nil {
				answered = false
			}
			done <- answered
		}()
		answered = p.Check(ctx) == nil
	}()
	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case ok := <-done:
		return ok
	case <-timer.C:
		return false
	}
}
