package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
)

// nodePrunePanicMax is the most of a recovered panic's text report gets.
const nodePrunePanicMax = 300

// nodePruneFlight keeps the node image prune to one run at a time. Its zero
// value is ready to use.
type nodePruneFlight struct {
	mu      sync.Mutex
	running bool
	// finished closes when the running run ends, so a waiter can tell the
	// flight is free without polling.
	finished chan struct{}
}

// Start launches run in its own goroutine and returns true, or starts nothing
// and returns false at once: when a run is already going, and when parent is
// already done, since that run's context would be canceled on arrival. The
// caller passes the daemon's long-lived context: the run derives from it, so
// it outlives the call and only canceling that context ends it. A panicking run
// is recovered so the daemon survives and counts as a failed run: the mark is
// cleared so the next heartbeat may start another, and report is called once
// with the panic value cut to nodePrunePanicMax bytes on a rune boundary. report
// runs on the run's goroutine and must not block; a panic in it is recovered and
// logged rather than ending the daemon, and a nil report logs the run's panic to
// the standard logger instead.
func (f *nodePruneFlight) Start(parent context.Context, run func(ctx context.Context), report func(panicValue any)) bool {
	if report == nil {
		report = func(panicValue any) { log.Printf("node image prune: recovered panic: %v", panicValue) }
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running || parent.Err() != nil {
		return false
	}
	f.running = true
	f.finished = make(chan struct{})
	finished := f.finished
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer cancel()
		defer close(finished)
		defer func() {
			if r := recover(); r != nil {
				callNodePruneReport(report, nodePrunePanicText(r))
			}
		}()
		defer func() {
			f.mu.Lock()
			f.running = false
			f.mu.Unlock()
		}()
		run(ctx)
	}()
	return true
}

// nodePrunePanicText is r as report text, cut to nodePrunePanicMax on a rune
// boundary.
func nodePrunePanicText(r any) string {
	text := fmt.Sprint(r)
	if len(text) > nodePrunePanicMax {
		text = strings.ToValidUTF8(text[:nodePrunePanicMax], "")
	}
	return text
}

// callNodePruneReport calls report and recovers a panic in it, logging that
// panic instead: report runs while the run's panic unwinds, so nothing above it
// would recover an escaping panic and the daemon would exit (gt-sj17sb).
func callNodePruneReport(report func(any), text string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("node image prune: report panicked: %s", nodePrunePanicText(r))
		}
	}()
	report(text)
}

// done returns a channel closed when the current run ends.
func (f *nodePruneFlight) done() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished
}
